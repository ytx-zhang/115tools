// settings.js —— 配置查看与修改（保存后实时生效）
import { api, toast } from './api.js';

const FIELDS = ['sync_path', 'strm_path', 'temp_path', 'strm_url', 'torrent_path', 'debounce_seconds', 'cron_interval_hours', 'auth_username'];

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
    // 定时全量同步开关（复选框）：默认开启
    form.elements['cron_enabled'].checked = cfg.cron_enabled !== false;
    // 密码与 refresh_token 不回显明文：仅清空并给占位提示
    form.elements['auth_password'].value = '';
    form.elements['auth_password'].placeholder =
      cfg.has_password ? '已设置，留空则保持不变' : '未设置';
    form.elements['refresh_token'].value = '';
    form.elements['refresh_token'].placeholder =
      cfg.has_refresh_token ? '已配置，留空则保持不变' : '未配置';
    // 配置不完整时，在设置页顶部给出提示与缺失项。
    const sb = document.getElementById('settings-banner');
    if (cfg.config_ready) {
      sb.hidden = true;
    } else {
      sb.hidden = false;
      sb.textContent = `⚠️ 配置不完整，同步未启动。待补齐：${(cfg.missing_fields || []).join('、')}`;
    }
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
    debounce_seconds: +form.elements['debounce_seconds'].value || 0,
    cron_enabled: form.elements['cron_enabled'].checked,
    cron_interval_hours: +form.elements['cron_interval_hours'].value || 12,
    auth_username: form.elements['auth_username'].value.trim(),
    auth_password: form.elements['auth_password'].value,
    // 有输入才提交；留空表示保持不变（后端跳过校验，不改动 token）
    refresh_token: form.elements['refresh_token'].value.trim(),
  };

  btn.disabled = true;
  try {
    const res = await api('/api/config', { method: 'PUT', body });
    if (res.started) {
      toast('已保存，同步器已启动', 'ok');
    } else if (res.ready) {
      toast(res.reloading ? '已保存，同步器热重载中…' : '已保存，实时生效', 'ok');
    } else {
      const miss = (res.missing || []).join('、');
      toast(`已保存（配置仍不完整，未启动同步）待补齐：${miss}`, 'err');
    }
    load(); // 重新拉取（清空密码框、刷新占位提示与横幅）
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
  }
}
