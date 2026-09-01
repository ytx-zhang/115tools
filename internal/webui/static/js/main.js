// main.js —— 应用入口（主包，静态加载）：登录、主题三态、hash 路由、SSE 分派。
// 视图模块（tasks/offline/cache/settings）按需 import()，加载后常驻（ES module 单例）。

import { api, connectSSE, toast, els } from './api.js';

// 全局状态（SSE 状态流驱动）
export const state = {
  configReady: true,
  missing: [],
  initError: '',
  tasks: [],
};

// 视图 init 时注册的处理回调（未加载的视图 → 安全丢弃该帧）
let onLogs = null;
let onOverview = null;
export function setHandlers(logs, overview) { onLogs = logs; onOverview = overview; }

let closeEvents = null;
let curView = '';
const mods = {};
let viewToken = 0;       // 切换请求序号：丢弃过期（快速连点导航不串台）

const views = {
  tasks:   { load: () => import('./tasks.js'),   init: 'initTasks',  stop: 'stopTasks' },
  offline: { load: () => import('./offline.js'), init: 'initOffline' },
  cache:   { load: () => import('./cache.js'),   init: 'initCache' },
  settings:{ load: () => import('./settings.js'), init: 'initSettings' },
};

// ──── 主题：固定跟随设备（:root { color-scheme: light dark } 已让 light-dark() 随系统切换，无需切换按钮） ────

// ──── 视图切换 ────

function currentView() {
  const h = decodeURIComponent(location.hash.slice(1));
  return views[h] ? h : 'tasks';
}
function setNav(name) {
  els('nav').querySelectorAll('.nav-item').forEach((b) => b.classList.toggle('active', b.dataset.view === name));
}
function showView(name) {
  document.querySelectorAll('.view').forEach((v) => { v.hidden = true; });
  els('view-' + name).hidden = false;
}

async function switchView(name) {
  if (!views[name]) name = 'tasks';
  if (curView === name && mods[name]) return;
  const token = ++viewToken;
  try {
    mods[name] ??= await views[name].load();   // 加载后常驻
    if (token !== viewToken) return;           // 过期切换丢弃
    if (curView && mods[curView]) mods[curView][views[curView].stop]?.();
    curView = name;
    if (location.hash.slice(1) !== name) location.hash = name;
    setNav(name);
    showView(name);
    mods[name][views[name].init]();
  } catch {
    toast('视图加载失败', 'err');
  }
}

// ──── 登录 / 概览 ────

export function showLogin() {
  els('login').hidden = false;
  els('app-view').hidden = true;
  if (closeEvents) { closeEvents(); closeEvents = null; }
}
function showApp() {
  els('login').hidden = true;
  els('app-view').hidden = false;
}

export async function refreshOverview() {
  try { applyOverview(await api('/api/overview')); } catch {}
}
function applyOverview(o) {
  state.configReady = o.config_ready;
  state.missing = o.missing || [];
  state.initError = o.init_error || '';
  state.tasks = o.tasks || [];
  onOverview?.();   // renderTasks（视图注册）
}

function startEvents() {
  closeEvents = connectSSE('/api/events', {
    onMessage: (en) => {
      if (!en) return;
      if (en.type === 'logs') onLogs?.(en);
      else if (en.type === 'overview' || Array.isArray(en.tasks)) applyOverview(en);
    },
  });
}

async function boot() {
  let me;
  try { me = await api('/api/me'); } catch { showLogin(); return; }
  if (me.auth_required && !me.logged_in) { showLogin(); return; }
  els('logout-btn').hidden = false;
  showApp();
  await switchView(currentView());   // 先加载首屏视图再开 SSE，避免首帧丢失
  startEvents();
  api('/api/version').then((v) => {
    const ver = document.querySelector('.brand-ver');
    if (ver && v.version) ver.textContent = 'v' + v.version;
  }).catch(() => {});
}

function bindStatic() {
  els('login-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    try {
      await api('/api/login', {
        method: 'POST',
        body: JSON.stringify({ username: fd.get('username'), password: fd.get('password') }),
      });
      boot();
    } catch (err) { els('login-form').querySelector('.muted').textContent = err.message; }
  });
  els('logout-btn').addEventListener('click', async () => {
    try { await api('/api/logout', { method: 'POST' }); } catch {}
    showLogin();
  });
  els('nav').addEventListener('click', (e) => {
    const btn = e.target.closest('.nav-item');
    if (btn?.dataset.view) switchView(btn.dataset.view);
  });
  window.addEventListener('hashchange', () => switchView(currentView()));
}

bindStatic();
boot();
