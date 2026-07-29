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

function isValidView(name) {
  return Object.prototype.hasOwnProperty.call(views, name);
}

// 仅负责 DOM 显隐、导航高亮与视图生命周期（不读写 location.hash）
function activate(name) {
  if (!isValidView(name)) name = 'dashboard';
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

// 唯一的“写路由”入口：写 location.hash，由 hashchange 触发 activate，避免循环
function navigate(name) {
  if (!isValidView(name)) name = 'dashboard';
  const target = '#' + name;
  if (location.hash === target) activate(name);
  else location.hash = target;
}

function hashToView() {
  const h = location.hash.replace(/^#/, '');
  return isValidView(h) ? h : 'dashboard';
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
  activate(hashToView());
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

// 导航切换：点击导航走 navigate（写 hash），由 hashchange 触发真正的视图切换
$('#nav').addEventListener('click', e => {
  const btn = e.target.closest('[data-view]');
  if (btn) navigate(btn.dataset.view);
});

// URL hash 变化（含手动输入 #xxx、前进/后退、刷新）：同步切换视图
window.addEventListener('hashchange', () => activate(hashToView()));

// 任意接口 401 → 回到登录页
window.addEventListener('auth:required', showLogin);

boot();
