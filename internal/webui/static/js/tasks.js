// tasks.js —— 任务中心：横幅、任务卡片、新建/编辑弹窗、预演面板、最近动态。

import { api, el, fmtTime, fmtDuration, toast, svgIcon, btnWithIcon } from './api.js';
import { state } from './main.js';

let taskConfigs = []; // 完整任务配置（含目录/开关）
let bound = false;
let taskCards = new Map(); // id -> { card, badge, fill, nums, runBtn, cur, meta }；SSE 推送时增量更新，不重建 DOM 防跳动

const stateLabel = { running: '运行中', success: '成功', canceled: '已取消', failed: '失败' };
const stateCls = { running: 'run', success: 'ok', canceled: 'warn', failed: 'err' };

// ──── 生命周期 ────

export function initTasks() {
  if (!bound) {
    bindOnce();
    bound = true;
  }
  loadTasks();
  loadActivity();
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
}

// renderBanners 顶部横幅：仅配置状态类（配置不完整 / 初始化失败）。
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

// cardState 由运行时快照推出卡片展示态：初始化中优先于排队中/运行中/空闲，此时禁止执行。
function cardState(rt) {
  if (rt?.initializing) return { cls: 'init', text: '初始化中', running: false, disabled: true };
  if (rt?.running) return { cls: 'run', text: '运行中', running: true, disabled: false };
  if (rt?.queued) return { cls: 'queued', text: '排队中', running: false, disabled: true };
  return { cls: '', text: '空闲', running: false, disabled: false };
}

// updateCard 只更新卡片动态部分（状态徽章/进度/数字/当前文件/按钮）。
function updateCard(e, rt) {
  const st = cardState(rt);
  const total = rt?.total || 0;
  const done = rt?.completed || 0;
  e.badge.className = ('badge ' + st.cls).trim();
  e.badge.textContent = st.text;
  // 三态：扫描中（running 且 total=0）→ 流动动画；执行中 → done/total 进度；空闲 → 0
  const scanning = st.running && !total;
  e.bar.className = 'progress' + (scanning ? ' ind' : '');
  e.fill.style.width = scanning ? '100%' : (total ? Math.min(100, done / total * 100) + '%' : '0%');
  e.nums.textContent = scanning ? '扫描中' : `${done} / ${total}`;
  e.cur.textContent = scanning ? '正在扫描…' : (st.running && rt?.current ? '正在处理 ' + rt.current : '');
  e.cur.title = e.cur.textContent;
  // 上次执行 / 下次定时实时刷新
  const metaTxt = [];
  if (rt?.last_run) metaTxt.push('上次 ' + fmtTime(rt.last_run));
  if (rt?.next_cron) metaTxt.push('下次 ' + fmtTime(rt.next_cron));
  e.meta.textContent = metaTxt.join(' · ');
  // 按钮文本就地更新（保留 SVG 图标；不可用 textContent 赋值——会清空子节点导致 use 丢失）
  const label = st.running ? '取消' : '执行';
  const last = e.runBtn.lastChild;
  if (last && last.nodeType === Node.TEXT_NODE) last.textContent = label;
  else e.runBtn.appendChild(document.createTextNode(label));
  e.runBtn.className = 'btn sm ' + (st.running ? 'danger' : 'primary');
  e.runBtn.dataset.action = st.running ? 'stop' : 'start';
  e.runBtn.querySelector('use').setAttribute('href', st.running ? '#i-stop' : '#i-play');
  // 初始化中：全部操作按钮禁用（运行时未就绪，预演/编辑/删除均无意义或存在竞态）
  e.card.querySelectorAll('.tc-actions button').forEach((b) => { b.disabled = st.disabled; });
}

// ──── 卡片 ────

