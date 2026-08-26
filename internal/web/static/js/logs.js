// logs.js —— 日志查看器（独立关注点）：分类计数 SSE + 日志流 SSE + 行级过滤 +
// 批量渲染队列 + 向上滚动翻历史。状态帧（en.status）通过 onStatus 回调交还仪表盘更新任务卡，
// 自身不依赖 dashboard 状态对象，避免循环依赖。
import { api, connectSSE, fmtTime } from './api.js';

let closeLogs = null;    // 日志 SSE 的关闭函数
let closeCounts = null;  // 分类计数 SSE 的关闭函数（与日志流分离，切换分类不重建）
let logFilter = 'all';  // all / warn / error / sync / strm / drive / cloud / db / system（同一行互斥）
const filterKeys = ['all', 'warn', 'error', 'sync', 'strm', 'drive', 'cloud', 'db', 'system'];

let pending = [];           // 待渲染事件队列（含 status 条目）
let flushScheduled = false; // 已安排 flush，避免一帧内重复调度

const MAX_LINES = 300;     // 贴底实时跟随时保留的最新日志窗口
const HISTORY_CAP = 1200;  // 向上翻历史时 DOM 允许膨胀的上限（仍远小于 ring 5000，防无限增长）
const TRIM_EVERY = 50;
let trimCount = 0;

// 向上滚动加载更早日志（历史分页）：后端 /api/logs/history?before=<seq> 返回该 seq 之前的条目。
// loadingHistory 防止并发翻页；historyExhausted 表示当前分类已无更早日志（不再空请求）。
let loadingHistory = false;
let historyExhausted = false;

const LEVEL_KEYS = new Set(['all', 'warn', 'error']);
const _chipQ = Object.fromEntries(filterKeys.map(k =>
  [k, `#log-filter .chip[data-${LEVEL_KEYS.has(k) ? 'lv' : 'mod'}="${k}"] .chip-count`]
));

// 模块中文名映射（module label 显示）；web 已并入 system
const moduleLabels = { sync: '同步', strm: 'STRM', drive: '直链', system: '系统', cloud: '云端', db: '数据库' };

// statusHandler 由 dashboard 通过 onStatus 注入：日志流里的状态帧转交它更新任务卡。
let statusHandler = null;
export function onStatus(fn) { statusHandler = fn; }

function _updateChip(key, val) {
  const el = document.querySelector(_chipQ[key]);
  if (el) el.textContent = val;
}

// 分类计数直接采用服务端 ring 可见条数（/api/logs/counts SSE），不再本地数渲染行，
// 与回放/翻页同一数据源，切换分类、回放截断都不会导致计数失真。

export function initLogs() {
  const logBox = document.getElementById('log-box');
  if (logBox && !logBox.querySelector('.log-line')) {
    logBox.innerHTML = '<div class="muted empty">正在连接日志流…</div>';
  }
  // 向上滚动加载更早历史：监听只挂一次（dataset 标记守卫）。
  if (logBox && !logBox.dataset.histBound) {
    logBox.dataset.histBound = '1';
    logBox.addEventListener('scroll', onLogScroll, { passive: true });
  }
  bindLogUI();
  openLogs();
  openCounts();
}

// 日志相关 UI（清空按钮、分类 chip）绑定一次（uiBound 守卫），跨 init/stop 生命周期不重复绑定。
let uiBound = false;
function bindLogUI() {
  if (uiBound) return;
  uiBound = true;

  const clear = document.getElementById('log-clear');
  if (clear) clear.addEventListener('click', clearLogs);

  // chip 过滤：容器事件委托，同一行互斥选中；切换分类时重建带 cat 的 SSE
  // （后端按 cat 过滤回放历史 + 实时推送，无需再拉独立的历史接口）。
  const filter = document.getElementById('log-filter');
  if (filter) filter.addEventListener('click', e => {
    const btn = e.target.closest('.chip');
    if (!btn) return;
    const cat = btn.dataset.lv || btn.dataset.mod;
    if (cat === logFilter) return; // 同分类不重建
    logFilter = cat;
    filter.querySelectorAll('.chip').forEach(b => b.classList.toggle('active', b === btn));
    reconnectLogs();
  });
}

