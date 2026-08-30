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
  loadSyslog();
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
// 系统级日志走下方「程序日志」卡片（renderSyslogs），顶部不再重复显示。
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
  use.setAttribute('href', running ? '#i-stop' : '#i-play');
}

// ──── 程序日志（无任务上下文的系统级日志：落库、筛选、跟随、向上加载更早） ────
let syslogs = [];      // 正序（旧→新），含 {seq,time,level,msg,attrs}
let sysLatest = 0;     // 已见最大 seq（SSE 去重）
let sysMore = true;    // 是否还有更旧日志可加载
let sysFollow = true;  // 跟随底部（新日志自动滚到底）
let sysLoading = false;
let sysFilter = 'all';
const SYS_PAGE = 100;

function sysBox() { return document.getElementById('system-log-list'); }

// appendSyslog SSE 实时推送的新日志（seq 去重，兼容回放与实时重叠）。
// 跟随模式下只增量追加一行，避免日志量大时每次全量重渲染。
export function appendSyslog(e) {
  if (!e || e.seq == null || e.seq <= sysLatest) return;
  syslogs.push(e);
  sysLatest = e.seq;
  appendSyslogLine(e);
  if (sysFollow) scrollSysBottom();
}

// appendSyslogLine 把一条日志追加到列表底部；空态/筛选不匹配时退回全量渲染。
function appendSyslogLine(e) {
  const box = sysBox();
  if (!box) return;
  if (sysFilter !== 'all' && e.level !== sysFilter) return;
  if (!box.children.length || box.querySelector('.empty')) { renderSyslogs(); return; }
  box.appendChild(syslogLine(e));
}

// loadSyslog 加载最新一批（默认 100 条）并滚到底部。
async function loadSyslog() {
  const box = sysBox();
  if (!box) return;
  try {
    const data = await api('/api/system-logs?limit=' + SYS_PAGE);
    syslogs = data.logs || [];
    sysLatest = syslogs.length ? syslogs[syslogs.length - 1].seq : 0;
    sysMore = !!data.has_more;
    renderSyslogs();
    scrollSysBottom();
  } catch { /* 静默：SSE 仍会补齐 */ }
}

// loadOlderSyslog 滚动到顶部时加载更旧的日志（插入列表头部并保持视口位置）。
async function loadOlderSyslog() {
  if (sysLoading || !sysMore || !syslogs.length) return;
  const box = sysBox();
  sysLoading = true;
  try {
    const data = await api(`/api/system-logs?limit=${SYS_PAGE}&before=${syslogs[0].seq}`);
    const older = data.logs || [];
    if (!older.length) { sysMore = false; return; }
    const keep = box.scrollHeight - box.scrollTop; // 距底部距离，插入后保持
    syslogs = [...older, ...syslogs];
    sysMore = !!data.has_more;
    renderSyslogs();
    box.scrollTop = box.scrollHeight - keep;
  } catch { /* 静默 */ } finally { sysLoading = false; }
}

function scrollSysBottom() {
  const box = sysBox();
  if (box) box.scrollTop = box.scrollHeight;
}

// renderSyslogs 按当前等级筛选渲染。
function renderSyslogs() {
  const box = sysBox();
  if (!box) return;
  box.innerHTML = '';
  const list = sysFilter === 'all' ? syslogs : syslogs.filter((l) => l.level === sysFilter);
  if (!list.length) {
    box.appendChild(el('div', 'muted empty', '暂无程序日志'));
    return;
  }
  list.forEach((l) => box.appendChild(syslogLine(l)));
}