function createCard(t, rt) {
  const card = el('div', 'task-card');
  card.dataset.id = t.id;

  // 头部：名称 + 方向徽章 + 启用开关
  const head = el('div', 'tc-head');
  const name = el('div', 'tc-name', t.name);
  name.title = t.name;
  const dirs = [t.upload && '上传', t.download && '下载'].filter(Boolean).join(' + ') || '未启用方向';
  const kind = el('span', 'badge k-badge', dirs);
  const sw = el('label', 'switch');
  const chk = document.createElement('input');
  chk.type = 'checkbox';
  chk.checked = !!t.enabled;
  chk.dataset.toggle = t.id;
  sw.append(chk, el('i'));
  head.append(name, kind, sw);

  // 目录（SVG 图标 + 路径）
  const dirsEl = el('div', 'tc-dirs');
  const dirSpan = (icon, label, dir) => {
    const s = el('span');
    s.append(svgIcon(icon), document.createTextNode(`${label} ${dir || '—'}`));
    s.title = `${label} ${dir || ''}`;
    return s;
  };
  dirsEl.append(dirSpan('#i-drive', '本地', t.local_dir));
  dirsEl.append(dirSpan('#i-cloud', '云端', t.cloud_dir));

  // 状态：徽章 + 进度条 + 数字 + 当前文件
  const st = cardState(rt);
  const total = rt?.total || 0;
  const done = rt?.completed || 0;
  const scanning = st.running && !total;
  const status = el('div', 'tc-status');
  const badge = el('span', ('badge ' + st.cls).trim(), st.text);
  const bar = el('div', 'progress' + (scanning ? ' ind' : ''));
  const fill = el('i');
  fill.style.width = scanning ? '100%' : (total ? Math.min(100, done / total * 100) + '%' : '0%');
  bar.appendChild(fill);
  const nums = el('span', 'tc-nums', scanning ? '扫描中' : `${done} / ${total}`);
  const cur = el('div', 'tc-cur', scanning ? '正在扫描…' : '');
  status.append(badge, bar, nums, cur);

  // 元信息：上次同步 / 下次定时
  const meta = el('div', 'tc-meta');
  const metaTxt = [];
  if (rt?.last_run) metaTxt.push('上次 ' + fmtTime(rt.last_run));
  if (rt?.next_cron) metaTxt.push('下次 ' + fmtTime(rt.next_cron));
  meta.textContent = metaTxt.join(' · ');

  // 操作（SVG 图标按钮）
  const actions = el('div', 'tc-actions');
  const runBtn = btnWithIcon('btn sm ' + (st.running ? 'danger' : 'primary'), st.running ? '停止' : '执行',
    st.running ? '#i-stop' : '#i-play', { action: st.running ? 'stop' : 'start', id: t.id });
  const dryBtn = btnWithIcon('btn sm', '预演', '#i-info', { action: 'dryrun', id: t.id });
  const logBtn = btnWithIcon('btn sm', '动态', '#i-clock', { action: 'history', id: t.id });
  const editBtn = btnWithIcon('btn sm', '编辑', '#i-edit', { action: 'edit', id: t.id });
  const delBtn = btnWithIcon('btn sm danger', '删除', '#i-trash', { action: 'delete', id: t.id });
  actions.append(runBtn, dryBtn, logBtn, editBtn, delBtn);
  if (st.disabled) actions.querySelectorAll('button').forEach((b) => { b.disabled = true; }); // 初始化中全部置灰

  card.append(head, dirsEl, status, meta, actions);
  return { card, badge, fill, nums, runBtn, cur, meta, bar };
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
    else if (action === 'dryrun') { openDryRun(id); }
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

  const form = document.getElementById('task-form');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const id = form.dataset.id || '';
    const t = formToTask(form, id);
    try {
      await saveTask(t);
      document.getElementById('task-dialog').close();
      toast('任务已保存', 'ok');
      loadTasks();
    } catch (err) { toast(err.message, 'err'); }
  });
  // 归档纯下载专用：编辑时手动勾/取消「上传」实时联动显隐
  form.elements.upload.addEventListener('change', () => applyArchiveRule(form));

  // 场景预设：点击卡片预填开关
  document.querySelectorAll('.preset-card').forEach((b) => {
    b.addEventListener('click', () => applyPreset(form, b.dataset.preset));
  });
  // 目录浏览
  document.getElementById('browse-local').addEventListener('click', () =>
    openFsBrowser(form.elements.local_dir));

  document.querySelectorAll('[data-close]').forEach((b) =>
    b.addEventListener('click', () => document.getElementById(b.dataset.close).close()));

  // ── 最近动态 ──
  document.getElementById('activity-refresh').addEventListener('click', loadActivity);
  document.getElementById('activity-clear').addEventListener('click', async () => {
    if (!confirm('确认清空全部动态记录？')) return;
    try {
      const ids = taskConfigs.map((t) => t.id);
      await Promise.all(ids.map((id) => api(`/api/activity?task_id=${encodeURIComponent(id)}`, { method: 'DELETE' })));
      toast('已清空动态记录', 'ok');
      loadActivity();
    } catch (err) { toast(err.message, 'err'); }
  });
  // 动态滚动到底自动加载下一页（容器滚动与页面滚动双保险，150ms 节流）
  const bindFeedScroll = (box, getCtrl) => {
    let timer = null;
    const check = () => {
      if (timer) return;
      timer = setTimeout(() => {
        timer = null;
        const ctrl = getCtrl();
        if (!ctrl) return;
        if (box.scrollHeight > box.clientHeight) {
          if (box.scrollTop + box.clientHeight >= box.scrollHeight - 40) ctrl.loadMore();
        } else {
          const r = box.getBoundingClientRect();
          if (r.bottom <= window.innerHeight + 60) ctrl.loadMore();
        }
      }, 150);
    };
    box.addEventListener('scroll', check);
    window.addEventListener('scroll', check);
  };
  bindFeedScroll(document.getElementById('activity-list'), () => activityCtrl);
  bindFeedScroll(document.getElementById('history-list'), () => historyCtrl);

  // ── 任务动态弹窗 ──
  document.getElementById('history-clear').addEventListener('click', async () => {
    if (!curTask) return;
    if (!confirm('确认清空该任务的全部动态记录？')) return;
    try {
      await api(`/api/activity?task_id=${encodeURIComponent(curTask)}`, { method: 'DELETE' });
      toast('已清空该任务动态', 'ok');
      openHistory(curTask);
    } catch (err) { toast(err.message, 'err'); }
  });

  // ── 预演弹窗 ──
  document.getElementById('dryrun-run').addEventListener('click', async () => {
    if (dryrunDanger > 0 && !confirm(`预演中包含 ${dryrunDanger} 个删除/归档等不可逆动作，确认执行？`)) return;
    if (dryrunDanger === 0 && !confirm('确认立即执行该任务？')) return;
    try {
      await api(`/api/tasks/${dryrunTask}/start`, { method: 'POST' });
      document.getElementById('dryrun-dialog').close();
      toast('任务已启动', 'ok');
    } catch (err) { toast(err.message, 'err'); }
  });

  // ── 目录浏览弹窗 ──
  bindFsBrowser();
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

