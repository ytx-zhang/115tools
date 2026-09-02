// tasks.js —— 任务中心（视图包，import() 后常驻）：横幅、任务卡片、编辑弹窗、预览、最近日志、日志面板。
// 结构声明在 index.html 的 <template>，本模块只 clone + 按选择器填值（一律 textContent，天然防 XSS）。

import { api, el, fmtTime, fmtDuration, toast, fromTemplate, els } from './api.js';
import { state, setHandlers } from './main.js';

let taskConfigs = [];          // 完整任务配置（含目录/开关）
let bound = false;
const taskCards = new Map();   // id -> 卡片元素引用；SSE 推送时增量更新，不重建 DOM 防跳动

const stateLabel = { running: '运行中', success: '成功', canceled: '已取消', failed: '失败' };
const stateCls = { running: 'run', success: 'ok', canceled: 'warn', failed: 'err' };

// ──── 生命周期 ────

export function initTasks() {
  if (!bound) { bindOnce(); bound = true; }
  setHandlers(handleLogs, renderTasks);   // 注册一次；切走视图不注销（模块常驻，继续累加未读）
  loadTasks();
  loadActivity();
  syncLogs();                             // 仅「首进非任务中心页」场景需要补拉
}

export function stopTasks() {}

async function loadTasks() {
  try { taskConfigs = (await api('/api/tasks')).tasks || []; }
  catch { taskConfigs = []; }
  taskCards.clear();
  els('task-grid').replaceChildren();
  renderTasks();
}

// ──── 渲染 ────

export function renderTasks() {
  renderBanners();
  renderGrid();
}

function renderBanners() {
  const box = els('banners');
  box.replaceChildren();
  const add = (cls, text) => box.append(el('div', 'banner ' + cls, text));
  if (!state.configReady) add('warn', '配置不完整，同步未启动，请到「设置」补齐：' + state.missing.join('、'));
  if (state.initError) add('error', '初始化失败：' + state.initError);
}

function renderGrid() {
  const grid = els('task-grid');
  const runtime = Object.fromEntries(state.tasks.map((t) => [t.id, t]));
  // 移除已删除任务的卡片
  for (const [id, e] of taskCards) {
    if (!taskConfigs.some((t) => t.id === id)) { e.card.remove(); taskCards.delete(id); }
  }
  // 新增 / 增量更新
  taskConfigs.forEach((t) => {
    let e = taskCards.get(t.id);
    if (!e) { e = createCard(t, runtime[t.id]); taskCards.set(t.id, e); grid.appendChild(e.card); }
    else updateCard(e, runtime[t.id]);
  });
  els('task-empty').hidden = taskConfigs.length > 0;
}

function cardState(rt) {
  if (rt?.initializing) return { cls: 'init', text: '初始化中', running: false, disabled: true };
  if (rt?.running) return { cls: 'run', text: '运行中', running: true, disabled: false };
  if (rt?.queued) return { cls: 'queued', text: '排队中', running: false, disabled: true };
  return { cls: '', text: '空闲', running: false, disabled: false };
}

// 卡片状态部分：构建与增量更新共用同一套 patch。
function updateCard(e, rt) {
  const st = cardState(rt);
  const total = rt?.total || 0;
  const done = rt?.completed || 0;
  const scanning = st.running && !total;
  e.badge.className = 'badge ' + st.cls;
  e.badge.textContent = st.text;
  e.bar.className = 'progress' + (scanning ? ' ind' : '');
  e.fill.style.width = scanning ? '100%' : (total ? Math.min(100, done / total * 100) + '%' : '0%');
  e.nums.textContent = scanning ? '扫描中' : `${done} / ${total}`;
  e.cur.textContent = st.running && rt?.current ? '正在处理 ' + rt.current : '';
  e.cur.title = e.cur.textContent;
  const meta = [];
  if (rt?.last_run) meta.push('上次 ' + fmtTime(rt.last_run));
  if (rt?.next_cron) meta.push('下次 ' + fmtTime(rt.next_cron));
  e.meta.textContent = meta.join(' · ');
  // 执行/停止按钮就地更新图标与文案（保留结构，仅改属性）
  e.run.use.setAttribute('href', st.running ? '#i-stop' : '#i-play');
  e.run.span.textContent = st.running ? '停止' : '执行';
  e.run.btn.className = 'btn sm ' + (st.running ? 'danger' : 'primary');
  e.run.btn.dataset.action = st.running ? 'stop' : 'start';
  e.card.querySelectorAll('.tc-actions button').forEach((b) => { b.disabled = st.disabled; });
}

