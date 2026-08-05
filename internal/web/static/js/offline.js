// offline.js —— 离线下载：添加任务、任务列表轮询、删除/清空、配额。
// 任务表格用 petite-vue v-for 声明式渲染（见 index.html v-scope="off"），自动转义文本；
// 表单提交/删除/清空的事件绑定仍走 bindOnce（离线下载无 SSE，命令式更直接）。
import { api, toast, toastError, fmtSize } from './api.js';

let timer = null;

const POLL_INTERVAL = 5000;
const FAIL_URL_LEN = 60;
const TORRENT_SIZE_THRESHOLD = 0;

const statusMap = {
  '-1': ['失败', 'err'],
  '0': ['分配中', 'warn'],
  '1': ['下载中', 'run'],
  '2': ['完成', 'ok'],
};

// off 是 petite-vue 托管的响应式状态（reactive 包裹，导出即代理，refresh 写入即刷新表格）。
export const off = window.PetiteVue.reactive({
  tasks: [],
  page: 1,
  pageCount: 1,
  count: 0,
  quota: '',
  get pageInfo() { return `${off.page} / ${off.pageCount} 页 · 共 ${off.count} 个任务`; },

  prev() { if (off.page > 1) { off.page--; off.refresh(); } },
  next() { if (off.page < off.pageCount) { off.page++; off.refresh(); } },

  async refresh() {
    try {
      const data = await api(`/api/offline/tasks?page=${off.page}`);
      off.pageCount = Math.max(1, data.page_count || 1);
      off.count = data.count || 0;
      this.tasks = (data.tasks || []).map(t => {
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
});

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

let bound = false;
function bindOnce() {
  if (bound) return;
  bound = true;

  document.getElementById('offline-add-form').addEventListener('submit', addTasks);
  document.getElementById('offline-refresh').onclick = () => { off.refresh(); loadQuota(); };

  document.querySelectorAll('[data-clear]').forEach(btn => {
    btn.onclick = async () => {
      if (!confirm(`确认${btn.textContent.trim()}？`)) return;
      try {
        await api('/api/offline/clear', { method: 'POST', body: { flag: +btn.dataset.clear } });
        toast('已清除', 'ok');
        off.page = 1;
        off.refresh();
      } catch (err) { toastError(err); }
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
      off.refresh();
    } catch (err) { toastError(err); }
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
  if (torrentFile instanceof File && torrentFile.size > TORRENT_SIZE_THRESHOLD) {
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
    off.quota = `配额：剩余 ${q.surplus} / 共 ${q.count}`;
  } catch { /* 配额非关键信息，失败不打扰 */ }
}
