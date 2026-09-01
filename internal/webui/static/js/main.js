// main.js —— 应用入口：登录、视图切换（hash 记忆）、全局 SSE 状态流。

import { api, connectSSE } from './api.js';
import { initTasks, stopTasks, renderTasks } from './tasks.js';
import { initOffline } from './offline.js';
import { initCache } from './cache.js';
import { initSettings } from './settings.js';

// 全局状态（SSE 状态流驱动）
export const state = {
  configReady: true,
  missing: [],
  initError: '',
  tasks: [],     // [{id,name,type,running,completed,total}]
};

let closeEvents = null;
let curView = ''; // 空 = 尚未初始化任何视图（保证首次 switchView 必然执行 init）

// ──── 主题（亮 / 暗 / 自动） ────
const THEME_KEY = '115tools-theme';
const THEME_ICON = { light: '#i-sun', dark: '#i-moon', auto: '#i-auto' };

function getTheme() {
  const t = localStorage.getItem(THEME_KEY);
  return t === 'light' || t === 'dark' ? t : 'auto';
}

function applyTheme(theme) {
  const root = document.documentElement;
  if (theme === 'auto') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', theme);
  localStorage.setItem(THEME_KEY, theme);
  document.querySelectorAll('.theme-toggle .ic use').forEach((u) => { u.setAttribute('href', THEME_ICON[theme]); });
}

function bindTheme() {
  applyTheme(getTheme()); // 首屏同步（防闪烁：样式内联提前设置）
  document.querySelectorAll('.theme-toggle').forEach((b) => {
    b.addEventListener('click', () => {
      const cur = getTheme();
      applyTheme(cur === 'auto' ? 'light' : cur === 'light' ? 'dark' : 'auto');
    });
  });
  // 仅「自动」模式跟随系统主题变化
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
    if (getTheme() === 'auto') applyTheme('auto');
  });
}

const views = {
  tasks: { init: initTasks, stop: stopTasks },
  offline: { init: initOffline },
  cache: { init: initCache },
  settings: { init: initSettings },
};

// currentView 从 URL hash 解析初始视图（#offline 等），非法/缺省回落任务中心。
function currentView() {
  const h = decodeURIComponent(location.hash.slice(1));
  return views[h] ? h : 'tasks';
}

export function showLogin() {
  document.getElementById('login').hidden = false;
  document.getElementById('app-view').hidden = true;
  if (closeEvents) { closeEvents(); closeEvents = null; }
}

function showApp() {
  document.getElementById('login').hidden = true;
  document.getElementById('app-view').hidden = false;
}

// switchView 切换视图：写 URL hash（刷新/前进后退后停留在当前 tab），并执行目标视图 init。
function switchView(name) {
  if (!views[name]) name = 'tasks';
  if (curView === name) return;
  views[curView]?.stop?.();
  curView = name;
  if (location.hash.slice(1) !== name) location.hash = name;
  document.querySelectorAll('#nav .nav-item').forEach((b) =>
    b.classList.toggle('active', b.dataset.view === name));
  document.querySelectorAll('.view').forEach((v) => { v.hidden = true; });
  document.getElementById('view-' + name).hidden = false;
  views[name].init();
}

// refreshOverview 主动拉取一次状态快照并刷新渲染（SSE 推送之外的兜底路径）。
export async function refreshOverview() {
  try {
    const o = await api('/api/overview');
    applyOverview(o);
  } catch { /* 静默：SSE 仍会推送 */ }
}

function applyOverview(o) {
  state.configReady = o.config_ready;
  state.missing = o.missing || [];
  state.initError = o.init_error || '';
  state.tasks = o.tasks || [];
  renderTasks();
}

// 全局 SSE 状态流：只推 overview（配置就绪 / 任务状态）。程序日志已回归 docker logs。
function startEvents() {
  closeEvents = connectSSE('/api/events', {
    onMessage: (en) => {
      // 后端 handleEvents 直接推送 overview 结构（无 type 包装），直接消费
      if (en && Array.isArray(en.tasks)) applyOverview(en);
    },
  });
}

async function boot() {
  let me;
  try { me = await api('/api/me'); } catch { showLogin(); return; }
  if (me.auth_required && !me.logged_in) { showLogin(); return; }

  document.getElementById('logout-btn').hidden = false;
  showApp();
  switchView(currentView()); // curView 初始为空，首次必然执行目标视图的 init
  startEvents();
  api('/api/version').then((v) => {
    const ver = document.querySelector('.brand-ver');
    if (ver && v.version) ver.textContent = 'v' + v.version;
  }).catch(() => {});
}

function bindStatic() {
  // 登录表单
  document.getElementById('login-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    try {
      await api('/api/login', {
        method: 'POST',
        body: JSON.stringify({ username: fd.get('username'), password: fd.get('password') }),
      });
      boot();
    } catch (err) {
      document.getElementById('login-form').querySelector('.muted').textContent = err.message;
    }
  });
  // 登出
  document.getElementById('logout-btn').addEventListener('click', async () => {
    try { await api('/api/logout', { method: 'POST' }); } catch {}
    showLogin();
  });
  // 导航
  document.getElementById('nav').addEventListener('click', (e) => {
    const btn = e.target.closest('.nav-item');
    if (btn && btn.dataset.view) switchView(btn.dataset.view);
  });
  // 浏览器前进/后退（hash 变化）跟随切换；同视图由 switchView 内部去重
  window.addEventListener('hashchange', () => switchView(currentView()));
}

bindTheme();
bindStatic();
boot();