function createCard(t, rt) {
  const card = fromTemplate('tpl-task-card');
  card.dataset.id = t.id;
  const e = {
    card,
    badge: card.querySelector('.tc-status .badge'),
    bar: card.querySelector('.progress'),
    fill: card.querySelector('.progress i'),
    nums: card.querySelector('.tc-nums'),
    cur: card.querySelector('.tc-cur'),
    meta: card.querySelector('.tc-meta'),
    run: {
      btn: card.querySelector('.tc-actions button'),
      use: card.querySelector('.tc-actions button use'),
      span: card.querySelector('.tc-actions button span'),
    },
  };
  card.querySelector('.tc-name').textContent = t.name;
  card.querySelector('.tc-name').title = t.name;
  card.querySelector('.local').textContent = t.local_dir || '—';
  card.querySelector('.cloud').textContent = t.cloud_dir || '—';
  const dirs = [t.upload && '上传', t.download && '下载'].filter(Boolean).join(' + ') || '未启用方向';
  card.querySelector('.k-badge').textContent = dirs;
  const toggle = card.querySelector('[data-toggle]');
  toggle.checked = !!t.enabled;
  toggle.dataset.toggle = t.id;
  updateCard(e, rt);
  return e;
}

// ──── 卡片操作 ────

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

// ──── 场景预设（快速填充，无选中态；点完即隐藏预设区） ────

function applyArchiveRule(form) {
  const opt = els('opt-archive');
  if (!opt) return;
  if (form.elements.upload.checked) { opt.hidden = true; form.elements.archive.checked = false; }
  else opt.hidden = false;
}
function applyPreset(form, preset) {
  const e = form.elements;
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
  els('adv-up').hidden = preset === 'pull';
  els('adv-down').hidden = preset === 'push';
  applyArchiveRule(form);
  els('preset-box').hidden = true;
}
// 新建任务：数值字段默认值统一来自 FIELDS，避免与 HTML value / 后端默认三处重复
function applyNumDefaults(form) {
  for (const [name, , type, def] of FIELDS) {
    if (type === 'int') form.elements[name].value = def;
  }
}

// ──── 新建 / 编辑弹窗（字段表驱动双向映射） ────

const FIELDS = [
  ['name', 'name'], ['enabled', 'enabled', 'bool'], ['local_dir', 'local_dir'], ['cloud_dir', 'cloud_dir'],
  ['upload', 'upload', 'bool'], ['watch', 'watch', 'bool'], ['quiet_minutes', 'quiet_minutes', 'int', 10],
  ['instant_now', 'instant_now', 'bool'], ['download', 'download', 'bool'], ['to_strm_dl', 'to_strm_dl', 'bool'],
  ['archive', 'archive', 'bool'], ['to_strm', 'to_strm', 'bool'], ['to_cache', 'to_cache', 'bool'],
  ['cron_enabled', 'cron.enabled', 'bool'], ['cron_interval_hours', 'cron.interval_hours', 'int', 12],
];
const num = (v) => Math.max(0, parseInt(v, 10) || 0);
const getPath = (o, p) => p.split('.').reduce((a, k) => a?.[k], o);
function setPath(o, p, v) { const ks = p.split('.'); const last = ks.pop(); let c = o; for (const k of ks) c = c[k] ??= {}; c[last] = v; }

function taskToForm(form, t) {
  const e = form.elements;
  for (const [name, path, type, def] of FIELDS) {
    const v = getPath(t, path);
    if (type === 'bool') e[name].checked = !!v;
    else if (type === 'int') e[name].value = v ?? def;
    else e[name].value = v ?? '';
  }
}
function formToTask(form, id) {
  const e = form.elements;
  const t = { id };
  for (const [name, path, type] of FIELDS) {
    setPath(t, path, type === 'bool' ? e[name].checked : type === 'int' ? num(e[name].value) : e[name].value.trim());
  }
  return t;
}

function openDialog(id) {
  const form = els('task-form');
  form.reset();
  els('task-dialog-title').textContent = id ? '编辑任务' : '新建任务';
  form.dataset.id = id || '';
  els('preset-box').hidden = !!id;
  if (id) {
    els('adv-up').hidden = false;
    els('adv-down').hidden = false;
    const t = taskConfigs.find((x) => x.id === id);
    if (t) taskToForm(form, t);
    applyArchiveRule(form);
  } else {
    applyPreset(form, 'push');
    applyNumDefaults(form);
    els('preset-box').hidden = false;
  }
  els('task-dialog').showModal();
}

