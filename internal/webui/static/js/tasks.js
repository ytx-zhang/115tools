// tasks.js —— 任务中心：横幅、任务卡片、编辑弹窗、执行历史弹窗。

import { api, el, fmtTime, fmtBytes, fmtDuration, toast, svgIcon, btnWithIcon } from './api.js';
import { state } from './main.js';

let taskConfigs = []; // 完整任务配置（含目录/选项）
let bound = false;
let taskCards = new Map(); // id -> { card, badge, fill, nums, runBtn }；SSE 推送时增量更新，不重建 DOM 防跳动

const triggerLabel = { manual: '手动', cron: '定时', watch: '监听', init: '启动' };
const stateLabel = { running: '运行中', success: '成功', canceled: '已取消', failed: '失败' };
const stateCls = { running: 'run', success: 'ok', canceled: 'warn', failed: 'err' };
const kindLabel = { push: '本地同步', pull: '云端同步' };

// ──── 生命周期 ────

export function initTasks() {
  if (!bound) {
    bindOnce();
    bound = true;
  }
  loadTasks();
}

export function stopTasks() {}

async function loadTasks() {
  try {
    const data = await api('/api/tasks');
    taskConfigs = data.tasks || [];
  } catch { taskConfigs = []; }
  // 全量重建：必须先清空 DOM 再清映射表，否则旧卡片残留造成重复显示
  const grid = document.getElementById('task-grid');
  if (grid) grid.innerHTML = '';
  taskCards.clear();
  renderTasks();
}

// ──── 渲染 ────

export function renderTasks() {
  renderBanners();
  renderGrid();
  renderSystemLogs();
}

// renderBanners 顶部横幅：仅配置状态类（配置不完整 / 初始化失败）。
// 系统级错误/警告日志只进下方「错误警告日志」卡片（renderSystemLogs），顶部不再重复显示。
function renderBanners() {
  const box = document.getElementById('banners');
  box.innerHTML = '';
  if (!state.configReady) {
    box.appendChild(bannerEl('warn', '配置不完整，同步未启动，请到「设置」补齐：' + state.missing.join('、')));
  }
  if (state.initError) {
    box.appendChild(bannerEl('error', '初始化失败：' + state.initError));
  }
}

function bannerEl(cls, text) {
  const d = el('div', 'banner ' + cls);
  d.append(svgIcon('#i-warn'), document.createTextNode(text));
  return d;
}

function renderGrid() {
  const grid = document.getElementById('task-grid');
  const empty = document.getElementById('task-empty');
  const runtime = {};
  state.tasks.forEach((t) => { runtime[t.id] = t; });

  // 移除已删除的任务卡片
  for (const [id, e] of taskCards) {
    if (!taskConfigs.some((t) => t.id === id)) {
      e.card.remove();
      taskCards.delete(id);
    }
  }
  // 新增 / 增量更新（SSE 推送只改状态，不重建 DOM → 卡片不跳动）
  taskConfigs.forEach((t) => {
    const rt = runtime[t.id];
    let e = taskCards.get(t.id);
    if (e) updateCard(e, rt);
    else {
      e = createCard(t, rt);
      taskCards.set(t.id, e);
      grid.appendChild(e.card);
    }
  });
  empty.hidden = taskConfigs.length > 0;
}

