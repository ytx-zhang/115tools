// offline.js —— 离线下载：添加任务、任务列表轮询、删除/清空、配额。
// 任务表格用 <template id="offline-row"> 克隆重建 tbody（5s 轮询低频，整表重建开销可接受）；
// 表单提交/删除/清空走命令式事件绑定（离线下载无 SSE，命令式更直接）。
import { api, toast, toastError, fmtSize } from './api.js';

let timer = null;

const POLL_INTERVAL = 5000;
const FAIL_URL_LEN = 60;

const statusMap = {
  '-1': ['失败', 'err'],
  '0': ['分配中', 'warn'],
  '1': ['下载中', 'run'],
  '2': ['完成', 'ok'],
};

// off 是普通状态对象，数据变化后显式调用 render 函数刷新 DOM。
export const off = {
  tasks: [],
  page: 1,
  pageCount: 1,
  count: 0,
  quota: '',

  prev() { if (off.page > 1) { off.page--; off.refresh(); } },
  next() { if (off.page < off.pageCount) { off.page++; off.refresh(); } },

  async refresh() {
    try {
      const data = await api(`/api/offline/tasks?page=${off.page}`);
      off.pageCount = Math.max(1, data.page_count || 1);
      off.count = data.count || 0;
      off.tasks = (data.tasks || []).map(t => {
        const [text, cls] = statusMap[String(t.status)] || [`状态${t.status}`, ''];
        return {
          info_hash: t.info_hash,
          name: t.name,
          sizeText: fmtSize(t.size),
          pct: +(t.percentDone || 0).toFixed(1),
          statusText: text,
          statusCls: cls,
        };
      });
      renderTasks();
      renderPager();
    } catch (err) {
      // 401 表示会话失效需重新登录，停止轮询；其余错误（网络抖动 / 服务端临时异常）
      // 仅提示，保留轮询，避免一次失败就彻底停掉任务列表。
      if (err.status === 401) {
        stopOffline();
        return;
      }
      toastError(err);
    }
  },
};

export function initOffline() {
  bindOnce();
  loadSavePathPlaceholder();
  off.refresh();
  loadQuota();
  poll();
}

function poll() {
  timer = setTimeout(() => {
    off.refresh();
    poll();
  }, POLL_INTERVAL);
}

export function stopOffline() {
  clearTimeout(timer);
  timer = null;
}

// ──── 渲染 ────

const rowTpl = document.getElementById('offline-row');

function renderTasks() {
  const tbody = document.getElementById('offline-tbody');
  if (!tbody) return;
  tbody.textContent = '';
  if (!off.tasks.length) {
    const tr = document.createElement('tr');
    tr.innerHTML = '<td colspan="5" class="muted center">暂无任务</td>';
    tbody.appendChild(tr);
    return;
  }
  for (const t of off.tasks) {
    const frag = rowTpl.content.cloneNode(true);
    const cells = {};
    frag.querySelectorAll('[data-cell]').forEach(el => { cells[el.dataset.cell] = el; });
    cells.name.textContent = t.name;
    cells.name.title = t.name;
    cells.size.textContent = t.sizeText;
    cells.bar.style.width = t.pct + '%';
    cells.pct.textContent = t.pct + '%';
    cells.status.textContent = t.statusText;
    cells.status.className = `badge ${t.statusCls}`;
    cells.del.textContent = '删任务';
    cells.del.dataset.hash = t.info_hash;
    cells.del.dataset.del = '0';
    cells.delFiles.textContent = '删任务+文件';
    cells.delFiles.dataset.hash = t.info_hash;
    cells.delFiles.dataset.del = '1';
    tbody.appendChild(frag);
  }
}

function renderPager() {
  const info = document.getElementById('page-info');
  if (info) info.textContent = `${off.page} / ${off.pageCount} 页 · 共 ${off.count} 个任务`;
  const prev = document.getElementById('page-prev');
  const next = document.getElementById('page-next');
  if (prev) prev.disabled = off.page <= 1;
  if (next) next.disabled = off.page >= off.pageCount;
}

function renderQuota() {
  const el = document.getElementById('offline-quota');
  if (el) el.textContent = off.quota;
}

// ──── 事件绑定（一次性，stop/init 生命周期不重复绑定）────

