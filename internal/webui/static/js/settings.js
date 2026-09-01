// settings.js —— 设置（视图包，import() 按需加载）：全局设置表单读写。

import { api, toast, els } from './api.js';
import { refreshOverview } from './main.js';

let bound = false;

export function initSettings() {
  if (!bound) { bindOnce(); bound = true; }
  load();
}

async function load() {
  try {
    const s = await api('/api/settings');
    const f = els('config-form').elements;
    f.strm_url.value = s.strm_url || '';
    f.temp_dir.value = s.temp_dir || '';
    f.cache_dir.value = s.cache_dir || '';
    f.cache_retention_days.value = s.cache_retention_days ?? 1;
    f.offline_dir.value = s.offline_dir || '';
    f.video_exts.value = (s.video_exts || []).join(', ');
    f.upload_exclude.value = (s.upload_exclude || []).join(', ');
    f.auth_username.value = s.auth_username || '';
    f.auth_password.value = '';
    f.refresh_token.value = '';
    f.refresh_token.placeholder = s.has_refresh_token ? '已配置（留空保持不变）' : '115 开放平台刷新令牌';
  } catch (err) { toast(err.message, 'err'); }
}

const splitList = (v) => v.split(',').map((s) => s.trim()).filter(Boolean);

function bindOnce() {
  els('config-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const f = e.target.elements;
    try {
      await api('/api/settings', {
        method: 'PUT',
        body: JSON.stringify({
          strm_url: f.strm_url.value.trim(),
          temp_dir: f.temp_dir.value.trim(),
          cache_dir: f.cache_dir.value.trim(),
          cache_retention_days: parseInt(f.cache_retention_days.value, 10) || 0,
          offline_dir: f.offline_dir.value.trim(),
          video_exts: splitList(f.video_exts.value),
          upload_exclude: splitList(f.upload_exclude.value),
          auth_username: f.auth_username.value.trim(),
          auth_password: f.auth_password.value,
          refresh_token: f.refresh_token.value.trim(),
        }),
      });
      toast('配置已保存', 'ok');
      f.auth_password.value = '';
      f.refresh_token.value = '';
      load();
      refreshOverview();   // 保存后主动拉状态快照，任务中心横幅立即反映最新配置
    } catch (err) { toast(err.message, 'err'); }
  });
}