// updateCard 只更新卡片动态部分（状态徽章/进度/数字/运行按钮）。
function updateCard(e, rt) {
  const running = rt?.running;
  const total = rt?.total || 0;
  const done = rt?.completed || 0;
  e.badge.className = 'badge' + (running ? ' run' : '');
  e.badge.textContent = running ? '运行中' : '空闲';
  e.fill.style.width = total ? Math.min(100, done / total * 100) + '%' : '0%';
  e.nums.textContent = `${done} / ${total}`;
  // 按钮文本就地更新（保留 SVG 图标；不可用 textContent 赋值——会清空子节点导致 use 丢失）
  const label = running ? '停止' : '执行';
  const last = e.runBtn.lastChild;
  if (last && last.nodeType === Node.TEXT_NODE) last.textContent = label;
  else e.runBtn.appendChild(document.createTextNode(label));
  e.runBtn.className = 'btn sm ' + (running ? 'danger' : 'primary');
  e.runBtn.dataset.action = running ? 'stop' : 'start';
  const use = e.runBtn.querySelector('use');
  const icon = running ? '#i-stop' : '#i-play';
  use.setAttribute('href', icon);
  use.setAttributeNS('http://www.w3.org/1999/xlink', 'xlink:href', icon);
}

// renderSystemLogs 渲染系统级错误/警告日志卡片（独立于顶部配置横幅）。
function renderSystemLogs() {
  const box = document.getElementById('system-log-list');
  if (!box) return;
  box.innerHTML = '';
  if (!state.banners.length) {
    box.appendChild(el('div', 'muted empty', '暂无系统错误/警告'));
    return;
  }
  state.banners.forEach((b) => {
    const line = el('div', 'log-line lv-' + (b.level === 'ERROR' ? 'error' : 'warn'));
    line.append(
      el('span', 't', fmtTime(b.time)),
      el('span', 'lv', b.level),
    );
    line.appendChild(document.createTextNode(' ' + (b.msg || '') + (b.attrs ? '  ' + b.attrs : '')));
    box.appendChild(line);
  });
}

// createCard 创建任务卡片，返回可增量更新的元素引用。
function createCard(t, rt) {
  const card = el('div', 'task-card k-' + (t.kind === 'pull' ? 'pull' : 'push'));
  card.dataset.id = t.id;

  // 头部：名称 + 类型徽章 + 启用开关
  const head = el('div', 'tc-head');
  const name = el('div', 'tc-name', t.name);
  name.title = t.name;
  const kind = el('span', 'badge ' + t.kind, kindLabel[t.kind] || t.kind);
  const sw = el('label', 'switch');
  const chk = document.createElement('input');
  chk.type = 'checkbox';
  chk.checked = !!t.enabled;
  chk.dataset.toggle = t.id;
  sw.append(chk, el('i'));
  head.append(name, kind, sw);

  // 目录（SVG 图标 + 路径）
  const dirs = el('div', 'tc-dirs');
  const dirSpan = (icon, label, dir) => {
    const s = el('span');
    s.append(svgIcon(icon), document.createTextNode(`${label} ${dir || '—'}`));
    s.title = `${label} ${dir || ''}`;
    return s;
  };
  dirs.append(dirSpan('#i-drive', '本地', t.local_dir));
  dirs.append(dirSpan('#i-cloud', '云端', t.cloud_dir));

  // 状态
  const running = rt?.running;
  const total = rt?.total || 0;
  const done = rt?.completed || 0;
  const status = el('div', 'tc-status');
  const badge = el('span', 'badge ' + (running ? 'run' : ''), running ? '运行中' : '空闲');
  const bar = el('div', 'progress');
  const fill = el('i');
  fill.style.width = total ? Math.min(100, done / total * 100) + '%' : '0%';
  bar.appendChild(fill);
  const nums = el('span', 'tc-nums', `${done} / ${total}`);
  status.append(badge, bar, nums);

  // 操作（SVG 图标按钮）
  const actions = el('div', 'tc-actions');
  const runBtn = btnWithIcon('btn sm ' + (running ? 'danger' : 'primary'), running ? '停止' : '执行',
    running ? '#i-stop' : '#i-play', { action: running ? 'stop' : 'start', id: t.id });
  const logBtn = btnWithIcon('btn sm', '日志', '#i-clock', { action: 'history', id: t.id });
  const editBtn = btnWithIcon('btn sm', '编辑', '#i-edit', { action: 'edit', id: t.id });
  const delBtn = btnWithIcon('btn sm danger', '删除', '#i-trash', { action: 'delete', id: t.id });
  actions.append(runBtn, logBtn, editBtn, delBtn);

  card.append(head, dirs, status, actions);
  return { card, badge, fill, nums, runBtn };
}

