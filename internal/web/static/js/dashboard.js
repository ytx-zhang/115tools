// dashboard.js —— 任务状态与日志统一走 /api/logs SSE。
// 状态卡用「普通状态对象 + 命令式 render 函数」更新 DOM；日志流走批量渲染队列。
import { api, toast, connectSSE } from './api.js';

const startText = { sync: '开始同步', strm: '开始生成' };

// dash 是普通状态对象（无响应式代理），数据变化后显式调用 renderStatus 刷新 DOM。
export const dash = {
  configReady: true,
  missing: [],
  initError: '',
  sync: { running: false, done: 0, total: 0, ready: false, badgeText: '未就绪', badgeCls: 'badge warn', barWidth: '0%', btnText: '开始同步', btnCls: 'btn primary' },
  strm: { running: false, done: 0, total: 0, ready: false, badgeText: '未就绪', badgeCls: 'badge warn', barWidth: '0%', btnText: '开始生成', btnCls: 'btn primary' },

  setStatus(name, st) {
    const s = dash[name];
    s.running = !!st?.running;
    s.done = st?.completed || 0;
    s.total = st?.total || 0;
    s.ready = !!st;
    s.barWidth = s.total ? `${Math.min(100, s.done / s.total * 100)}%` : '0%';
    if (!st) { s.badgeText = '未就绪'; s.badgeCls = 'badge warn'; }
    else if (s.running) { s.badgeText = '运行中'; s.badgeCls = 'badge run'; }
    else { s.badgeText = '空闲'; s.badgeCls = 'badge'; }
    s.btnText = s.running ? '停 止' : startText[name];
    s.btnCls = s.running ? 'btn danger' : 'btn primary';
  },

  async toggle(name) {
    if (dash[name].running) {
      await api(`/api/task/${name}`, { method: 'DELETE' });
      toast('已发送停止指令');
    } else {
      await api(`/api/task/${name}`, { method: 'POST' });
      toast('任务已启动', 'ok');
    }
  },
};

export function initDashboard() {
  bindTaskButtons();
  renderStatus();
  initLogs();
}

export function stopDashboard() {
  stopLogs();
}

// ──── 状态渲染（banner + 任务卡）────

function renderStatus() {
  renderBanners();
  renderTaskCard('sync');
  renderTaskCard('strm');
}

function renderBanners() {
  const cb = document.getElementById('config-banner');
  if (cb) {
    cb.hidden = dash.configReady;
    const miss = document.getElementById('config-banner-missing');
    if (miss) miss.textContent = dash.missing.join('、');
  }
  const eb = document.getElementById('init-error-banner');
  if (eb) {
    eb.hidden = !dash.initError;
    const txt = document.getElementById('init-error-text');
    if (txt) txt.textContent = dash.initError;
  }
}

function renderTaskCard(name) {
  const s = dash[name];
  const badge = document.getElementById(`${name}-badge`);
  if (badge) {
    badge.textContent = s.badgeText;
    badge.className = s.badgeCls;
  }
  const done = document.getElementById(`${name}-done`);
  if (done) done.textContent = s.done;
  const total = document.getElementById(`${name}-total`);
  if (total) total.textContent = s.total;
  const bar = document.getElementById(`${name}-bar`);
  if (bar) bar.style.width = s.barWidth;
  const btn = document.getElementById(`${name}-btn`);
  if (btn) {
    btn.textContent = s.btnText;
    btn.className = s.btnCls;
    btn.disabled = !s.ready;
  }
}

function bindTaskButtons() {
  const sync = document.getElementById('sync-btn');
  if (sync) sync.addEventListener('click', () => dash.toggle('sync'));
  const strm = document.getElementById('strm-btn');
  if (strm) strm.addEventListener('click', () => dash.toggle('strm'));
}

// ──── 日志流（批量渲染：事件入队 → rAF 调度 → Fragment 一次插入）────

let logBox = null;

let closeLogs = null;   // 日志 SSE 的关闭函数
let logFilter = 'all';  // all / warn / error / sync / strm / drive / cloud / db / system（同一行互斥）
const filterKeys = ['all', 'warn', 'error', 'sync', 'strm', 'drive', 'cloud', 'db', 'system'];
let counts = Object.fromEntries(filterKeys.map(k => [k, 0]));

let pending = [];           // 待渲染事件队列（含 status 条目）
let flushScheduled = false; // 已安排 flush，避免一帧内重复调度

const MAX_LINES = 300;
const TRIM_EVERY = 50;
let trimCount = 0;

const LEVEL_KEYS = new Set(['all', 'warn', 'error']);
const _chipQ = Object.fromEntries(filterKeys.map(k =>
  [k, `#log-filter .chip[data-${LEVEL_KEYS.has(k) ? 'lv' : 'mod'}="${k}"] .chip-count`]
));

