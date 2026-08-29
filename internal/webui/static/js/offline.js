// offline.js —— 离线下载：添加链接/种子、任务列表、分页、删除/清除。

import { api, el, fmtBytes, toast, btnWithIcon } from './api.js';

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

// loadDefaultDir 用全局设置的「离线下载默认目录」预填保存路径（用户已输入则不覆盖）。
async function loadDefaultDir() {
  try {
    const s = await api('/api/settings');
    const input = document.getElementById('offline-save-path');
    const dir = (s.offline_dir || '').trim();
    input.placeholder = dir ? `留空默认 ${dir}` : '留空默认云端根 /';
    if (!input.value && dir) input.value = dir;
  } catch { /* 静默：保持原样 */ }
}

async function refresh() {
  loadList(page);
  loadQuota();
}

async function loadQuota() {
  try {
    const q = await api('/api/offline/quota');
    document.getElementById('offline-quota').textContent = `配额 ${q.used}/${q.count}（剩 ${q.surplus}）`;
  } catch { document.getElementById('offline-quota').textContent = ''; }
}

async function loadList(p) {
  try {
    const data = await api(`/api/offline/tasks?page=${p}`);
    page = data.page || 0;
    pageCount = data.page_count || 1;
    renderList(data.tasks || []);
    document.getElementById('page-info').textContent = `第 ${page + 1} / ${pageCount} 页 · 共 ${data.count || 0} 个任务`;
  } catch (err) {
    toast(err.message, 'err');
  }
}

function renderList(tasks) {
  const tbody = document.getElementById('offline-tbody');
  tbody.innerHTML = '';
  if (!tasks.length) {
    const tr = el('tr');
    const td = el('td');
    td.colSpan = 5;
    td.className = 'table-empty';
    td.textContent = '暂无离线任务';
    tr.appendChild(td);
    tbody.appendChild(tr);
    return;
  }
  tasks.forEach((t) => {
    const tr = el('tr');
    const name = el('td');
    name.textContent = t.name;
    const size = el('td', null, fmtBytes(t.size));
    const prog = el('td');
    const bar = el('div', 'progress');
    const fill = el('i');
    fill.style.width = (t.percentDone || 0) + '%';
    bar.appendChild(fill);
    const pct = el('span', 'muted', ` ${(t.percentDone || 0).toFixed(1)}%`);
    prog.append(bar, pct);
    const st = el('td');
    st.appendChild(el('span', 'badge ' + (statusCls[t.status] || ''), statusLabel[t.status] ?? String(t.status)));
    const ops = el('td');
    const del = btnWithIcon('btn sm', '删除', '#i-trash', { hash: t.info_hash, act: 'del' });
    const delFiles = btnWithIcon('btn sm danger', '删除+源文件', '#i-trash', { hash: t.info_hash, act: 'delFiles' });
    ops.append(del, delFiles);
    tr.append(name, size, prog, st, ops);
    tbody.appendChild(tr);
  });
}

function bindOnce() {
  // 添加链接
  document.getElementById('offline-add-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const urls = fd.get('urls');
    const savePath = fd.get('save_path') || '';
    try {
      const r = await api('/api/offline/add', { method: 'POST', body: JSON.stringify({ urls, save_path: savePath }) });
      toast(`添加成功 ${r.added} 条`, 'ok');
      e.target.reset();
      refresh();
    } catch (err) { toast(err.message, 'err'); }
  });

  // 种子上传
  document.getElementById('offline-torrent').addEventListener('change', async (e) => {
    const file = e.target.files[0];
    if (!file) return;
    const fd = new FormData();
    fd.append('torrent', file);
    fd.append('save_path', document.getElementById('offline-save-path').value || '');
    try {
      const r = await api('/api/offline/torrent', { method: 'POST', body: fd });
      toast(r.added ? '种子任务已添加' : '种子任务添加失败', r.added ? 'ok' : 'err');
      e.target.value = '';
      refresh();
    } catch (err) { toast(err.message, 'err'); }
  });

  // 任务行操作（删除/删除+源文件）
  document.getElementById('offline-tbody').addEventListener('click', async (e) => {
    const btn = e.target.closest('button[data-act]');
    if (!btn) return;
    const hash = btn.dataset.hash;
    const deleteFiles = btn.dataset.act === 'delFiles';
    if (!confirm(`确认删除任务${deleteFiles ? '及其源文件' : ''}？`)) return;
    try {
      await api('/api/offline/delete', { method: 'POST', body: JSON.stringify({ info_hash: hash, delete_files: deleteFiles }) });
      toast('已删除', 'ok');
      refresh();
    } catch (err) { toast(err.message, 'err'); }
  });

  // 清除按钮
  document.querySelectorAll('#view-offline [data-clear]').forEach((b) =>
    b.addEventListener('click', async () => {
      const flag = parseInt(b.dataset.clear, 10);
      if (!confirm('确认清除对应任务？')) return;
      try {
        await api('/api/offline/clear', { method: 'POST', body: JSON.stringify({ flag }) });
        toast('已清除', 'ok');
        refresh();
      } catch (err) { toast(err.message, 'err'); }
    }));

  document.getElementById('offline-refresh').addEventListener('click', refresh);
  document.getElementById('page-prev').addEventListener('click', () => { if (page > 0) loadList(page - 1); });
  document.getElementById('page-next').addEventListener('click', () => { if (page < pageCount - 1) loadList(page + 1); });
}
