// settings.js —— 配置查看与修改（保存后完整重新初始化同步器）。
// 表单字段用 petite-vue v-model 双向绑定（见 index.html v-scope="cfg"），提交前做 trim/类型转换。
import { api, toast } from './api.js';

export const cfg = window.PetiteVue.reactive({
  refresh_token: '',
  sync_path: '',
  strm_path: '',
  temp_path: '',
  strm_url: '',
  torrent_path: '',
  debounce_seconds: 0,
  video_exts: '',
  upload_exclude: '',
  cron_enabled: true,
  cron_interval_hours: 12,
  auth_username: '',
  auth_password: '',
  has_token: false,
  token_placeholder: '填入后保存以更新/轮换 token',

  async load() {
    try {
      const c = await api('/api/config');
      cfg.sync_path = c.sync_path ?? '';
      cfg.strm_path = c.strm_path ?? '';
      this.temp_path = c.temp_path ?? '';
      this.strm_url = c.strm_url ?? '';
      this.torrent_path = c.torrent_path ?? '';
      this.debounce_seconds = c.debounce_seconds ?? 0;
      this.cron_enabled = c.cron?.enabled !== false;
      this.cron_interval_hours = c.cron?.interval_hours || 12;
      this.auth_username = c.auth_username ?? '';
      cfg.auth_password = '';
      cfg.refresh_token = '';
      cfg.has_token = !!c.has_refresh_token;
      cfg.token_placeholder = cfg.has_token
        ? '已配置（留空保存保持不变）'
        : '填入后保存以更新/轮换 token';
      cfg.video_exts = (c.video_exts || []).join(', ');
      cfg.upload_exclude = (c.upload_exclude || []).join(', ');
    } catch (err) {
      toast(err.message, 'err');
    }
  },

  async save(e) {
    e?.preventDefault();
    const btn = document.querySelector('#config-form [type=submit]');
    if (btn) btn.disabled = true;
    const body = {
      sync_path: cfg.sync_path.trim(),
      strm_path: cfg.strm_path.trim(),
      temp_path: cfg.temp_path.trim(),
      strm_url: cfg.strm_url.trim(),
      torrent_path: cfg.torrent_path.trim(),
      debounce_seconds: +cfg.debounce_seconds || 0,
      cron: {
        enabled: cfg.cron_enabled,
        interval_hours: +cfg.cron_interval_hours || 12,
      },
      video_exts: cfg.video_exts.split(',').map(s => s.trim()).filter(Boolean),
      upload_exclude: cfg.upload_exclude.split(',').map(s => s.trim()).filter(Boolean),
      auth_username: cfg.auth_username.trim(),
      auth_password: cfg.auth_password,
      refresh_token: cfg.refresh_token.trim(),
    };
    try {
      const res = await api('/api/config', { method: 'PUT', body });
      if (res.ok) {
        toast('配置已保存，同步器已重启', 'ok');
      } else {
        toast(res.error || '保存失败', 'err');
      }
      await cfg.load();
    } catch (err) {
      toast(err.message, 'err');
    } finally {
      if (btn) btn.disabled = false;
    }
  },
});