// 手写时间格式（保留毫秒），免每条重建 Intl 实例
function fmtTime(d) {
  const p = n => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${String(d.getMilliseconds()).padStart(3, '0')}`;
}

function _updateChip(key, val) {
  const el = document.querySelector(_chipQ[key]);
  if (el) el.textContent = val;
}

function resetCounts() {
  for (const k of filterKeys) { counts[k] = 0; _updateChip(k, '0'); }
}

// 计数纯变量累加，DOM 写入延迟到 flush 末尾统一完成（消除每条 querySelector）
function bumpCount(level, mod) {
  counts.all++;
  if (level === 'WARN') counts.warn++;
  if (level === 'ERROR') counts.error++;
  if (mod && counts.hasOwnProperty(mod)) counts[mod]++;
}

// 模块中文名映射（module label 显示）；web 已并入 system
const moduleLabels = { sync: '同步', strm: 'STRM', drive: '直链', system: '系统', cloud: '云端', db: '数据库' };

export function initLogs() {
  logBox = document.getElementById('log-box');
  if (logBox && !logBox.querySelector('.log-line')) {
    logBox.innerHTML = '<div class="muted empty">正在连接日志流…</div>';
  }
  closeLogs = connectSSE('/api/logs', {
    onMessage: renderLog,
    onOpen: () => {
      pending = [];
      if (logBox) logBox.innerHTML = '<div class="muted empty">暂无日志</div>';
      resetCounts();
    },
    shouldReconnect: () => !document.getElementById('view-dashboard').hidden,
  });

  const clear = document.getElementById('log-clear');
  if (clear) clear.onclick = clearLogs;

  // chip 过滤：容器事件委托，同一行互斥选中；切换分类时强制滚到底显示最新日志
  const filter = document.getElementById('log-filter');
  if (filter) filter.addEventListener('click', e => {
    const btn = e.target.closest('.chip');
    if (!btn) return;
    logFilter = btn.dataset.lv || btn.dataset.mod;
    filter.querySelectorAll('.chip').forEach(b => b.classList.toggle('active', b === btn));
    applyFilter();
    if (logBox) logBox.scrollTop = logBox.scrollHeight;
  });
}

export function stopLogs() {
  closeLogs?.();
  closeLogs = null;
}

async function clearLogs() {
  pending = [];
  if (logBox) logBox.innerHTML = '<div class="muted empty">暂无日志</div>';
  resetCounts();
  try { await api('/api/logs/clear', { method: 'POST' }); } catch { /* 忽略 */ }
}

function matchFilter(level, mod) {
  if (logFilter === 'all') return true;
  if (logFilter === 'warn') return level === 'WARN';
  if (logFilter === 'error') return level === 'ERROR';
  // 按模块过滤 — 该模块所有级别
  return mod === logFilter;
}

function applyFilter() {
  if (!logBox) return;
  logBox.querySelectorAll('.log-line').forEach(line => {
    line.hidden = !matchFilter(line.dataset.level, line.dataset.module);
  });
}

// 入队：单条或数组帧（后端回放为单个 JSON 数组帧）统一进 pending，由 flush 批量渲染。
function renderLog(en) {
  if (Array.isArray(en)) pending.push(...en);
  else pending.push(en);
  scheduleFlush();
}

function scheduleFlush() {
  if (flushScheduled) return;
  flushScheduled = true;
  // 页面可见走 rAF 贴帧渲染；隐藏时降级定时器，避免后台空转。
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(flush);
  else setTimeout(flush, 25);
}

function flush() {
  flushScheduled = false;
  const batch = pending;
  pending = [];
  if (!batch.length) return;

  const box = logBox;
  // 贴底自动跟随滚动；用户上翻查看历史不被强制拉回（批量后整批只重排一次）。
  const wasAtBottom = !!box &&
    box.scrollTop + box.clientHeight >= box.scrollHeight - 8;

  // status 条目更新任务卡与 banner；日志累计计数。
  let domCount = 0;
  for (const en of batch) {
    if (en.status) { handleStatus(en); continue; }
    domCount++;
    bumpCount(String(en.level || 'INFO').toUpperCase(), String(en.module || 'system'));
  }

  // 首帧裁剪：超过 MAX_LINES 只构建末尾 MAX_LINES 条 DOM（计数已全量累计，与 trim 语义一致）。
  const start = Math.max(0, domCount - MAX_LINES);
  let skip = 0;
  const frag = document.createDocumentFragment();
  for (const en of batch) {
    if (en.status) continue;
    if (skip++ < start) continue;
    frag.appendChild(buildLine(en));
  }

  if (frag.childElementCount) {
    const empty = box && box.querySelector('.empty');
    if (empty) empty.remove();
    if (box) box.appendChild(frag);
    trimToMax();
  }
  flushChips();
  if (wasAtBottom && box) box.scrollTop = box.scrollHeight;
}

function handleStatus(en) {
  dash.configReady = en.status.config_ready;
  dash.missing = en.status.missing || [];
  dash.initError = en.status.init_error || '';
  dash.setStatus('sync', en.status.sync);
  dash.setStatus('strm', en.status.strm);
  renderStatus();
}

// 构建单行日志 DOM（纯 textContent 写入，天然防 XSS）
function buildLine(en) {
  const level = String(en.level || 'INFO').toUpperCase();
  const mod = String(en.module || 'system');
  const line = document.createElement('div');
  line.className = 'log-line lv-' + level.toLowerCase();
  line.dataset.level = level;
  line.dataset.module = mod;
  line.hidden = !matchFilter(level, mod);

  // 模块标签
  const modSpan = document.createElement('span');
  modSpan.className = 'log-mod lv-' + mod;
  modSpan.textContent = moduleLabels[mod] || mod;

  const t = document.createElement('span');
  t.className = 'log-time';
  t.textContent = fmtTime(new Date(en.time));

  const lv = document.createElement('span');
  lv.className = 'log-lv';
  lv.textContent = en.level;

  const msg = document.createElement('span');
  msg.className = 'log-msg';
  msg.textContent = en.msg + (en.attrs ? '  ' + en.attrs : '');

  line.append(modSpan, t, lv, msg);
  return line;
}

function flushChips() {
  for (const k of filterKeys) _updateChip(k, counts[k]);
}

// 超出上限裁掉最早的行（与原逻辑一致，flush 后统一执行）
function trimToMax() {
  if (!logBox) return;
  if (++trimCount % TRIM_EVERY !== 0) return;
  while (logBox.childElementCount > MAX_LINES) logBox.removeChild(logBox.firstElementChild);
}
