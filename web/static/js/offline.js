// offline.js —— 离线下载：添加任务、任务列表轮询、删除/清空、配额
import { api, toast, fmtSize, esc } from './api.js';

let timer = null;
let page = 1;
let pageCount = 1;

const statusMap = {
  '-1': ['失败', 'err'],
  '0': ['分配中', 'warn'],
  '1': ['下载中', 'run'],
  '2': ['完成', 'ok'],
};

export function initOffline() {
  bindOnce();
  loadSavePathPlaceholder();
  refresh();
  loadQuota();
  timer = setInterval(refresh, 5000);
}

export function stopOffline() {
  clearInterval(timer);
  timer = null;
}

let bound = false;
function bindOnce() {
  if (bound) return;
  bound = true;

  document.getElementById('offline-add-form').addEventListener('submit', addTasks);
  document.getElementById('offline-refresh').onclick = () => { refresh(); loadQuota(); };
  document.getElementById('page-prev').onclick = () => { if (page > 1) { page--; refresh(); } };
  document.getElementById('page-next').onclick = () => { if (page < pageCount) { page++; refresh(); } };

  document.querySelectorAll('[data-clear]').forEach(btn => {
    btn.onclick = async () => {
      if (!confirm(`确认${btn.textContent.trim()}？`)) return;
      try {
        await api('/api/offline/clear', { method: 'POST', body: { flag: +btn.dataset.clear } });
        toast('已清除', 'ok');
        page = 1;
        refresh();
      } catch (err) { toast(err.message, 'err'); }
    };
  });

  // 事件委托：删除单个任务
  document.getElementById('offline-tbody').addEventListener('click', async e => {
    const btn = e.target.closest('[data-hash]');
    if (!btn) return;
    const delFiles = btn.dataset.del === '1';
    if (!confirm(delFiles ? '删除任务并删除已下载文件？' : '删除该任务（保留已下载文件）？')) return;
    try {
      await api('/api/offline/delete', {
        method: 'POST',
        body: { info_hash: btn.dataset.hash, delete_files: delFiles },
      });
      toast('已删除', 'ok');
      refresh();
    } catch (err) { toast(err.message, 'err'); }
  });
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
      page = 1;
      refresh();
      loadQuota();
    } catch (err) {
      toast(err.message, 'err');
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
    failed.forEach(f => toast(`${f.message || '添加失败'}：${f.url.slice(0, 60)}`, 'err'));
    if (res.added) form.querySelector('[name=urls]').value = '';
    page = 1;
    refresh();
    loadQuota();
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
  }
}

async function refresh() {
  try {
    const data = await api(`/api/offline/tasks?page=${page}`);
    pageCount = Math.max(1, data.page_count || 1);
    document.getElementById('page-info').textContent = `${page} / ${pageCount} 页 · 共 ${data.count || 0} 个任务`;
    renderTasks(data.tasks || []);
  } catch (err) {
    if (err.status !== 401) toast(err.message, 'err');
    stopOffline();
  }
}

function renderTasks(tasks) {
  const tbody = document.getElementById('offline-tbody');
  if (!tasks.length) {
    tbody.innerHTML = '<tr><td colspan="5" class="muted center">暂无任务</td></tr>';
    return;
  }
  tbody.innerHTML = tasks.map(t => {
    const [text, cls] = statusMap[String(t.status)] || [`状态${t.status}`, ''];
    const pct = (t.percentDone || 0).toFixed(1);
    return `<tr>
      <td class="name" title="${esc(t.name)}">${esc(t.name)}</td>
      <td>${fmtSize(t.size)}</td>
      <td><div class="progress" style="width:90px"><i style="width:${pct}%"></i></div>${pct}%</td>
      <td><span class="badge ${cls}">${text}</span></td>
      <td>
        <button class="btn sm" data-hash="${esc(t.info_hash)}" data-del="0">删任务</button>
        <button class="btn sm danger" data-hash="${esc(t.info_hash)}" data-del="1">删任务+文件</button>
      </td>
    </tr>`;
  }).join('');
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
    document.getElementById('quota-info').textContent =
      `配额：剩余 ${q.surplus} / 共 ${q.count}`;
  } catch { /* 配额非关键信息，失败不打扰 */ }
}
