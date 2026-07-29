// logs.js —— 日志流：渲染、级别过滤/暂停滚动/清空。
import { api } from './api.js';
import { connectSSE } from './sse.js';

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