// ──── 场景预设 ────

// applyArchiveRule 归档选项纯下载专用：开启上传时隐藏并取消勾选，否则显示。
// 与后端 normalize（upload 强制关 archive）双保险。
function applyArchiveRule(form) {
  const opt = document.getElementById('opt-archive');
  if (!opt) return;
  if (form.elements.upload.checked) {
    opt.hidden = true;
    form.elements.archive.checked = false;
  } else {
    opt.hidden = false;
  }
}

// applyPreset 预填开关并联动隐藏方向组：三选一，覆盖高级选项后滚动到基础信息。
// 搬上去隐藏「云端→本地」组、拉回本地隐藏「本地→云端」组；定时组始终显示。
function applyPreset(form, preset) {
  document.querySelectorAll('.preset-card').forEach((b) => b.classList.toggle('sel', b.dataset.preset === preset));
  const e = form.elements;
  // 先全关，再按场景开
  ['upload', 'watch', 'instant_now', 'download', 'archive', 'to_strm', 'to_strm_dl', 'to_cache', 'cron_enabled']
    .forEach((n) => { e[n].checked = false; });
  const common = { cron_enabled: true };
  const presets = {
    push: { upload: true, watch: true, instant_now: true, to_cache: true, to_strm: true, ...common },
    pull: { download: true, archive: true, to_strm_dl: true, ...common },
    both: { upload: true, watch: true, instant_now: true, to_cache: true, to_strm: true,
            download: true, to_strm_dl: true, ...common },
  };
  Object.entries(presets[preset] || {}).forEach(([k, v]) => { e[k].checked = v; });
  document.getElementById('adv-up').hidden = preset === 'pull';
  document.getElementById('adv-down').hidden = preset === 'push';
  applyArchiveRule(form); // push/both → archive 隐藏取消；pull → 显示且保持勾选
  document.getElementById('preset-box').hidden = true;
}

// ──── 新建 / 编辑弹窗 ────