// ──── 事件绑定（一次性，事件委托） ────

function bindOnce() {
  document.getElementById('task-grid').addEventListener('click', async (e) => {
    const btn = e.target.closest('button[data-action]');
    if (!btn) return;
    const { action, id } = btn.dataset;
    if (action === 'start') { await startTask(id); }
    else if (action === 'stop') { await stopTask(id); }
    else if (action === 'history') { openHistory(id); }
    else if (action === 'edit') { openDialog(id); }
    else if (action === 'delete') { await deleteTask(id); }
  });

  // 启用开关（change 委托）
  document.getElementById('task-grid').addEventListener('change', async (e) => {
    const chk = e.target.closest('input[data-toggle]');
    if (!chk) return;
    const t = taskConfigs.find((x) => x.id === chk.dataset.toggle);
    if (!t) return;
    t.enabled = chk.checked;
    try { await saveTask(t); toast('已' + (chk.checked ? '启用' : '停用'), 'ok'); }
    catch (err) { chk.checked = !chk.checked; toast(err.message, 'err'); }
  });

  document.getElementById('task-new').addEventListener('click', () => openDialog(null));
  document.getElementById('empty-new').addEventListener('click', () => openDialog(null));

  // 任务编辑弹窗
  const dlg = document.getElementById('task-dialog');
  const form = document.getElementById('task-form');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const id = form.dataset.id || '';
    const t = formToTask(form, id);
    try {
      await saveTask(t);
      dlg.close();
      toast('任务已保存', 'ok');
      loadTasks();
    } catch (err) { toast(err.message, 'err'); }
  });
  // 类型切换 → 显隐方向组
  form.elements.kind.addEventListener('change', () => updateDlgGroups(form));
  // rescan_then_pull 勾选 → 显隐 pull 组
  form.elements.rescan_then_pull.addEventListener('change', () => updateDlgGroups(form));
  document.querySelectorAll('[data-close]').forEach((b) =>
    b.addEventListener('click', () => document.getElementById(b.dataset.close).close()));

  // 清空系统级错误/警告日志
  document.getElementById('system-logs-clear').addEventListener('click', async () => {
    try {
      await api('/api/banners/clear', { method: 'POST' });
      state.banners = [];
      renderTasks();
      toast('已清空错误警告日志');
    } catch (err) { toast(err.message, 'err'); }
  });

  // 清空当前任务的执行历史与明细日志
  document.getElementById('history-clear').addEventListener('click', async () => {
    if (!curRunTask) return;
    if (!confirm('确认清空该任务的全部执行历史与日志？')) return;
    try {
      await api(`/api/tasks/${curRunTask}/runs`, { method: 'DELETE' });
      toast('已清空该任务日志', 'ok');
      openHistory(curRunTask);
    } catch (err) { toast(err.message, 'err'); }
  });
}

async function startTask(id) {
  try { await api(`/api/tasks/${id}/start`, { method: 'POST' }); toast('任务已启动', 'ok'); }
  catch (err) { toast(err.message, 'err'); }
}
async function stopTask(id) {
  try { await api(`/api/tasks/${id}/stop`, { method: 'POST' }); toast('已发送停止指令'); }
  catch (err) { toast(err.message, 'err'); }
}
async function deleteTask(id) {
  if (!confirm('确认删除该任务？')) return;
  const purge = confirm('是否同时清理该任务的本地索引记录？\n（确定=清理，取消=保留索引）');
  try {
    await api(`/api/tasks/${id}${purge ? '?purge=1' : ''}`, { method: 'DELETE' });
    toast('任务已删除', 'ok');
    loadTasks();
  } catch (err) { toast(err.message, 'err'); }
}
async function saveTask(t) {
  if (t.id) return api(`/api/tasks/${t.id}`, { method: 'PUT', body: JSON.stringify(t) });
  return api('/api/tasks', { method: 'POST', body: JSON.stringify(t) });
}

