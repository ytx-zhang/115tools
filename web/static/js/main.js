// main.js —— 应用入口：会话检测、登录、视图路由
import { api, toast } from './api.js';
import { initDashboard, stopDashboard } from './dashboard.js';
import { initOffline, stopOffline } from './offline.js';
import { initSettings } from './settings.js';

const $ = sel => document.querySelector(sel);

const views = {
  dashboard: { el: () => $('#view-dashboard'), enter: initDashboard, leave: stopDashboard },
  offline: { el: () => $('#view-offline'), enter: initOffline, leave: stopOffline },
  settings: { el: () => $('#view-settings'), enter: initSettings, leave: () => { } },
};
let current = null;

function switchView(name) {
  if (current === name) return;
  if (current) {
    views[current].leave();
    views[current].el().hidden = true;
  }
  current = name;
  views[name].el().hidden = false;
  views[name].enter();
  document.querySelectorAll('.nav-item[data-view]').forEach(b =>
    b.classList.toggle('active', b.dataset.view === name));
}

function showLogin() {
  if (current) { views[current].leave(); current = null; }
  $('#app-view').hidden = true;
  $('#login-view').hidden = false;
}

function showApp(authRequired) {
  $('#login-view').hidden = true;
  $('#app-view').hidden = false;
  $('#logout-btn').hidden = !authRequired;
  document.querySelectorAll('.view').forEach(v => v.hidden = true);
  current = null;
  switchView('dashboard');
}

async function boot() {
  try {
    const me = await api('/api/me');
    if (me.logged_in) showApp(me.auth_required);
    else showLogin();
  } catch {
    showLogin();
  }
}

// 登录表单
$('#login-form').addEventListener('submit', async e => {
  e.preventDefault();
  const fd = new FormData(e.target);
  const btn = e.target.querySelector('button');
  btn.disabled = true;
  try {
    await api('/api/login', {
      method: 'POST',
      body: { username: fd.get('username'), password: fd.get('password') },
    });
    e.target.reset();
    showApp(true);
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
  }
});

// 退出登录
$('#logout-btn').addEventListener('click', async () => {
  try { await api('/api/logout', { method: 'POST' }); } catch { /* 忽略 */ }
  showLogin();
});

// 导航切换
$('#nav').addEventListener('click', e => {
  const btn = e.target.closest('[data-view]');
  if (btn) switchView(btn.dataset.view);
});

// 任意接口 401 → 回到登录页
window.addEventListener('auth:required', showLogin);

boot();