function openDialog(id) {
  const form = document.getElementById('task-form');
  form.reset();
  document.querySelectorAll('.preset-card').forEach((b) => b.classList.remove('sel'));
  document.getElementById('task-dialog-title').textContent = id ? '编辑任务' : '新建任务';
  form.dataset.id = id || '';
  // 新建：显示场景预设并预选「搬上去」；编辑：隐藏预设，回填现有配置
  document.getElementById('preset-box').hidden = !!id;
  if (id) {
    // 编辑回填前恢复全部方向组可见（form.reset 不清 hidden）
    document.getElementById('adv-up').hidden = false;
    document.getElementById('adv-down').hidden = false;
    const t = taskConfigs.find((x) => x.id === id);
    if (t) taskToForm(form, t);
    applyArchiveRule(form); // 旧配置异常组合（upload+archive）回填后自动取消并隐藏
  } else {
    applyPreset(form, 'push');
    document.getElementById('preset-box').hidden = false;
  }
  document.getElementById('task-dialog').showModal();
}

function taskToForm(form, t) {
  const e = form.elements;
  e.name.value = t.name || '';
  e.enabled.checked = !!t.enabled;
  e.local_dir.value = t.local_dir || '';
  e.cloud_dir.value = t.cloud_dir || '';
  e.upload.checked = !!t.upload;
  e.watch.checked = !!t.watch;
  e.quiet_minutes.value = t.quiet_minutes ?? 10;
  e.instant_now.checked = !!t.instant_now;
  e.download.checked = !!t.download;
  e.to_strm_dl.checked = !!t.to_strm_dl;
  e.archive.checked = !!t.archive;
  e.to_strm.checked = !!t.to_strm;
  e.to_cache.checked = !!t.to_cache;
  e.cron_enabled.checked = !!t.cron?.enabled;
  e.cron_interval_hours.value = t.cron?.interval_hours ?? 12;
}

function formToTask(form, id) {
  const e = form.elements;
  const num = (v) => Math.max(0, parseInt(v, 10) || 0);
  return {
    id,
    name: e.name.value.trim(),
    enabled: e.enabled.checked,
    local_dir: e.local_dir.value.trim(),
    cloud_dir: e.cloud_dir.value.trim(),
    upload: e.upload.checked,
    watch: e.watch.checked,
    quiet_minutes: num(e.quiet_minutes.value),
    instant_now: e.instant_now.checked,
    download: e.download.checked,
    to_strm_dl: e.to_strm_dl.checked,
    archive: e.archive.checked,
    to_strm: e.to_strm.checked,
    to_cache: e.to_cache.checked,
    cron: { enabled: e.cron_enabled.checked, interval_hours: num(e.cron_interval_hours.value) },
  };
}

// ──── 目录浏览 ────

let fsTarget = null;
let fsCurrent = '/';

function bindFsBrowser() {
  document.getElementById('fs-up').addEventListener('click', () => loadFs(fsCurrent === '/' ? '/' : fsCurrent.replace(/\/[^/]*$/, '') || '/'));
  document.getElementById('fs-pick').addEventListener('click', () => {
    if (fsTarget) fsTarget.value = fsCurrent;
    document.getElementById('fs-dialog').close();
  });
}

function openFsBrowser(input) {
  fsTarget = input;
  document.getElementById('fs-dialog').showModal();
  loadFs(input.value || '/');
}

async function loadFs(path) {
  const box = document.getElementById('fs-list');
  try {
    const data = await api('/api/fs?path=' + encodeURIComponent(path));
    fsCurrent = data.path;
    document.getElementById('fs-path').textContent = data.path;
    document.getElementById('fs-up').disabled = !data.parent;
    box.innerHTML = '';
    if (!data.dirs.length) {
      box.appendChild(el('div', 'muted empty', '没有子目录'));
      return;
    }
    data.dirs.forEach((d) => {
      const row = el('button', 'fs-row', d.name + '/');
      row.addEventListener('click', () => loadFs((fsCurrent === '/' ? '' : fsCurrent) + '/' + d.name));
      box.appendChild(row);
    });
  } catch (err) {
    box.innerHTML = '';
    box.appendChild(el('div', 'muted', '加载失败：' + err.message));
  }
}

// ──── 预演 ────

let dryrunTask = null;
let dryrunDanger = 0;

