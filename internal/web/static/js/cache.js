// cache.js —— 本地透传缓存管理：列表展示（文件名主/pickcode 小字/大小/过期时间）、
// 复选框多选 + 批量删除。列表按文件名排序（后端已排好），无轮询，进入视图时刷新一次。
// 渲染用 <template id="cache-row"> 克隆重建 tbody；文本全部 textContent 写入避免 XSS。
import { api, toast, toastError, fmtSize } from './api.js';

const rowTpl = document.getElementById('cache-row');

let bound = false;

export function initCache() {
  bindOnce();
  refresh();
}

// ──── 渲染 ────

async function refresh() {
  try {
    const data = await api('/api/cache');
    render(data.items || [], data.total_size || 0);
  } catch (err) {
    toastError(err);
  }
}

function render(items, totalSize) {
  const tbody = document.getElementById('cache-tbody');
  if (!tbody) return;
  const all = document.getElementById('cache-check-all');
  if (all) all.checked = false;

  tbody.textContent = '';
  if (!items.length) {
    const tr = document.createElement('tr');
    tr.innerHTML = '<td colspan="4" class="muted center">暂无缓存</td>';
    tbody.appendChild(tr);
  } else {
    for (const it of items) {
      const frag = rowTpl.content.cloneNode(true);
      const cells = {};
      frag.querySelectorAll('[data-cell]').forEach(el => { cells[el.dataset.cell] = el; });
      cells.check.dataset.pickcode = it.pickcode;
      cells.name.textContent = it.name || '(未知文件名)';
      cells.name.title = it.name || '';
      cells.pickcode.textContent = it.pickcode;
      cells.size.textContent = fmtSize(it.size);
      cells.expire.textContent = fmtExpire(it.expires_at);
      cells.expire.title = it.cached_at ? '移入缓存 ' + fmtFull(it.cached_at) : '';
      tbody.appendChild(frag);
    }
  }

  const summary = document.getElementById('cache-summary');
  if (summary) summary.textContent = `${items.length} 项 · 共 ${fmtSize(totalSize)}`;
  updateActionState();
}

// fmtExpire 相对 + 绝对时间：如「3.2 天后 · 8月25日 12:00」，已过期显示「已过期」。
function fmtExpire(iso) {
  if (!iso) return '-';
  const t = new Date(iso);
  const diff = t.getTime() - Date.now();
  const abs = `${t.getMonth() + 1}月${t.getDate()}日 ${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}`;
  if (diff <= 0) return `已过期 · ${abs}`;
  const hours = diff / 3600000;
  return hours < 24 ? `${hours.toFixed(1)} 小时后 · ${abs}` : `${(hours / 24).toFixed(1)} 天后 · ${abs}`;
}

function fmtFull(iso) {
  const t = new Date(iso);
  const p = n => String(n).padStart(2, '0');
  return `${t.getFullYear()}-${p(t.getMonth() + 1)}-${p(t.getDate())} ${p(t.getHours())}:${p(t.getMinutes())}`;
}

// ──── 事件绑定（一次性，重复进入视图不重复绑定）────

function bindOnce() {
  if (bound) return;
  bound = true;

  document.getElementById('cache-refresh').onclick = refresh;
  document.getElementById('cache-check-all').addEventListener('change', toggleAll);
  document.getElementById('cache-delete-selected').onclick = deleteSelected;
  // 事件委托：行内复选框变化 → 更新「删除选中」可用态
  document.getElementById('cache-tbody').addEventListener('change', e => {
    if (e.target.matches('.cache-check')) updateActionState();
  });
}

function toggleAll(e) {
  document.querySelectorAll('.cache-check').forEach(cb => { cb.checked = e.target.checked; });
  updateActionState();
}

function selected() {
  return [...document.querySelectorAll('.cache-check:checked')].map(cb => cb.dataset.pickcode);
}

function updateActionState() {
  const btn = document.getElementById('cache-delete-selected');
  if (btn) btn.disabled = selected().length === 0;
}

async function deleteSelected() {
  const codes = selected();
  if (!codes.length) return;
  if (!confirm(`确认删除选中的 ${codes.length} 项本地缓存？删除后播放将回源 115 重新拉取。`)) return;
  try {
    const res = await api('/api/cache/delete', { method: 'POST', body: { pickcodes: codes } });
    toast(`已删除 ${res.deleted} 项`, 'ok');
    refresh();
  } catch (err) {
    toastError(err);
  }
}
