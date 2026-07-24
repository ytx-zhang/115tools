// settings.js —— 配置查看与修改（保存后实时生效）
import { api, toast } from './api.js';

const FIELDS = ['sync_path', 'strm_path', 'temp_path', 'strm_url', 'torrent_path', 'settle_seconds', 'auth_username'];

export function initSettings() {
  bindOnce();
  load();
}

let bound = false;
function bindOnce() {
  if (bound) return;
  bound = true;
  document.getElementById('config-form').addEventListener('submit', save);
}

async function load() {
  try {
    const cfg = await api('/api/config');
    const form = document.getElementById('config-form');
    for (const f of FIELDS) form.elements[f].value = cfg[f] ?? '';
    form.elements['auth_password'].value = '';
    form.elements['auth_password'].placeholder =
      cfg.has_password ? '已设置，留空则保持不变' : '未设置';
  } catch (err) {
    toast(err.message, 'err');
  }
}

async function save(e) {
  e.preventDefault();
  const form = e.target;
  const btn = form.querySelector('[type=submit]');
  const body = {
    sync_path: form.elements['sync_path'].value.trim(),
    strm_path: form.elements['strm_path'].value.trim(),
    temp_path: form.elements['temp_path'].value.trim(),
    strm_url: form.elements['strm_url'].value.trim(),
    torrent_path: form.elements['torrent_path'].value.trim(),
    settle_seconds: +form.elements['settle_seconds'].value || 0,
    auth_username: form.elements['auth_username'].value.trim(),
    auth_password: form.elements['auth_password'].value,
  };

  btn.disabled = true;
  try {
    const res = await api('/api/config', { method: 'PUT', body });
    toast(res.reloading ? '已保存，同步器热重载中…' : '已保存，实时生效', 'ok');
    load(); // 重新拉取（清空密码框、刷新占位提示）
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
  }
}
