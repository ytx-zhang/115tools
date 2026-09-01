// offline.js —— 离线下载（视图包，import() 按需加载）：添加链接/种子、任务列表、分页、删除/清除。

import { api, el, fmtBytes, toast, fromTemplate, els } from './api.js';

let page = 0;
let pageCount = 1;
let bound = false;

const statusLabel = { '-1': '失败', '0': '分配中', '1': '下载中', '2': '成功' };
const statusCls = { '-1': 'err', '0': 'warn', '1': 'run', '2': 'ok' };

export function initOffline() {
  if (!bound) { bindOnce(); bound = true; }
  loadDefaultDir();
  refresh();
}

async function loadDefaultDir() {
  try {
    const s = await api('/api/settings');
    const input = els('offline-save-path');
    const dir = (s.offline_dir || '').trim();
    input.placeholder = dir ? `留空默认 ${dir}` : '留空默认云端根 /';
    if (!input.value && dir) input.value = dir;
  } catch {}
}

async function refresh() {
  loadList(page);
  loadQuota();
}
async function loadQuota() {
  try {
    const q = await api('/api/offline/quota');
    els('offline-quota').textContent = `配额 ${q.used}/${q.count}（剩 ${q.surplus}）`;
  } catch { els('offline-quota').textContent = ''; }
}
async function loadList(p) {
  try {
    const data = await api(`/api/offline/tasks?page=${p}`);
    page = data.page || 0;
    pageCount = data.page_count || 1;
    renderList(data.tasks || []);
    els('page-info').textContent = `第 ${page + 1} / ${pageCount} 页 · 共 ${data.count || 0} 个任务`;
  } catch (err) { toast(err.message, 'err'); }
}

function renderList(tasks) {
  const tbody = els('offline-tbody');
  if (!tasks.length) { tbody.replaceChildren(emptyRow(5, '暂无离线任务')); return; }
  tbody.replaceChildren(...tasks.map((t) => {
    const tr = fromTemplate('tpl-offline-row', {
      '.c-name': t.name,
      '.c-size': fmtBytes(t.size),
      '.pct': ` ${(t.percentDone || 0).toFixed(1)}%`,
    });
    tr.querySelector('.progress i').style.width = (t.percentDone || 0) + '%';
    const badge = tr.querySelector('.badge');
    badge.className = 'badge ' + (statusCls[t.status] || '');
    badge.textContent = statusLabel[t.status] ?? String(t.status);
    tr.querySelectorAll('[data-act]').forEach((b) => { b.dataset.hash = t.info_hash; });
    return tr;
  }));
}
function emptyRow(colSpan, text) {
  const tr = el('tr');
  const td = el('td', 'table-empty', text);
  td.colSpan = colSpan;
  tr.append(td);
  return tr;
}

function bindOnce() {
  els('offline-add-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    try {
      const r = await api('/api/offline/add', { method: 'POST', body: JSON.stringify({ urls: fd.get('urls'), save_path: fd.get('save_path') || '' }) });
      toast(`添加成功 ${r.added} 条`, 'ok');
      e.target.reset();
      refresh();
    } catch (err) { toast(err.message, 'err'); }
  });
  els('offline-torrent').addEventListener('change', async (e) => {
    const file = e.target.files[0];
    if (!file) return;
    const fd = new FormData();
    fd.append('torrent', file);
    fd.append('save_path', els('offline-save-path').value || '');
    try {
      const r = await api('/api/offline/torrent', { method: 'POST', body: fd });
      toast(r.added ? '种子任务已添加' : '种子任务添加失败', r.added ? 'ok' : 'err');
      e.target.value = '';
      refresh();
    } catch (err) { toast(err.message, 'err'); }
  });
  els('offline-tbody').addEventListener('click', async (e) => {
    const btn = e.target.closest('button[data-act]');
    if (!btn) return;
    const deleteFiles = btn.dataset.act === 'delFiles';
    if (!confirm(`确认删除任务${deleteFiles ? '及其源文件' : ''}？`)) return;
    try {
      await api('/api/offline/delete', { method: 'POST', body: JSON.stringify({ info_hash: btn.dataset.hash, delete_files: deleteFiles }) });
      toast('已删除', 'ok');
      refresh();
    } catch (err) { toast(err.message, 'err'); }
  });
  document.querySelectorAll('#view-offline [data-clear]').forEach((b) =>
    b.addEventListener('click', async () => {
      if (!confirm('确认清除对应任务？')) return;
      try {
        await api('/api/offline/clear', { method: 'POST', body: JSON.stringify({ flag: parseInt(b.dataset.clear, 10) }) });
        toast('已清除', 'ok');
        refresh();
      } catch (err) { toast(err.message, 'err'); }
    }));
  els('offline-refresh').addEventListener('click', refresh);
  els('page-prev').addEventListener('click', () => { if (page > 0) loadList(page - 1); });
  els('page-next').addEventListener('click', () => { if (page < pageCount - 1) loadList(page + 1); });
}
