// api.js —— fetch 封装与全局提示
// 401 时派发 auth:required 事件，由 main.js 切回登录页。

export async function api(path, options = {}) {
  const isFormData = options.body instanceof FormData;
  if (options.body && typeof options.body !== 'string' && !isFormData) {
    options.body = JSON.stringify(options.body);
    options.headers = { 'Content-Type': 'application/json', ...options.headers };
  }
  const resp = await fetch(path, options);

  if (resp.status === 401) {
    window.dispatchEvent(new CustomEvent('auth:required'));
    throw new ApiError(401, '未登录或会话已过期');
  }

  let data = null;
  try { data = await resp.json(); } catch { /* 非 JSON 响应 */ }

  if (!resp.ok) {
    throw new ApiError(resp.status, data?.error || `请求失败（HTTP ${resp.status}）`);
  }
  return data;
}

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

// toast(msg, type)：type 为 ok / err / 空
export function toast(msg, type = '') {
  const box = document.getElementById('toast-box');
  const el = document.createElement('div');
  el.className = `toast ${type}`;
  el.textContent = msg;
  box.appendChild(el);
  setTimeout(() => el.remove(), 4000);
}

// 格式化字节数
export function fmtSize(bytes) {
  if (!bytes) return '-';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, n = bytes;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(n >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

// HTML 转义
export function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g,
    c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// connectSSE 打开一条 SSE 连接，内聚重连/会话/守卫逻辑，消除重复的 EventSource 样板。
// 调用方仅需提供 onMessage（必须）与 onOpen（可选）渲染回调；返回 close 函数，离开视图时调用。
// shouldReconnect 可选：返回 true 才在断线后重连（解耦对具体视图元素的硬编码，默认始终重连）。
export function connectSSE(url, { onMessage, onOpen, shouldReconnect } = {}) {
  let es = null;
  let seq = 0; // 本连接序号，防重连回调误关已重建的新连接

  function open() {
    es?.close();
    es = new EventSource(url);
    const errSeq = ++seq;
    es.onmessage = e => {
      try { onMessage(JSON.parse(e.data)); } catch { /* 忽略损坏帧 */ }
    };
    if (onOpen) es.onopen = onOpen;
    es.onerror = () => {
      es?.close();
      es = null;
      setTimeout(async () => {
        if (errSeq !== seq) return; // 期间已重建过，放弃本次重连
        if (shouldReconnect && !shouldReconnect()) return;
        try { await api('/api/me'); open(); } catch { /* 401 已由事件处理 */ }
      }, 3000);
    };
  }

  open();
  return () => { seq++; es?.close(); es = null; };
}