// ──── 编辑弹窗 ────

function updateDlgGroups(form) {
  const isPush = form.elements.kind.value === 'push';
  const push = form.querySelector('[data-group="push"]');
  const attach = form.querySelector('[data-group="attach"]');
  const pull = form.querySelector('[data-group="pull"]');
  push.hidden = !isPush;
  // 附带云端扫描：仅 push 任务勾选「全量扫描后扫描云端」后显示（时机与间隔随全量扫描走）
  attach.hidden = !(isPush && form.elements.rescan_then_pull.checked);
  pull.hidden = isPush;
}

function openDialog(id) {
  const form = document.getElementById('task-form');
  form.reset();
  document.getElementById('task-dialog-title').textContent = id ? '编辑任务' : '新建任务';
  form.dataset.id = id || '';
  if (id) {
    const t = taskConfigs.find((x) => x.id === id);
    if (t) taskToForm(form, t);
  }
  updateDlgGroups(form);
  document.getElementById('task-dialog').showModal();
}

function taskToForm(form, t) {
  const e = form.elements;
  e.name.value = t.name || '';
  e.kind.value = t.kind || 'push';
  e.local_dir.value = t.local_dir || '';
  e.cloud_dir.value = t.cloud_dir || '';
  e.enabled.checked = !!t.enabled;
  e.watch_enabled.checked = !!t.watch?.enabled;
  e.debounce_minutes.value = t.watch?.quiet_minutes ?? 10;
  e.strm_now.checked = !!t.watch?.strm_now;
  e.video_now.checked = !!t.watch?.video_now;
  e.rescan_enabled.checked = !!t.rescan?.enabled;
  e.rescan_interval_hours.value = t.rescan?.interval_hours ?? 12;
  e.rescan_then_pull.checked = !!t.rescan_then_pull;
  e.to_strm.checked = !!t.to_strm;
  e.to_cache.checked = !!t.to_cache;
  // 附带云端扫描组（仅 push 任务展示）
  e.drop_redundant.checked = !!t.drop_redundant;
  e.attach_to_strm.checked = t.kind !== 'pull' ? !!t.pull_to_strm : false;
  // 云端方向组（仅 pull 任务展示）
  e.pull_cron_enabled.checked = !!t.pull_cron?.enabled;
  e.pull_cron_interval_hours.value = t.pull_cron?.interval_hours ?? 12;
  e.pull_to_strm.checked = !!t.pull_to_strm;
  e.archive_to_temp.checked = !!t.archive_to_temp;
}

// formToTask 按任务类型组装：fetch_missing 恒为 true（云端扫描的本职，不暴露开关）；
// drop_redundant 仅 push 任务的附带扫描有意义（pull 任务无法判定冗余，恒 false，引擎层同样归一）。
function formToTask(form, id) {
  const e = form.elements;
  const num = (v) => Math.max(0, parseInt(v, 10) || 0);
  const isPush = e.kind.value === 'push';
  const t = {
    id,
    name: e.name.value.trim(),
    kind: e.kind.value,
    enabled: e.enabled.checked,
    local_dir: e.local_dir.value.trim(),
    cloud_dir: e.cloud_dir.value.trim(),
    watch: { enabled: e.watch_enabled.checked, quiet_minutes: num(e.debounce_minutes.value), strm_now: e.strm_now.checked, video_now: e.video_now.checked },
    rescan: { enabled: e.rescan_enabled.checked, interval_hours: num(e.rescan_interval_hours.value) },
    rescan_then_pull: e.rescan_then_pull.checked,
    to_strm: e.to_strm.checked,
    to_cache: e.to_cache.checked,
    pull_cron: { enabled: false, interval_hours: 12 },
    pull_to_strm: false,
    drop_redundant: false,
    fetch_missing: true,
    archive_to_temp: false,
  };
  if (isPush) {
    t.pull_to_strm = e.attach_to_strm.checked;
    t.drop_redundant = e.drop_redundant.checked;
  } else {
    t.pull_cron = { enabled: e.pull_cron_enabled.checked, interval_hours: num(e.pull_cron_interval_hours.value) };
    t.pull_to_strm = e.pull_to_strm.checked;
    t.archive_to_temp = e.archive_to_temp.checked;
  }
  return t;
}

