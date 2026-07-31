// dashboard.js —— 任务状态实时展示（SSE）与启停控制，含日志流（渲染/过滤/暂停/清空）。
// 状态卡用 petite-vue 声明式绑定（见 index.html v-scope="dash"）；日志流保留命令式 append。
import { api, toast, connectSSE } from './api.js';

let closeStatus = null; // 状态 SSE 的关闭函数

const startText = { sync: '开始同步', strm: '开始生成' };

// dash 是 petite-vue 托管的响应式状态对象（reactive 包裹，导出即代理，SSE 写入即刷新 DOM）。
export const dash = window.PetiteVue.reactive({
  configReady: true,
  missing: [],
  reloading: false,
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
  closeStatus = connectSSE('/api/status', { onMessage: render });
  initLogs();
}

export function stopDashboard() {
  closeStatus?.();
  closeStatus = null;
  stopLogs();
}

function render(data) {
  dash.configReady = data.config_ready;
  dash.missing = data.missing || [];
  dash.reloading = !data.ready && data.config_ready;
  dash.setStatus('sync', data.sync);
  dash.setStatus('strm', data.strm);
}

// ──── 日志流（保留命令式 append）────

let closeLogs = null;   // 日志 SSE 的关闭函数
let logPaused = false;  // 暂停自动滚动
let logFilter = 'all';  // all / info / warn / error
const counts = { all: 0, info: 0, warn: 0, error: 0 };

function resetCounts() {
  counts.all = counts.info = counts.warn = counts.error = 0;
  document.querySelectorAll('#log-filter .chip').forEach(ch => {
    const s = ch.querySelector('.chip-count');
    if (s) s.textContent = '0';
  });
}

function bumpCount(level) {
  counts.all++;
  const allEl = document.querySelector('#log-filter .chip[data-lv="all"] .chip-count');
  if (allEl) allEl.textContent = counts.all;
  const key = String(level).toLowerCase();
  if (key in counts) {
    counts[key]++;
    const el = document.querySelector(`#log-filter .chip[data-lv="${key}"] .chip-count`);
    if (el) el.textContent = counts[key];
  }
}

export function initLogs() {
  const box = document.getElementById('log-box');
  if (box && !box.querySelector('.log-line')) {
    box.innerHTML = '<div class="muted empty">正在连接日志流…</div>';
  }
  closeLogs = connectSSE('/api/logs', {
    onMessage: renderLog,
    onOpen: () => {
      // 重连时服务端会重新回放近期日志，先清空旧内容避免重复。
      const box = document.getElementById('log-box');
      if (box) box.innerHTML = '<div class="muted empty">暂无日志</div>';
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
      logFilter = btn.dataset.lv;
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
  const box = document.getElementById('log-box');
  if (box) box.innerHTML = '<div class="muted empty">暂无日志</div>';
  resetCounts();
  try { await api('/api/logs/clear', { method: 'POST' }); } catch { /* 忽略 */ }
}

function matchFilter(level) {
  return logFilter === 'all' || level === logFilter.toUpperCase();
}

function applyFilter() {
  const box = document.getElementById('log-box');
  if (!box) return;
  box.querySelectorAll('.log-line').forEach(line => {
    line.hidden = !matchFilter(line.dataset.level);
  });
}

function renderLog(en) {
  const box = document.getElementById('log-box');
  if (!box) return;
  const empty = box.querySelector('.empty');
  if (empty) empty.remove();

  const level = String(en.level || 'INFO').toUpperCase();
  const line = document.createElement('div');
  line.className = 'log-line lv-' + level.toLowerCase();
  line.dataset.level = level;
  line.hidden = !matchFilter(level);

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

  line.append(t, lv, msg);
  box.appendChild(line);

  bumpCount(level);
  while (box.childElementCount > 300) box.removeChild(box.firstElementChild);
  if (!logPaused) box.scrollTop = box.scrollHeight;
}
