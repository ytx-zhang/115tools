// main.js —— 应用入口：登录探活、视图路由（hash 显隐）、petite-vue 挂载、菜单切换。
import { api, toast } from './api.js';
import { dash, initDashboard, stopDashboard } from './dashboard.js';
import { off, initOffline, stopOffline } from './offline.js';
import { cfg } from './settings.js';

const ALLOWED = ['dashboard', 'offline', 'settings'];
const VIEWS = {
  dashboard: { show: () => { document.getElementById('view-dashboard').hidden = false; }, hide: () => { document.getElementById('view-dashboard').hidden = true; }, init: initDashboard, stop: stopDashboard },
  offline: { show: () => { document.getElementById('view-offline').hidden = false; }, hide: () => { document.getElementById('view-offline').hidden = true; }, init: initOffline, stop: stopOffline },
  settings: { show: () => { document.getElementById('view-settings').hidden = false; }, hide: () => { document.getElementById('view-settings').hidden = true; }, init: () => cfg.load(), stop: () => {} },
};

let current = null;

function showView(name) {
  if (current && VIEWS[current] && current !== name) VIEWS[current].stop();
  for (const v in VIEWS) {
    if (v === name) VIEWS[v].show();
    else VIEWS[v].hide();
  }
  // 菜单高亮
  document.querySelectorAll('[data-view]').forEach(b => b.classList.toggle('active', b.dataset.view === name));
  if (VIEWS[name] && VIEWS[name].init) VIEWS[name].init();
  if (location.hash !== '#' + name) history.replaceState(null, '', '#' + name);
  current = name;
}

function handleRoute() {
  const name = location.hash.replace('#', '') || 'dashboard';
  if (ALLOWED.includes(name)) showView(name);
  else showView('dashboard');
}

async function ping() {
  try {
    const me = await api('/api/me');
    // /api/me 不接 protect 中间件，永远 200，必须读返回体判定是否真已登录。
    return { ok: !!me?.logged_in, authRequired: !!me?.auth_required };
  } catch {
    return { ok: false, authRequired: false };
  }
}

function logout() {
  // 先清服务端 session（失败不阻塞，reload 后仍需登录）。
  fetch('/api/logout', {method: 'POST', credentials: 'same-origin'}).catch(() => {}).finally(() => {
    location.reload();
  });
}

// 监听登录失效（API 统一抛出 401 时广播）
window.addEventListener('auth:required', logout);
// 退出登录按钮
const lb = document.getElementById('logout-btn');
if (lb) lb.addEventListener('click', logout);

async function boot() {
  const res = await ping();
  if (!res.ok) {
    document.getElementById('login').hidden = false;
    document.getElementById('app-view').hidden = true;
    bindLogin();
    return;
  }
  document.getElementById('login').hidden = true;
  document.getElementById('app-view').hidden = false;
  // 仅启用登录验证时显示「退出登录」按钮（auth_required 为 true）。
  const lb = document.getElementById('logout-btn');
  if (lb) lb.hidden = !res.authRequired;

  // 托管三视图的声明式绑定（v-scope），命令式 SSE/轮询仍由各 view 模块驱动。
  PetiteVue.createApp({ dash, off, cfg }).mount('#app-view');

  // 导航按钮点击改 hash，由 hashchange 单一归口路由（不直接调 showView，避免双路径）。
  document.querySelectorAll('[data-view]').forEach(b => {
    b.addEventListener('click', () => { location.hash = '#' + b.dataset.view; });
  });
  window.addEventListener('hashchange', handleRoute);
  handleRoute();
}

function bindLogin() {
  const form = document.getElementById('login-form');
  if (!form) return;
  form.addEventListener('submit', async e => {
    e.preventDefault();
    const fd = new FormData(form);
    const btn = form.querySelector('[type=submit]');
    btn.disabled = true;
    try {
      const res = await api('/api/login', {
        method: 'POST',
        body: { username: fd.get('username'), password: fd.get('password') },
      });
      if (res.ok) {
        toast('登录成功', 'ok');
        location.reload();
      } else {
        toast(res.message || '登录失败', 'err');
        btn.disabled = false;
      }
    } catch (err) {
      toast(err.message || '登录失败', 'err');
      btn.disabled = false;
    }
  });
}

boot();