// ──── 执行历史弹窗 ────

let curRunTask = null;

async function openHistory(id) {
  curRunTask = id;
  const t = taskConfigs.find((x) => x.id === id);
  document.getElementById('history-dialog-title').textContent = (t?.name || id) + ' · 执行历史';
  document.getElementById('run-log').hidden = true;
  const list = document.getElementById('history-list');
  list.innerHTML = '<div class="muted">加载中…</div>';
  document.getElementById('history-dialog').showModal();
  try {
    const data = await api(`/api/tasks/${id}/runs?limit=100`);
    renderRuns(data.runs || []);
  } catch (err) {
    list.innerHTML = '';
    list.appendChild(el('div', 'muted', '加载失败：' + err.message));
  }
}

function renderRuns(runs) {
  const list = document.getElementById('history-list');
  list.innerHTML = '';
  if (!runs.length) { list.appendChild(el('div', 'muted', '暂无执行记录')); return; }
  runs.forEach((r) => {
    const row = el('div', 'hist-row');
    row.dataset.seq = r.seq;
    const badge = el('span', 'badge ' + (stateCls[r.state] || ''), stateLabel[r.state] || r.state);
    const trig = el('span', 'badge', (triggerLabel[r.trigger] || r.trigger) +
      (r.direction === 'pull' ? ' · 云端' : ' · 本地'));
    const time = el('span', 'hr-time', fmtTime(r.started_at));
    const stats = el('span', 'hr-stats',
      `耗时 ${fmtDuration(r.duration_ms)}` +
      (r.counters?.uploaded ? ` · 上传 ${r.counters.uploaded}` : '') +
      (r.counters?.downloaded ? ` · 下载 ${r.counters.downloaded}` : '') +
      (r.counters?.strm_generated ? ` · STRM ${r.counters.strm_generated}` : '') +
      (r.counters?.deleted ? ` · 删除 ${r.counters.deleted}` : ''));
    row.append(badge, trig, time, stats);
    row.addEventListener('click', () => openRunLog(r));
    list.appendChild(row);
  });
}

async function openRunLog(run) {
  const box = document.getElementById('run-log');
  box.hidden = false;
  box.innerHTML = '<div class="muted">加载日志…</div>';
  if (run.error) {
    box.appendChild(el('div', 'log-line lv-error', '⚠ ' + run.error));
  }
  try {
    const data = await api(`/api/tasks/${curRunTask}/runs/${run.seq}/logs`);
    renderLogs(data.logs || []);
  } catch (err) {
    box.innerHTML = '';
    box.appendChild(el('div', 'muted', '加载失败：' + err.message));
  }
}

function renderLogs(logs) {
  const box = document.getElementById('run-log');
  box.innerHTML = '';
  if (!logs.length) { box.appendChild(el('div', 'muted', '（无明细日志）')); return; }
  logs.forEach((l) => {
    const line = el('div', 'log-line lv-' + l.level.toLowerCase());
    line.append(
      el('span', 't', fmtTime(l.time)),
      el('span', 'lv', l.level),
    );
    line.appendChild(document.createTextNode(' ' + l.msg + (l.attrs ? '  ' + l.attrs : '')));
    box.appendChild(line);
  });
  box.scrollTop = box.scrollHeight;
}
