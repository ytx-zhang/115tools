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