// openCounts 建立分类计数 SSE（与日志流分离）。服务端基于 ring 扫描给出各分类当前可见条数，
// 仅在有新日志写入时推送（事件驱动、空闲不推），前端 chip 直接显示；与回放/翻页同一数据源，
// 保证「chip 显示有日志 ⇔ 点进去能看到日志」。
function openCounts() {
  closeCounts = connectSSE('/api/logs/counts', {
    onMessage: en => {
      if (en && en.counts) {
        for (const k in en.counts) _updateChip(k, String(en.counts[k]));
      }
    },
    shouldReconnect: () => !document.getElementById('view-dashboard').hidden,
  });
}

// openLogs 建立（或重建）带当前分类的日志 SSE。后端按 cat 过滤回放历史并实时推送，
// 切换分类时调用 reconnectLogs 断开旧连接、用新 cat 重建即可，无需独立历史接口。
function openLogs() {
  closeLogs = connectSSE(`/api/logs?cat=${encodeURIComponent(logFilter)}`, {
    onMessage: renderLog,
    onOpen: () => {
      pending = [];
      loadingHistory = false;
      historyExhausted = false;
      const box = document.getElementById('log-box');
      if (box) box.innerHTML = '<div class="muted empty">暂无日志</div>';
    },
    shouldReconnect: () => !document.getElementById('view-dashboard').hidden,
  });
}

// reconnectLogs 切换分类：断开当前连接并以新 cat 重建 SSE。
function reconnectLogs() {
  closeLogs?.();
  closeLogs = null;
  openLogs();
}

export function stopLogs() {
  closeLogs?.();
  closeLogs = null;
  closeCounts?.();
  closeCounts = null;
}

async function clearLogs() {
  pending = [];
  loadingHistory = false;
  historyExhausted = false;
  const box = document.getElementById('log-box');
  if (box) box.innerHTML = '<div class="muted empty">暂无日志</div>';
  // chip 计数来自事件驱动的 counts SSE（仅新日志写入时推送），清空后若长时间无新日志
  // 旧计数不会刷新，故本地立即归零；后续 counts SSE 会以服务端权威值校正。
  for (const k of filterKeys) _updateChip(k, '0');
  try { await api('/api/logs/clear', { method: 'POST' }); } catch { /* 忽略 */ }
}

// matchFilter 是前端行级显隐过滤：SSE 已推送全量日志后，按当前 chip 即时显隐已渲染行。
// ⚠️ 与后端 logs.LogFilter.Matches 逻辑对称但服务不同切面（后端按 cat 过滤回放历史），不可互相删除。
function matchFilter(level, mod) {
  if (logFilter === 'all') return true;
  if (logFilter === 'warn') return level === 'WARN';
  if (logFilter === 'error') return level === 'ERROR';
  // 按模块过滤 — 该模块所有级别
  return mod === logFilter;
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

  const box = document.getElementById('log-box');
  // 贴底自动跟随滚动；用户上翻查看历史不被强制拉回（批量后整批只重排一次）。
  const wasAtBottom = !!box &&
    box.scrollTop + box.clientHeight >= box.scrollHeight - 8;

  // status 条目更新任务卡与 banner。
  let domCount = 0;
  for (const en of batch) {
    if (en.status) { handleStatus(en); continue; }
    domCount++;
  }

  // 首帧裁剪：超过 MAX_LINES 只构建末尾 MAX_LINES 条 DOM。
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
    trimToMax(wasAtBottom);
  }
  if (wasAtBottom && box) box.scrollTop = box.scrollHeight;
}

// 状态帧交还 dashboard 更新任务卡（通过 onStatus 注入的回调）。
function handleStatus(en) {
  if (statusHandler) statusHandler(en);
}