// syslogLine 构建一条日志行。
function syslogLine(l) {
  const line = el('div', 'log-line lv-' + l.level.toLowerCase());
  line.append(
    el('span', 't', fmtTime(l.time)),
    el('span', 'lv', l.level),
  );
  line.appendChild(document.createTextNode(' ' + (l.msg || '') + (l.attrs ? '  ' + l.attrs : '')));
  return line;
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
  // 附带扫描勾选「下载文件」→ 显隐「生成 STRM 文件」
  form.elements.attach_fetch.addEventListener('change', () => updateAttachStrm(form));
  document.querySelectorAll('[data-close]').forEach((b) =>
    b.addEventListener('click', () => document.getElementById(b.dataset.close).close()));

  // 清空程序日志
  document.getElementById('system-logs-clear').addEventListener('click', async () => {
    if (!confirm('确认清空全部程序日志？')) return;
    try {
      await api('/api/system-logs', { method: 'DELETE' });
      syslogs = [];
      sysLatest = 0;
      sysMore = true;
      renderSyslogs();
      toast('已清空程序日志', 'ok');
    } catch (err) { toast(err.message, 'err'); }
  });
  // 等级筛选
  document.getElementById('syslog-filter').addEventListener('change', (e) => {
    sysFilter = e.target.value;
    renderSyslogs();
    scrollSysBottom();
  });
  // 程序日志滚动：到底恢复跟随；滚到顶部加载更早
  const sbox = document.getElementById('system-log-list');
  sbox.addEventListener('scroll', () => {
    const nearBottom = sbox.scrollHeight - sbox.scrollTop - sbox.clientHeight < 40;
    sysFollow = nearBottom;
    if (sbox.scrollTop < 30) loadOlderSyslog();
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
  updateAttachStrm(form);
}

// updateAttachStrm 未勾选「下载文件」时隐藏「生成 STRM 文件」并自动取消其勾选
// （不下载就没有 strm 可言；隐藏状态下残留 checked 会导致保存时误提交空附带扫描）。
function updateAttachStrm(form) {
  const row = form.querySelector('#attach-strm-row');
  const fetchChecked = form.elements.attach_fetch.checked;
  if (row) row.hidden = !fetchChecked;
  if (!fetchChecked) form.elements.attach_to_strm.checked = false;
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
  // 方向配置按类型分组：push 段（含可选的 after_pull 连带云端扫描）/ pull 段
  const push = t.push || {};
  const watch = push.watch || {};
  const rescan = push.rescan || {};
  const attach = push.after_pull || {};
  const pull = t.pull || {};
  const cron = pull.cron || {};
  e.watch_enabled.checked = !!watch.enabled;
  e.debounce_minutes.value = watch.quiet_minutes ?? 10;
  e.strm_now.checked = !!watch.strm_now;
  e.video_now.checked = !!watch.video_now;
  e.rescan_enabled.checked = !!rescan.enabled;
  e.rescan_interval_hours.value = rescan.interval_hours ?? 12;
  e.rescan_then_pull.checked = !!push.after_pull;
  e.to_strm.checked = !!push.to_strm;
  e.to_cache.checked = !!push.to_cache;
  // 附带云端扫描组（仅 push 任务 + 勾选「全量扫描后扫描云端」时展示）
  e.attach_fetch.checked = !!attach.fetch_missing;
  e.attach_to_strm.checked = !!attach.to_strm;
  e.drop_redundant.checked = !!attach.drop_redundant;
  // 云端方向组（仅 pull 任务展示）
  e.pull_cron_enabled.checked = !!cron.enabled;
  e.pull_cron_interval_hours.value = cron.interval_hours ?? 12;
  e.pull_to_strm.checked = !!pull.to_strm;
  e.archive_to_temp.checked = !!pull.archive_to_temp;
}

// formToTask 只组装与当前类型匹配的那一段方向配置：
// 下载云端独有是云端扫描的本职（恒开，不暴露开关）；冗余删除只有 push 的连带扫描可配。
function formToTask(form, id) {
  const e = form.elements;
  const num = (v) => Math.max(0, parseInt(v, 10) || 0);
  const t = {
    id,
    name: e.name.value.trim(),
    kind: e.kind.value,
    enabled: e.enabled.checked,
    local_dir: e.local_dir.value.trim(),
    cloud_dir: e.cloud_dir.value.trim(),
  };
  if (t.kind === 'push') {
    t.push = {
      watch: { enabled: e.watch_enabled.checked, quiet_minutes: num(e.debounce_minutes.value), strm_now: e.strm_now.checked, video_now: e.video_now.checked },
      rescan: { enabled: e.rescan_enabled.checked, interval_hours: num(e.rescan_interval_hours.value) },
      to_strm: e.to_strm.checked,
      to_cache: e.to_cache.checked,
    };
    if (e.rescan_then_pull.checked) {
      const after = {
        fetch_missing: e.attach_fetch.checked,
        to_strm: e.attach_to_strm.checked,
        drop_redundant: e.drop_redundant.checked,
      };
      // 三个子选项都没勾时附带扫描无事可做，自动去掉「全量扫描后扫描云端」（不提交 after_pull）
      if (after.fetch_missing || after.to_strm || after.drop_redundant) {
        t.push.after_pull = after;
      }
    }
  } else {
    t.pull = {
      cron: { enabled: e.pull_cron_enabled.checked, interval_hours: num(e.pull_cron_interval_hours.value) },
      to_strm: e.pull_to_strm.checked,
      archive_to_temp: e.archive_to_temp.checked,
    };
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
    const data = await api(`/api/tasks/${id}/runs?limit=200`);
    renderRuns(data.runs || []);
  } catch (err) {
    list.innerHTML = '';
    list.appendChild(el('div', 'muted', '加载失败：' + err.message));
  }
}

// renderRuns 分批渲染执行历史（每批 RUNS_PAGE 条），避免一次竖排 200 条 DOM。
function renderRuns(runs) {
  const list = document.getElementById('history-list');
  list.innerHTML = '';
  if (!runs.length) { list.appendChild(el('div', 'muted', '暂无执行记录')); return; }
  const PAGE = 6;
  let shown = 0;
  const more = el('button', 'btn ghost sm more-btn');
  const show = () => {
    runs.slice(shown, shown + PAGE).forEach((r) => {
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
      row.addEventListener('click', () => {
        // 选中标识：只保留当前行的 sel 高亮
        list.querySelectorAll('.hist-row.sel').forEach((e) => e.classList.remove('sel'));
        row.classList.add('sel');
        openRunLog(r);
      });
      list.insertBefore(row, more);
      shown++;
    });
    if (shown >= runs.length) { more.remove(); }
    else {
      more.textContent = `加载更多（剩 ${runs.length - shown} 条）`;
      more.addEventListener('click', show);
    }
  };
  more.addEventListener('click', show);
  list.appendChild(more);
  show();
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

// renderLogs 分批渲染单次执行的明细日志（每批 LOGS_PAGE 条），单条 run 最多 1000 条时避免一次渲染堆爆。
function renderLogs(logs) {
  const box = document.getElementById('run-log');
  box.innerHTML = '';
  if (!logs.length) { box.appendChild(el('div', 'muted', '（无明细日志）')); return; }
  const PAGE = 200;
  let shown = 0;
  const more = el('button', 'btn ghost sm more-btn');
  const show = () => {
    logs.slice(shown, shown + PAGE).forEach((l) => {
      const line = el('div', 'log-line lv-' + l.level.toLowerCase());
      line.append(
        el('span', 't', fmtTime(l.time)),
        el('span', 'lv', l.level),
      );
      line.appendChild(document.createTextNode(' ' + l.msg + (l.attrs ? '  ' + l.attrs : '')));
      box.insertBefore(line, more);
      shown++;
    });
    if (shown >= logs.length) { more.remove(); }
    else {
      more.textContent = `加载更多（剩 ${logs.length - shown} 条）`;
      more.addEventListener('click', show);
    }
  };
  more.addEventListener('click', show);
  box.appendChild(more);
  show();
  box.scrollTop = box.scrollHeight;
}