async function openDryRun(id) {
  dryrunTask = id;
  const t = taskConfigs.find((x) => x.id === id);
  document.getElementById('dryrun-title').textContent = (t?.name || id) + ' · 预演';
  const list = document.getElementById('dryrun-list');
  const groups = document.getElementById('dryrun-groups');
  groups.innerHTML = '';
  list.innerHTML = '<div class="muted">计算中…</div>';
  document.getElementById('dryrun-dialog').showModal();
  try {
    const data = await api(`/api/tasks/${id}/dry-run`);
    dryrunDanger = data.danger || 0;
    groups.innerHTML = '';
    (data.groups || []).forEach((g) => {
      const chip = el('span', 'op-chip' + (g.danger ? ' danger' : ''), `${g.label} ${g.count}`);
      groups.appendChild(chip);
    });
    if (!groups.children.length) groups.appendChild(el('span', 'muted', '无事可做'));
    renderDryOps(list, data.ops || []);
  } catch (err) {
    list.innerHTML = '';
    list.appendChild(el('div', 'muted', '预演失败：' + err.message));
  }
}

function renderDryOps(box, ops) {
  box.innerHTML = '';
  if (!ops.length) {
    box.appendChild(el('div', 'muted empty', '没有需要执行的动作，两边已经一致'));
    return;
  }
  ops.forEach((op) => {
    const line = el('div', 'log-line' + (op.danger ? ' lv-error' : ''));
    line.append(
      el('span', 'lv', op.label),
    );
    const p = el('span', 't', op.path);
    line.appendChild(p);
    if (op.reason) line.appendChild(el('span', 'muted', '  ' + op.reason));
    box.appendChild(line);
  });
}

// ──── 最近动态 ────

let curTask = null;

function activityRow(ev) {
  const row = el('div', 'hist-row' + (ev.state === 'failed' ? ' fail' : ''));
  const badge = el('span', 'badge ' + (stateCls[ev.state] || ''), stateLabel[ev.state] || ev.state);
  const trig = el('span', 'badge', (ev.scope === 'download' ? '云端' : '本地') + ' · ' +
    ({ manual: '手动', cron: '定时', watch: '监听', init: '启动' }[ev.trigger] || ev.trigger));
  const time = el('span', 'hr-time', fmtTime(ev.time));
  const s = ev.stats || {};
  const parts = [`耗时 ${fmtDuration(ev.duration_ms)}`];
  if (s.uploaded) parts.push(`上传 ${s.uploaded}`);
  if (s.downloaded) parts.push(`下载 ${s.downloaded}`);
  if (s.strm_generated) parts.push(`STRM ${s.strm_generated}`);
  if (s.deleted) parts.push(`清理 ${s.deleted}`);
  if (s.failed) parts.push(`失败 ${s.failed}`);
  if (s.dirs && s.dirs.length) parts.push('涉及 ' + s.dirs.join('、'));
  const stats = el('span', 'hr-stats', parts.join(' · '));
  row.append(badge, trig, time, stats);
  if (ev.error) {
    const errLine = el('div', 'log-line lv-error', '⚠ ' + ev.error);
    row.appendChild(errLine);
  }
  return row;
}

// 动态分页控制器：滚动到底自动加载下一页（每页 50，返回不足 50 判无更多）。
let activityCtrl = null; // 任务中心最近动态
let historyCtrl = null;  // 任务动态弹窗

function createActivityFeed(list, taskID) {
  let loaded = 0, hasMore = true, loading = false, pending = null;
  return {
    async loadMore() {
      if (loading || !hasMore) return;
      loading = true;
      if (loaded > 0) { pending = el('div', 'muted', '加载中…'); list.appendChild(pending); }
      try {
        const q = taskID ? `task_id=${encodeURIComponent(taskID)}&` : '';
        const data = await api(`/api/activity?${q}offset=${loaded}&limit=50`);
        const events = data.events || [];
        events.forEach((ev) => list.appendChild(activityRow(ev)));
        loaded += events.length;
        hasMore = events.length >= 50;
        if (loaded === 0 && !events.length) list.appendChild(el('div', 'muted empty', '暂无动态记录'));
      } catch {
        if (loaded === 0) list.appendChild(el('div', 'muted', '加载失败'));
        hasMore = false;
      } finally {
        if (pending) { pending.remove(); pending = null; }
        loading = false;
      }
    },
  };
}

function loadActivity() {
  const list = document.getElementById('activity-list');
  list.innerHTML = '';
  activityCtrl = createActivityFeed(list, '');
  activityCtrl.loadMore();
}

async function openHistory(id) {
  curTask = id;
  const t = taskConfigs.find((x) => x.id === id);
  document.getElementById('history-dialog-title').textContent = (t?.name || id) + ' · 执行动态';
  const list = document.getElementById('history-list');
  list.innerHTML = '';
  historyCtrl = createActivityFeed(list, id);
  historyCtrl.loadMore();
  document.getElementById('history-dialog').showModal();
}
