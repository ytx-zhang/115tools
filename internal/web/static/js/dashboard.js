// dashboard.js —— 任务状态卡与横幅。日志流已抽到 logs.js（经 onStatus 回调把状态帧交还此处）。
// 状态卡用「普通状态对象 + 命令式 render 函数」更新 DOM。
import { api, toast } from './api.js';
import { initLogs, stopLogs, onStatus } from './logs.js';

const startText = { sync: '开始同步', strm: '开始生成', local: '开始扫描' };

// dash 是普通状态对象（无响应式代理），数据变化后显式调用 renderStatus 刷新 DOM。
export const dash = {
  configReady: true,
  missing: [],
  initError: '',
  sync: { running: false, done: 0, total: 0, ready: false, badgeText: '未就绪', badgeCls: 'badge warn', barWidth: '0%', btnText: '开始同步', btnCls: 'btn primary' },
  strm: { running: false, done: 0, total: 0, ready: false, badgeText: '未就绪', badgeCls: 'badge warn', barWidth: '0%', btnText: '开始生成', btnCls: 'btn primary' },
  local: { running: false, done: 0, total: 0, ready: false, badgeText: '未就绪', badgeCls: 'badge warn', barWidth: '0%', btnText: '开始扫描', btnCls: 'btn primary' },

  setStatus(name, st) {
    const s = dash[name];
    s.running = !!st?.running;
    s.ready = !!st;
    if (!st) { s.badgeText = '未就绪'; s.badgeCls = 'badge warn'; }
    else if (s.running) { s.badgeText = '运行中'; s.badgeCls = 'badge run'; }
    else { s.badgeText = '空闲'; s.badgeCls = 'badge'; }
    s.btnText = s.running ? '停 止' : startText[name];
    s.btnCls = s.running ? 'btn danger' : 'btn primary';
    // 三个任务卡统一更新进度条
    s.done = st?.completed || 0;
    s.total = st?.total || 0;
    s.barWidth = s.total ? `${Math.min(100, s.done / s.total * 100)}%` : '0%';
  },

  async toggle(name) {
    if (dash[name].running) {
      await api(`/api/task/${name}`, { method: 'DELETE' });
      toast('已发送停止指令');
    } else {
      await api(`/api/task/${name}`, { method: 'POST' });
      toast('任务已启动', 'ok');
    }
  },
};

// 按任务维度的启停请求去重：请求返回前忽略重复点击，避免连点产生多条通知
const toggling = {};
export async function toggleTask(name) {
  if (toggling[name]) return;
  toggling[name] = true;
  try { await dash.toggle(name); } finally { toggling[name] = false; }
}

export function initDashboard() {
  // 把日志流里的状态帧映射回任务卡（logs.js 通过 onStatus 回调交还，避免循环依赖）。
  onStatus(en => {
    dash.configReady = en.status.config_ready;
    dash.missing = en.status.missing || [];
    dash.initError = en.status.init_error || '';
    dash.setStatus('sync', en.status.sync);
    dash.setStatus('strm', en.status.strm);
    dash.setStatus('local', en.status.local);
    renderStatus();
  });
  bindOnce();
  renderStatus();
  initLogs();
}

export function stopDashboard() {
  stopLogs();
}

// ──── 状态渲染（banner + 任务卡）────

function renderStatus() {
  renderBanners();
  renderTaskCard('sync');
  renderTaskCard('strm');
  renderTaskCard('local');
}

function renderBanners() {
  // 两条 banner 结构对称：{container, 显示条件, 文本元素, 文本}，循环收敛
  const defs = [
    { id: 'config-banner', hidden: dash.configReady, txtId: 'config-banner-missing', txt: dash.missing.join('、') },
    { id: 'init-error-banner', hidden: !dash.initError, txtId: 'init-error-text', txt: dash.initError },
  ];
  for (const d of defs) {
    const el = document.getElementById(d.id);
    if (el) el.hidden = d.hidden;
    const txt = document.getElementById(d.txtId);
    if (txt) txt.textContent = d.txt;
  }
}

function renderTaskCard(name) {
  const s = dash[name];
  const badge = document.getElementById(`${name}-badge`);
  if (badge) {
    badge.textContent = s.badgeText;
    badge.className = s.badgeCls;
  }
  const done = document.getElementById(`${name}-done`);
  if (done) done.textContent = s.done;
  const total = document.getElementById(`${name}-total`);
  if (total) total.textContent = s.total;
  const bar = document.getElementById(`${name}-bar`);
  if (bar) bar.style.width = s.barWidth;
  const btn = document.getElementById(`${name}-btn`);
  if (btn) {
    btn.textContent = s.btnText;
    btn.className = s.btnCls;
    btn.disabled = !s.ready;
  }
}

// ──── 事件绑定（一次性，stop/init 生命周期不重复绑定）────
let bound = false;
function bindOnce() {
  if (bound) return;
  bound = true;

  const sync = document.getElementById('sync-btn');
  if (sync) sync.addEventListener('click', () => toggleTask('sync'));
  const strm = document.getElementById('strm-btn');
  if (strm) strm.addEventListener('click', () => toggleTask('strm'));
  const local = document.getElementById('local-btn');
  if (local) local.addEventListener('click', () => toggleTask('local'));
}