let bound = false;
function bindOnce() {
  if (bound) return;
  bound = true;

  document.getElementById('offline-add-form').addEventListener('submit', addTasks);
  document.getElementById('offline-refresh').onclick = () => { off.refresh(); loadQuota(); };
  document.getElementById('page-prev').onclick = () => off.prev();
  document.getElementById('page-next').onclick = () => off.next();

  document.querySelectorAll('[data-clear]').forEach(btn => {
    btn.onclick = () => runOfflineAction(
      `确认${btn.textContent.trim()}？`,
      '/api/offline/clear',
      { flag: +btn.dataset.clear },
      '已清除',
      true, // 清空后回到第一页
    );
  });

  // 事件委托：删除单个任务
  document.getElementById('offline-tbody').addEventListener('click', e => {
    const btn = e.target.closest('[data-hash]');
    if (!btn) return;
    const delFiles = btn.dataset.del === '1';
    runOfflineAction(
      delFiles ? '删除任务并删除已下载文件？' : '删除该任务（保留已下载文件）？',
      '/api/offline/delete',
      { info_hash: btn.dataset.hash, delete_files: delFiles },
      '已删除',
      false,
    );
  });
}

// runOfflineAction 统一「确认 → 调 API → 提示 → 刷新列表」的操作样板。
// 删除与清空两个 handler 的差异（确认文案/接口/参数/提示文案/是否回首页）以参数表达。
async function runOfflineAction(confirmMsg, path, body, toastMsg, resetPage) {
  if (!confirm(confirmMsg)) return;
  try {
    await api(path, { method: 'POST', body });
    toast(toastMsg, 'ok');
    if (resetPage) off.page = 1;
    off.refresh();
  } catch (err) { toastError(err); }
}

async function addTasks(e) {
  e.preventDefault();
  const form = e.target;
  const fd = new FormData(form);
  const btn = form.querySelector('[type=submit]');
  btn.disabled = true;

  // 优先处理种子文件上传
  const torrentFile = fd.get('torrent');
  if (torrentFile instanceof File && torrentFile.size > 0) {
    try {
      const upfd = new FormData();
      upfd.set('torrent', torrentFile);
      upfd.set('save_path', fd.get('save_path') || '');
      const res = await api('/api/offline/torrent', { method: 'POST', body: upfd });
      toast(res.added ? '种子添加成功' : '种子添加失败', res.added ? 'ok' : 'err');
      if (res.added) form.querySelector('[name=torrent]').value = '';
      off.page = 1;
      off.refresh();
      loadQuota();
    } catch (err) {
      toastError(err);
    } finally {
      btn.disabled = false;
    }
    return;
  }

  // 无种子文件 → URL 模式
  const urls = (fd.get('urls') || '').toString().trim();
  if (!urls) {
    toast('请输入下载链接或选择种子文件', 'err');
    btn.disabled = false;
    return;
  }
  try {
    const res = await api('/api/offline/add', {
      method: 'POST',
      body: { urls, save_path: fd.get('save_path') },
    });
    const failed = res.results.filter(r => !r.state);
    toast(`成功添加 ${res.added} 条任务` + (failed.length ? `，失败 ${failed.length} 条` : ''),
      failed.length ? 'err' : 'ok');
    failed.forEach(f => toast(`${f.message || '添加失败'}：${f.url.slice(0, FAIL_URL_LEN)}`, 'err'));
    if (res.added) form.querySelector('[name=urls]').value = '';
    off.page = 1;
    off.refresh();
    loadQuota();
  } catch (err) {
    toastError(err);
  } finally {
    btn.disabled = false;
  }
}

// 将保存目录输入框的占位提示设为已配置的 STRM 目录
async function loadSavePathPlaceholder() {
  const input = document.querySelector('#offline-add-form [name=save_path]');
  if (!input) return;
  try {
    const cfg = await api('/api/config');
    if (cfg.strm_path) input.placeholder = cfg.strm_path;
  } catch { /* 配置不可用时保留默认占位 */ }
}

async function loadQuota() {
  try {
    const q = await api('/api/offline/quota');
    // count>0 显示完整配额（接口能拿到总量），否则仅显示剩余
    off.quota = q.count > 0
      ? `配额：剩余 ${q.surplus} / 共 ${q.count}`
      : `配额：剩余 ${q.surplus}`;
    renderQuota();
  } catch { /* 配额非关键信息，失败不打扰 */ }
}
