// settings.js —— 配置查看与修改（保存后完整重新初始化同步器）。
// 表单为普通 HTML 控件（单一数据源），load() 按 name 写入、save() 按 name 读取构造 PUT body，
// 提交前做 trim/类型转换；DOM 写入一律走 value/checked，天然防 XSS。
import { api, toast, toastError } from './api.js';

export const cfg = {
  has_token: false,
  token_placeholder: '填入后保存以更新/轮换 token',

  async load() {
    try {
      const c = await api('/api/config');
      fillForm(c);
    } catch (err) {
      toastError(err);
    }
  },

  async save(e) {
    e?.preventDefault();
    const btn = document.querySelector('#config-form [type=submit]');
    if (btn) btn.disabled = true;
    const body = {
      sync_path: get('sync_path').trim(),
      strm_path: get('strm_path').trim(),
      temp_path: get('temp_path').trim(),
      strm_url: get('strm_url').trim(),

      debounce_minutes: +get('debounce_minutes') || 0,
      cron: {
        enabled: el('cron_enabled')?.checked ?? false,
        interval_hours: +get('cron_interval_hours') || 12,
      },
      video_exts: get('video_exts').split(',').map(s => s.trim()).filter(Boolean),
      upload_exclude: get('upload_exclude').split(',').map(s => s.trim()).filter(Boolean),
      cache_retention_days: +get('cache_retention_days') || 0,
      auth_username: get('auth_username').trim(),
      auth_password: get('auth_password'),
      refresh_token: get('refresh_token').trim(),
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
      toastError(err);
    } finally {
      if (btn) btn.disabled = false;
    }
  },
};

function el(name) { return document.querySelector(`#config-form [name="${name}"]`); }
function get(name) { return el(name)?.value ?? ''; }
function set(name, val) { const input = el(name); if (input) input.value = val ?? ''; }

// 将服务端配置写入表单控件（密码类始终置空；token 输入框占位提示反映是否已配置）。
function fillForm(c) {
  set('refresh_token', '');
  set('sync_path', c.sync_path);
  set('strm_path', c.strm_path);
  set('temp_path', c.temp_path);
  set('strm_url', c.strm_url);

  set('debounce_minutes', c.debounce_minutes ?? 0);
  set('video_exts', (c.video_exts || []).join(', '));
  set('upload_exclude', (c.upload_exclude || []).join(', '));
  set('cache_retention_days', c.cache_retention_days ?? 1);
  set('cron_interval_hours', c.cron?.interval_hours || 12);
  set('auth_username', c.auth_username);
  set('auth_password', '');

  const cron = el('cron_enabled');
  if (cron) cron.checked = c.cron?.enabled !== false;

  cfg.has_token = !!c.has_refresh_token;
  cfg.token_placeholder = cfg.has_token
    ? '已配置（留空保存保持不变）'
    : '填入后保存以更新/轮换 token';
  const token = el('refresh_token');
  if (token) token.placeholder = cfg.token_placeholder;
}

// 表单提交入口（script type=module 加载时 DOM 已就绪）
document.getElementById('config-form')?.addEventListener('submit', e => {
  e.preventDefault();
  cfg.save();
});
