// cache.js —— 本地缓存：列表、多选删除。

import { api, el, fmtBytes, fmtTime, toast } from './api.js';

let bound = false;

export function initCache() {
  if (!bound) { bindOnce(); bound = true; }
  refresh();
}

async function refresh() {
  try {
    const data = await api('/api/cache');
    renderList(data.items || []);
    document.getElementById('cache-summary').textContent = `${data.count || 0} 项 · ${fmtBytes(data.total_size)}`;
  } catch (err) {
    toast(err.message, 'err');
  }
}

function renderList(items) {
  const tbody = document.getElementById('cache-tbody');
  tbody.innerHTML = '';
  document.getElementById('cache-delete-selected').disabled = true;
  document.getElementById('cache-check-all').checked = false;
  if (!items.length) {
    const tr = el('tr');
    const td = el('td');
    td.colSpan = 4;
    td.className = 'table-empty';
    td.textContent = '暂无缓存项';
    tr.appendChild(td);
    tbody.appendChild(tr);
    return;
  }
  items.forEach((it) => {
    const tr = el('tr');
    const check = el('td');
    const c = document.createElement('input');
    c.type = 'checkbox';
    c.className = 'cache-check';
    c.value = it.pickcode;
    check.appendChild(c);
    const name = el('td');
    const nm = el('div');
    nm.textContent = it.name;
    const pc = el('div', 'muted');
    pc.textContent = it.pickcode;
    pc.style.cssText = 'font-size:11px;font-family:monospace';
    name.append(nm, pc);
    const size = el('td', null, fmtBytes(it.size));
    const expire = el('td', null, fmtTime(it.expires_at));
    tr.append(check, name, size, expire);
    tbody.appendChild(tr);
  });
}

function selected() {
  return [...document.querySelectorAll('.cache-check:checked')].map((c) => c.value);
}

function bindOnce() {
  document.getElementById('cache-refresh').addEventListener('click', refresh);

  document.getElementById('cache-check-all').addEventListener('change', (e) => {
    document.querySelectorAll('.cache-check').forEach((c) => { c.checked = e.target.checked; });
    document.getElementById('cache-delete-selected').disabled = selected().length === 0;
  });

  document.getElementById('cache-tbody').addEventListener('change', () => {
    document.getElementById('cache-delete-selected').disabled = selected().length === 0;
  });

  document.getElementById('cache-delete-selected').addEventListener('click', async () => {
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
