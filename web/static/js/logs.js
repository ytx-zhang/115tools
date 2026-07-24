// logs.js —— 统一日志流：连接 /api/logs SSE，渲染日志行，支持级别过滤/暂停滚动/清空。
//
// 运行日志与错误日志已合并为这一条管道：服务端 slog 输出的所有级别日志
// （含失败明细的 ERROR 级）都经 logstream 推送过来。
// 「错误」过滤按钮即等价于旧的独立错误日志卡片，且附带时间戳与上下文。
import { api } from './api.js';

let logsEs = null;     // 日志 SSE 连接
let logPaused = false; // 暂停自动滚动（用户上翻查看历史时勾选）
let logFilter = 'all'; // 当前级别过滤器：all / info / warn / error

// initLogs 初始化日志卡片（进入仪表盘时由 dashboard.js 调用）。
export function initLogs() {
  connectLogs();

  const pause = document.getElementById('log-pause');
  if (pause) pause.onchange = e => { logPaused = e.target.checked; };

  const clear = document.getElementById('log-clear');
  if (clear) clear.onclick = clearLogs;

  // 级别过滤按钮组：点击切换过滤器，并对已有日志行即时生效
  document.querySelectorAll('#log-filter .chip').forEach(btn => {
    btn.onclick = () => {
      logFilter = btn.dataset.lv;
      document.querySelectorAll('#log-filter .chip')
        .forEach(b => b.classList.toggle('active', b === btn));
      applyFilter();
    };
  });
}

// stopLogs 关闭日志流（离开仪表盘时由 dashboard.js 调用）。
export function stopLogs() {
  logsEs?.close();
  logsEs = null;
}

// connectLogs 打开日志 SSE。连接时服务端会先回放近期日志，再持续推送新日志。
function connectLogs() {
  logsEs?.close();
  const box = document.getElementById('log-box');
  if (box && !box.querySelector('.log-line')) {
    box.innerHTML = '<div class="muted empty">正在连接日志流…</div>';
  }
  logsEs = new EventSource('/api/logs');
  logsEs.onmessage = e => {
    try { renderLog(JSON.parse(e.data)); } catch { /* 忽略损坏帧 */ }
  };
  logsEs.onopen = () => {
    // 连接已建立：重连时服务端会重新回放近期日志，先清空旧内容避免重复。
    // 注意服务端在回放前先发 ": connected" 注释帧，本 onopen 必在其 onmessage 之前触发，
    // 因此此处清空不会误删即将到达的回放数据。
    const box = document.getElementById('log-box');
    if (box) box.innerHTML = '<div class="muted empty">暂无日志</div>';
  };
  logsEs.onerror = () => {
    // 断线（含 401 / 服务重启）：若会话失效则退回登录页，否则 3 秒后重连
    logsEs?.close();
    logsEs = null;
    setTimeout(async () => {
      if (!document.getElementById('view-dashboard').hidden) {
        try { await api('/api/me'); connectLogs(); } catch { /* 401 已由事件处理 */ }
      }
    }, 3000);
  };
}

// clearLogs 清空：页面 DOM 与服务端内存缓冲一起清。
async function clearLogs() {
  const box = document.getElementById('log-box');
  if (box) box.innerHTML = '<div class="muted empty">暂无日志</div>';
  try { await api('/api/logs/clear', { method: 'POST' }); } catch { /* 忽略 */ }
}

// matchFilter 判断某级别的日志行在当前过滤器下是否应显示。
function matchFilter(level) {
  return logFilter === 'all' || level === logFilter.toUpperCase();
}

// applyFilter 按当前过滤器刷新已有日志行的显隐（纯前端行为，即时生效）。
function applyFilter() {
  const box = document.getElementById('log-box');
  if (!box) return;
  box.querySelectorAll('.log-line').forEach(line => {
    line.hidden = !matchFilter(line.dataset.level);
  });
}

// renderLog 把一条日志渲染为一行：时间 + 级别徽标 + 消息。
// 行始终加入 DOM，仅按过滤器决定显隐——这样切换过滤器能找回之前收到的行。
function renderLog(en) {
  const box = document.getElementById('log-box');
  if (!box) return;
  const empty = box.querySelector('.empty');
  if (empty) empty.remove();

  const level = String(en.level || 'INFO').toUpperCase();
  const line = document.createElement('div');
  line.className = 'log-line lv-' + level.toLowerCase();
  line.dataset.level = level; // 供过滤器判定
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

  // 控制 DOM 体积，最多保留 300 条
  while (box.childElementCount > 300) box.removeChild(box.firstElementChild);

  if (!logPaused) box.scrollTop = box.scrollHeight;
}
