// settings.js —— 配置查看与修改（保存后实时生效）。
// 表单字段用 petite-vue v-model 双向绑定（见 index.html v-scope="cfg"），提交前做 trim/类型转换。
import { api, toast } from './api.js';

const FIELDS = ['sync_path', 'strm_path', 'temp_path', 'strm_url', 'torrent_path', 'debounce_seconds', 'auth_username'];

// cfg 是 petite-vue 托管的响应式状态（reactive 包裹，导出即代理，v-model 双向绑定生效）。
export const cfg = window.PetiteVue.reactive({
  configReady: true,
  bannerText: '',
  // 表单字段（v-model 绑定；布尔/数字需显式类型）
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
      // 密码与 refresh_token 不回显明文：仅置空并给占位提示
      cfg.auth_password = '';
      cfg.refresh_token = '';
      // 已配置则不显示「未配置」误导文案，改为反映真实状态
      cfg.has_token = !!c.has_refresh_token;
      cfg.token_placeholder = cfg.has_token
        ? '已配置（留空保存保持不变）'
        : '填入后保存以更新/轮换 token';
      cfg.video_exts = (c.video_exts || []).join(', ');
      cfg.upload_exclude = (c.upload_exclude || []).join(', ');

      if (c.config_ready) {
        cfg.configReady = true;
      } else {
        cfg.configReady = false;
        cfg.bannerText = `⚠️ 配置不完整，同步未启动。待补齐：${(c.missing_fields || []).join('、')}`;
      }
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
      // 有输入才提交；留空表示保持不变（后端跳过校验，不改动 token）
      refresh_token: cfg.refresh_token.trim(),
    };
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
      await cfg.load(); // 重新拉取（清空密码框、刷新占位提示与横幅）
    } catch (err) {
      toast(err.message, 'err');
    } finally {
      if (btn) btn.disabled = false;
    }
  },
});
