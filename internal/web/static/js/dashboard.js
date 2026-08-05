// dashboard.js —— 任务状态与日志统一走 /api/logs SSE。
// 状态卡用 petite-vue 声明式绑定（见 index.html v-scope="dash"）；日志流保留命令式 append。
import { api, toast, connectSSE } from './api.js';

const startText = { sync: '开始同步', strm: '开始生成' };

// dash 是 petite-vue 托管的响应式状态对象（reactive 包裹，导出即代理，SSE 写入即刷新 DOM）。
export const dash = window.PetiteVue.reactive({
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
});

export function initDashboard() {
  initLogs();
}

export function stopDashboard() {
  stopLogs();
}

// ──── 日志流（保留命令式 append）────

let logBox = null;

let closeLogs = null;   // 日志 SSE 的关闭函数
let logPaused = false;  // 暂停自动滚动
let logFilter = 'all';  // all / warn / error / sync / strm / drive / web / system（同一行互斥）
const filterKeys = ['all', 'warn', 'error', 'sync', 'strm', 'drive', 'web', 'system', 'cloud', 'db'];
let counts = Object.fromEntries(filterKeys.map(k => [k, 0]));

const MAX_LINES = 300;
const TRIM_EVERY = 50;
let trimCount = 0;

const LEVEL_KEYS = new Set(['all', 'warn', 'error']);
const _chipQ = Object.fromEntries(filterKeys.map(k =>
  [k, `#log-filter .chip[data-${LEVEL_KEYS.has(k) ? 'lv' : 'mod'}="${k}"] .chip-count`]
));

function _updateChip(key, val) {
  const el = document.querySelector(_chipQ[key]);
  if (el) el.textContent = val;
}

function resetCounts() {
  for (const k of filterKeys) { counts[k] = 0; _updateChip(k, '0'); }
}

function bumpCount(level, mod) {
  counts.all++;
  _updateChip('all', counts.all);
  if (level === 'WARN' || level === 'ERROR') { counts.warn++; _updateChip('warn', counts.warn); }
  if (level === 'ERROR') { counts.error++; _updateChip('error', counts.error); }
  if (mod && counts.hasOwnProperty(mod)) { counts[mod]++; _updateChip(mod, counts[mod]); }
}

// 模块中文名映射（module label 显示）
const moduleLabels = { sync: '同步', strm: 'STRM', drive: '直链', web: '管理', system: '系统', cloud: '云端', db: '数据库' };

export function initLogs() {
  logBox = document.getElementById('log-box');
  if (logBox && !logBox.querySelector('.log-line')) {
    logBox.innerHTML = '<div class="muted empty">正在连接日志流…</div>';
  }
  closeLogs = connectSSE('/api/logs', {
    onMessage: renderLog,
    onOpen: () => {
      if (logBox) logBox.innerHTML = '<div class="muted empty">暂无日志</div>';
      resetCounts();
    },
    shouldReconnect: () => !document.getElementById('view-dashboard').hidden,
  });

  const pause = document.getElementById('log-pause');
  if (pause) pause.onchange = e => { logPaused = e.target.checked; };

  const clear = document.getElementById('log-clear');
  if (clear) clear.onclick = clearLogs;

  document.querySelectorAll('#log-filter .chip').forEach(btn => {
    btn.onclick = () => {
      logFilter = btn.dataset.lv || btn.dataset.mod;
      document.querySelectorAll('#log-filter .chip')
        .forEach(b => b.classList.toggle('active', b === btn));
      applyFilter();
    };
  });
}

export function stopLogs() {
  closeLogs?.();
  closeLogs = null;
}

async function clearLogs() {
  if (logBox) logBox.innerHTML = '<div class="muted empty">暂无日志</div>';
  resetCounts();
  try { await api('/api/logs/clear', { method: 'POST' }); } catch { /* 忽略 */ }
}

function matchFilter(level, mod) {
  if (logFilter === 'all') return true;
  if (logFilter === 'warn') return level === 'WARN' || level === 'ERROR';
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

function renderLog(en) {
  // status 条目不创建日志 DOM，直接更新 dash 卡片
  if (en.status) {
    dash.configReady = en.status.config_ready;
    dash.missing = en.status.missing || [];
    dash.initError = en.status.init_error || '';
    dash.setStatus('sync', en.status.sync);
    dash.setStatus('strm', en.status.strm);
    return;
  }

  if (!logBox) return;
  const empty = logBox.querySelector('.empty');
  if (empty) empty.remove();

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
  const d = new Date(en.time);
  const ms = String(d.getMilliseconds()).padStart(3, '0');
  t.textContent = d.toLocaleTimeString('zh-CN', { hour12: false }) + '.' + ms;

  const lv = document.createElement('span');
  lv.className = 'log-lv';
  lv.textContent = en.level;

  const msg = document.createElement('span');
  msg.className = 'log-msg';
  msg.textContent = en.msg + (en.attrs ? '  ' + en.attrs : '');

  line.append(modSpan, t, lv, msg);
  logBox.appendChild(line);

  bumpCount(level, mod);
  if (++trimCount % TRIM_EVERY === 0) {
    while (logBox.childElementCount > MAX_LINES) logBox.removeChild(logBox.firstElementChild);
  }
  if (!logPaused) logBox.scrollTop = logBox.scrollHeight;
}