// ──── 目录浏览 ────

let fsTarget = null;
let fsCurrent = '/';

function bindFsBrowser() {
  els('fs-up').addEventListener('click', () => { loadFs(fsCurrent === '/' ? '/' : fsCurrent.replace(/\/[^/]*$/, '') || '/'); });
  els('fs-pick').addEventListener('click', () => {
    if (fsTarget) fsTarget.value = fsCurrent;
    els('fs-dialog').close();
  });
}
function openFsBrowser(input) {
  fsTarget = input;
  els('fs-dialog').showModal();
  loadFs(input.value || '/');
}
async function loadFs(path) {
  const box = els('fs-list');
  try {
    const data = await api('/api/fs?path=' + encodeURIComponent(path));
    fsCurrent = data.path;
    els('fs-path').textContent = data.path;
    els('fs-up').disabled = !data.parent;
    box.replaceChildren(...data.dirs.map((d) => {
      const b = fromTemplate('tpl-fs-row');
      b.textContent = d.name + '/';
      b.addEventListener('click', () => loadFs((fsCurrent === '/' ? '' : fsCurrent) + '/' + d.name));
      return b;
    }));
    if (!data.dirs.length) box.append(el('div', 'muted', '没有子目录'));
  } catch (err) {
    box.replaceChildren(el('div', 'muted', '加载失败：' + err.message));
  }
}

// ──── 预览 ────

let dryrunTask = null;
let dryrunDanger = 0;

async function openDryRun(id) {
  dryrunTask = id;
  const t = taskConfigs.find((x) => x.id === id);
  els('dryrun-title').textContent = (t?.name || id) + ' · 预览';
  const list = els('dryrun-list');
  const groups = els('dryrun-groups');
  groups.replaceChildren();
  list.replaceChildren(el('div', 'muted', '计算中…'));
  els('dryrun-dialog').showModal();
  try {
    const data = await api(`/api/tasks/${id}/dry-run`);
    dryrunDanger = data.danger || 0;
    groups.replaceChildren(...(data.groups || []).map((g) =>
      el('span', 'badge' + (g.danger ? ' warn' : ' ok'), `${g.label} ${g.count}`)));
    if (!groups.children.length) groups.append(el('span', 'muted', '无事可做'));
    renderDryOps(list, data.ops || []);
  } catch (err) {
    list.replaceChildren(el('div', 'muted', '预览失败：' + err.message));
  }
}

// 预览动作行与日志行同构（标签 + 路径 + 可选说明），复用 tpl-line。
function renderDryOps(box, ops) {
  box.replaceChildren();
  if (!ops.length) { box.append(el('div', 'muted', '没有需要执行的动作，两边已经一致')); return; }
  box.replaceChildren(...ops.map((op) => {
    const n = fromTemplate('tpl-line', { '.lv': op.label, '.msg': op.path });
    if (op.danger) n.className = 'log-line lv-error';
    if (op.reason) n.querySelector('.attrs').textContent = ' ' + op.reason;
    return n;
  }));
}

// ──── 警告与错误日志（logfeed 收集，SSE 推送，右下角提醒） ────

const LOG_CAP = 300;          // 与后端缓冲幅度一致
let logEntries = [];          // 最新在前
let logUnread = 0;            // 角标未读数 = 缓冲内全部条目（用户不清空就一直显示）
let logSynced = false;        // 仅首次进入任务中心补拉一次全量

function dlgOpen() { const d = els('log-dialog'); return d && d.open; }

// handleLogs 由 main.js SSE 分派调用：full=连接回放/断线重连全量，否则增量追加。
export function handleLogs(p) {
  const logs = Array.isArray(p.logs) ? p.logs : [];
  if (p.full) logEntries = logs;
  else if (logs.length) {
    logEntries = logs.concat(logEntries);
    if (logEntries.length > LOG_CAP) logEntries.length = LOG_CAP;
  }
  logUnread = logEntries.length;   // 全部条目都算，不清空不走
  if (dlgOpen()) renderLogList(); else updateLogAlert();
}

