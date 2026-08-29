// api.js —— 请求封装、SSE、toast、格式化工具（零依赖）。

// api 通用请求：自动 JSON、401 未登录时退回登录页。
export async function api(path, opts = {}) {
  const headers = { ...(opts.headers || {}) };
  if (opts.body && !(opts.body instanceof FormData)) {
    headers['Content-Type'] = 'application/json';
  }
  const res = await fetch(path, { ...opts, headers });
  if (res.status === 401) {
    const { showLogin } = await import('./main.js');
    showLogin();
    throw new Error('unauthorized');
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}

// connectSSE 建立 SSE 连接，返回关闭函数。
export function connectSSE(path, { onMessage, onOpen, onError } = {}) {
  const es = new EventSource(path);
  let closed = false;
  es.onmessage = (e) => {
    let data;
    try { data = JSON.parse(e.data); } catch { return; }
    onMessage && onMessage(data);
  };
  es.onopen = () => onOpen && onOpen();
  es.onerror = () => {
    if (closed) return;
    onError && onError();
  };
  return () => { closed = true; es.close(); };
}

// toast 轻提示（图标 + 倒计时条）。
const TOAST_ICON = { ok: '#i-check', err: '#i-x', info: '#i-info' };
export function toast(msg, type = 'info') {
  const box = document.getElementById('toast-box');
  if (!box) return;
  const el = document.createElement('div');
  el.className = 'toast ' + type;
  const ic = svgIcon(TOAST_ICON[type] || TOAST_ICON.info, 't-ic');
  const span = document.createElement('span');
  span.textContent = msg;
  el.append(ic, span);
  box.appendChild(el);
  setTimeout(() => {
    el.style.transition = 'opacity .3s, transform .3s';
    el.style.opacity = '0';
    el.style.transform = 'translateX(24px)';
    setTimeout(() => el.remove(), 320);
  }, 3000);
}

// fmtTime 本地时间 HH:MM:SS。
export function fmtTime(d) {
  if (!d) return '';
  const t = new Date(d);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(t.getMonth() + 1)}-${p(t.getDate())} ${p(t.getHours())}:${p(t.getMinutes())}:${p(t.getSeconds())}`;
}

// fmtBytes 字节数格式化。
export function fmtBytes(n) {
  if (!n || n <= 0) return '0B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 || i === 0 ? 0 : 1) + units[i];
}

// fmtDuration 毫秒 → 可读耗时。
export function fmtDuration(ms) {
  if (ms == null) return '';
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  return `${m}m${Math.round(s % 60)}s`;
}

// el 便捷创建元素。
export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}

// svgIcon 创建带 use 引用的 SVG 图标（href 与 xlink:href 双写，兼容旧浏览器）。
export function svgIcon(icon, cls = 'ic') {
  const ns = 'http://www.w3.org/2000/svg';
  const s = document.createElementNS(ns, 'svg');
  s.classList.add(cls);
  const u = document.createElementNS(ns, 'use');
  u.setAttribute('href', icon);
  u.setAttributeNS('http://www.w3.org/1999/xlink', 'xlink:href', icon);
  s.appendChild(u);
  return s;
}

// btnWithIcon 带 SVG 图标的按钮（cls 如 'btn sm'，text 可传 null，dataset 映射为 data-* 属性）。
export function btnWithIcon(cls, text, icon, dataset) {
  const b = el('button', cls, text);
  b.prepend(svgIcon(icon));
  for (const k in dataset) b.dataset[k] = dataset[k];
  return b;
}
