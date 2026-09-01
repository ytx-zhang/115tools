// cache.js —— 本地缓存（视图包，import() 按需加载）：列表、多选删除。

import { api, el, fmtBytes, fmtTime, toast, fromTemplate, els } from './api.js';

let bound = false;

export function initCache() {
  if (!bound) { bindOnce(); bound = true; }
  refresh();
}

async function refresh() {
  try {
    const data = await api('/api/cache');
    renderList(data.items || []);
    els('cache-summary').textContent = `${data.count || 0} 项 · ${fmtBytes(data.total_size)}`;
  } catch (err) { toast(err.message, 'err'); }
}

function renderList(items) {
  const tbody = els('cache-tbody');
  const delBtn = els('cache-delete-selected');
  const checkAll = els('cache-check-all');
  delBtn.disabled = true;
  checkAll.checked = false;
  if (!items.length) {
    const tr = el('tr');
    const td = el('td', 'table-empty', '暂无缓存项');
    td.colSpan = 4;
    tr.append(td);
    tbody.replaceChildren(tr);
    return;
  }
  tbody.replaceChildren(...items.map((it) => {
    const tr = fromTemplate('tpl-cache-row', {
      '.c-name': it.name,
      '.c-pc': it.pickcode,
      '.c-size': fmtBytes(it.size),
      '.c-exp': fmtTime(it.expires_at),
    });
    tr.querySelector('.cache-check').value = it.pickcode;
    return tr;
  }));
}

const selected = () => [...document.querySelectorAll('.cache-check:checked')].map((c) => c.value);

function bindOnce() {
  els('cache-refresh').addEventListener('click', refresh);
  els('cache-check-all').addEventListener('change', (e) => {
    document.querySelectorAll('.cache-check').forEach((c) => { c.checked = e.target.checked; });
    els('cache-delete-selected').disabled = selected().length === 0;
  });
  els('cache-tbody').addEventListener('change', () => {
    els('cache-delete-selected').disabled = selected().length === 0;
  });
  els('cache-delete-selected').addEventListener('click', async () => {
    const codes = selected();
    if (!codes.length) return;
    if (!confirm(`确认删除选中的 ${codes.length} 项缓存？`)) return;
    try {
      const r = await api('/api/cache/delete', { method: 'POST', body: JSON.stringify({ pickcodes: codes }) });
      toast(`已删除 ${r.deleted} 项`, 'ok');
      refresh();
    } catch (err) { toast(err.message, 'err'); }
  });
}