// 补拉全量（仅「首进非任务中心页」漏收场景）。
async function syncLogs() {
  if (logSynced) return;
  logSynced = true;
  try {
    const { logs } = await api('/api/logs');
    logEntries = logs ?? [];
    logUnread = logEntries.length;
    dlgOpen() ? renderLogList() : updateLogAlert();
  } catch {}
}

function updateLogAlert() {
  const btn = els('log-alert');
  const badge = els('log-unread');
  if (!btn || !badge) return;
  badge.textContent = logUnread > 99 ? '99+' : String(logUnread);
  btn.hidden = logUnread === 0;
  btn.classList.toggle('flash', logUnread > 0);
}

function renderLogList() {
  const list = els('log-list');
  if (!list) return;
  list.replaceChildren();
  if (!logEntries.length) { list.append(el('div', 'muted', '暂无警告或错误')); return; }
  list.replaceChildren(...logEntries.map(logRow));
}

// 日志行：复用 tpl-line（级别 + 时间 + 消息 + 可选属性）。
function logRow(e) {
  const isErr = e.level === 'ERROR';
  const n = fromTemplate('tpl-line', { '.lv': isErr ? 'ERR' : 'WARN', '.t': fmtTime(e.time), '.msg': e.msg });
  n.className = 'log-line ' + (isErr ? 'lv-error' : 'lv-warn');
  if (e.attrs?.length) n.querySelector('.attrs').textContent = ' ' + e.attrs.map((a) => `${a.key}=${a.value}`).join(' ');
  return n;
}

function openLogDialog() {
  renderLogList();
  els('log-dialog').showModal();
}
async function clearLogs() {
  if (!confirm('确认清空全部警告/错误日志？')) return;
  try {
    await api('/api/logs', { method: 'DELETE' });
    logEntries = [];
    logUnread = 0;
    renderLogList();
    updateLogAlert();
    toast('已清空日志', 'ok');
  } catch (err) { toast(err.message, 'err'); }
}

// ──── 最近日志 ────

let curTask = null;
let activityCtrl = null;
let historyCtrl = null;

// 日志行：复用 tpl-act-row（状态 + 触发 + 时间 + 统计 + 可选错误行）。
function activityRow(ev) {
  const s = ev.stats || {};
  const parts = [`耗时 ${fmtDuration(ev.duration_ms)}`];
  if (s.uploaded) parts.push(`上传 ${s.uploaded}`);
  if (s.downloaded) parts.push(`下载 ${s.downloaded}`);
  if (s.strm_generated) parts.push(`STRM ${s.strm_generated}`);
  if (s.deleted) parts.push(`清理 ${s.deleted}`);
  if (s.failed) parts.push(`失败 ${s.failed}`);
  if (s.dirs?.length) parts.push('涉及 ' + s.dirs.join('、'));
  const node = fromTemplate('tpl-act-row', {
    '.badge': stateLabel[ev.state] || ev.state,
    '.trig': (ev.scope === 'download' ? '云端' : '本地') + ' · ' +
      ({ manual: '手动', cron: '定时', watch: '监听', init: '启动' }[ev.trigger] || ev.trigger),
    '.hr-time': fmtTime(ev.time),
    '.hr-stats': parts.join(' · '),
  });
  node.className = 'hist-row' + (ev.state === 'failed' ? ' fail' : '');
  node.querySelector('.badge').className = 'badge ' + (stateCls[ev.state] || '');
  if (ev.error) { const err = node.querySelector('.err'); err.hidden = false; err.textContent = '⚠ ' + ev.error; }
  return node;
}

function createActivityFeed(list, taskID) {
  let loaded = 0, hasMore = true, loading = false, pending = null;
  return {
    async loadMore() {
      if (loading || !hasMore) return;
      loading = true;
      if (loaded > 0) { pending = el('div', 'muted', '加载中…'); list.append(pending); }
      try {
        const q = taskID ? `task_id=${encodeURIComponent(taskID)}&` : '';
        const data = await api(`/api/activity?${q}offset=${loaded}&limit=50`);
        const events = data.events || [];
        events.forEach((ev) => list.append(activityRow(ev)));
        loaded += events.length;
        hasMore = events.length >= 50;
        if (loaded === 0 && !events.length) list.append(el('div', 'muted', '暂无日志记录'));
      } catch {
        if (loaded === 0) list.append(el('div', 'muted', '加载失败'));
        hasMore = false;
      } finally {
        if (pending) { pending.remove(); pending = null; }
        loading = false;
      }
    },
  };
}