// 构建单行日志 DOM（纯 textContent 写入，天然防 XSS）
function buildLine(en) {
  const level = String(en.level || 'INFO').toUpperCase();
  const mod = String(en.module || 'system');
  const line = document.createElement('div');
  line.className = 'log-line lv-' + level.toLowerCase();
  line.dataset.level = level;
  line.dataset.module = mod;
  line.dataset.seq = String(en.seq);
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

// 裁剪 DOM 行数。方向随「数据从哪端加入」而变，保证用户正在看的那一端不被裁掉：
// keepBottom=true（贴底实时流）：丢最旧（顶部），保留最新 MAX_LINES；
// keepBottom=false（向上翻历史）：丢最新（底部，离屏且会随实时流回流），保留旧日志，封顶 HISTORY_CAP。
// 这是相比原「永远裁顶部」的关键修正——原逻辑在 prepend 历史时会把刚加载的旧日志裁掉，翻页无效。
function trimToMax(keepBottom) {
  const box = document.getElementById('log-box');
  if (!box) return;
  if (++trimCount % TRIM_EVERY !== 0) return;
  const cap = keepBottom ? MAX_LINES : HISTORY_CAP;
  while (box.childElementCount > cap) {
    box.removeChild(keepBottom ? box.firstElementChild : box.lastElementChild);
  }
}

// ──── 历史分页：向上滚动加载更早日志 ────

// 滚动到顶（scrollTop 接近 0）时触发翻页，拉取当前分类中早于顶部 seq 的一批日志。
function onLogScroll() {
  const box = document.getElementById('log-box');
  if (!box || loadingHistory || historyExhausted) return;
  if (box.scrollTop < 24) loadHistory();
}

// 当前最顶部日志行的 seq，作为历史分页游标。无日志行时返回 0（视为「尚未回放」）。
function topSeq() {
  const box = document.getElementById('log-box');
  if (!box) return 0;
  const top = box.querySelector('.log-line');
  return top ? Number(top.dataset.seq) || 0 : 0;
}

// 拉取 before=<顶部seq> 的更早一批日志（后端按当前分类过滤），插入到最前面并维持视口位置，
// 避免查看历史时被跳回顶部（按插入前后高度差补偿 scrollTop）。
async function loadHistory() {
  const box = document.getElementById('log-box');
  if (loadingHistory || historyExhausted || !box) return;
  const before = topSeq();
  if (before <= 0) return; // 尚无锚点（未回放/无行），不标记穷尽，等待回放后再翻
  loadingHistory = true;
  try {
    const rows = await api(`/api/logs/history?cat=${encodeURIComponent(logFilter)}&before=${before}&limit=200`);
    if (!Array.isArray(rows) || !rows.length) {
      historyExhausted = true;
      markHistoryExhausted();
      return;
    }
    // 升序排（后端已按 ring 顺序，保险起见按 seq 重排），prepend 后顶部即最早一条。
    rows.toSorted((a, b) => (a.seq || 0) - (b.seq || 0));
    const frag = document.createDocumentFragment();
    for (const en of rows) frag.appendChild(buildLine(en));
    const prevHeight = box.scrollHeight;
    const empty = box.querySelector('.empty');
    if (empty) empty.remove();
    box.insertBefore(frag, box.firstChild);
    const delta = box.scrollHeight - prevHeight;
    if (delta > 0) box.scrollTop += delta; // 补偿：插入内容把上方顶下去，保持视口不动
    trimToMax(false); // 历史从顶部加入 → 裁底部（最新、离屏且会实时回流），保留刚加载的旧日志
  } catch {
    // 拉取失败：保留现状，允许下次滚动到顶重试
  } finally {
    loadingHistory = false;
  }
}

// 当前分类已无更早日志时，在顶部插入一条提示，避免用户反复滚动无效翻页。
function markHistoryExhausted() {
  const box = document.getElementById('log-box');
  if (!box || box.querySelector('.hist-top')) return;
  const tip = document.createElement('div');
  tip.className = 'muted empty hist-top';
  tip.textContent = '— 已加载全部历史日志 —';
  box.insertBefore(tip, box.firstChild);
}