function loadActivity() {
  const list = els('activity-list');
  list.replaceChildren();
  activityCtrl = createActivityFeed(list, '');
  activityCtrl.loadMore();
}
async function openHistory(id) {
  curTask = id;
  const t = taskConfigs.find((x) => x.id === id);
  els('history-dialog-title').textContent = (t?.name || id) + ' · 执行日志';
  const list = els('history-list');
  list.replaceChildren();
  historyCtrl = createActivityFeed(list, id);
  historyCtrl.loadMore();
  els('history-dialog').showModal();
}

// ──── 事件绑定（一次性，事件委托） ────

function bindOnce() {
  const grid = els('task-grid');
  grid.addEventListener('click', async (e) => {
    const btn = e.target.closest('button[data-action]');
    if (!btn) return;
    const id = btn.closest('.task-card')?.dataset.id;
    if (!id) return;
    const a = btn.dataset.action;
    if (a === 'start') startTask(id);
    else if (a === 'stop') stopTask(id);
    else if (a === 'history') openHistory(id);
    else if (a === 'dryrun') openDryRun(id);
    else if (a === 'edit') openDialog(id);
    else if (a === 'delete') deleteTask(id);
  });
  grid.addEventListener('change', async (e) => {
    const chk = e.target.closest('input[data-toggle]');
    if (!chk) return;
    const t = taskConfigs.find((x) => x.id === chk.dataset.toggle);
    if (!t) return;
    t.enabled = chk.checked;
    try { await saveTask(t); toast('已' + (chk.checked ? '启用' : '停用'), 'ok'); }
    catch (err) { chk.checked = !chk.checked; toast(err.message, 'err'); }
  });

  els('task-new').addEventListener('click', () => openDialog(null));
  els('empty-new').addEventListener('click', () => openDialog(null));

  const form = els('task-form');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    try {
      await saveTask(formToTask(form, form.dataset.id || ''));
      form.closest('dialog').close();
      toast('任务已保存', 'ok');
      loadTasks();
    } catch (err) { toast(err.message, 'err'); }
  });
  form.elements.upload.addEventListener('change', () => applyArchiveRule(form));
  document.querySelectorAll('.preset-card').forEach((b) =>
    b.addEventListener('click', () => applyPreset(form, b.dataset.preset)));
  els('browse-local').addEventListener('click', () => openFsBrowser(form.elements.local_dir));

  document.querySelectorAll('[data-close]').forEach((b) =>
    b.addEventListener('click', () => els(b.dataset.close).close()));

  els('activity-refresh').addEventListener('click', loadActivity);
  els('activity-clear').addEventListener('click', async () => {
    if (!confirm('确认清空全部日志记录？')) return;
    try {
      await Promise.all(taskConfigs.map((t) =>
        api(`/api/activity?task_id=${encodeURIComponent(t.id)}`, { method: 'DELETE' })));
      toast('已清空日志记录', 'ok');
      loadActivity();
    } catch (err) { toast(err.message, 'err'); }
  });

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
        } else if (box.getBoundingClientRect().bottom <= window.innerHeight + 60) ctrl.loadMore();
      }, 150);
    };
    box.addEventListener('scroll', check);
    window.addEventListener('scroll', check);
  };
  bindFeedScroll(els('activity-list'), () => activityCtrl);
  bindFeedScroll(els('history-list'), () => historyCtrl);

  els('history-clear').addEventListener('click', async () => {
    if (!curTask) return;
    if (!confirm('确认清空该任务的全部日志记录？')) return;
    try {
      await api(`/api/activity?task_id=${encodeURIComponent(curTask)}`, { method: 'DELETE' });
      toast('已清空该任务日志', 'ok');
      openHistory(curTask);
    } catch (err) { toast(err.message, 'err'); }
  });

  els('dryrun-run').addEventListener('click', async () => {
    if (dryrunDanger > 0 && !confirm(`预览中包含 ${dryrunDanger} 个删除/归档等不可逆动作，确认执行？`)) return;
    if (dryrunDanger === 0 && !confirm('确认立即执行该任务？')) return;
    try {
      await api(`/api/tasks/${dryrunTask}/start`, { method: 'POST' });
      els('dryrun-dialog').close();
      toast('任务已启动', 'ok');
    } catch (err) { toast(err.message, 'err'); }
  });

  els('log-alert').addEventListener('click', openLogDialog);
  els('log-clear').addEventListener('click', clearLogs);

  bindFsBrowser();
}
