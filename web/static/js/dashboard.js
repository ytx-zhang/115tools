// dashboard.js —— 任务状态实时展示（SSE）与启停控制。
// 日志部分（连接/渲染/过滤）已独立到 logs.js，这里只做状态卡片。
import { api, toast } from './api.js';
import { initLogs, stopLogs } from './logs.js';

let es = null; // 状态 SSE

export function initDashboard() {
  connect();
  initLogs();
  document.querySelectorAll('[data-role=toggle]').forEach(btn => {
    btn.onclick = () => toggleTask(btn);
  });
}

export function stopDashboard() {
  es?.close();
  es = null;
  stopLogs();
}

// connect 打开状态 SSE。事件驱动：服务端在任务进度变化/热重载时推送快照。
function connect() {
  es?.close();
  es = new EventSource('/api/status');
  es.onmessage = e => {
    try { render(JSON.parse(e.data)); } catch { /* 忽略损坏帧 */ }
  };
  es.onerror = () => {
    // 断线（含 401 / 服务重启）：3 秒后重连；若会话失效，api() 会触发登录页
    es?.close();
    es = null;
    setTimeout(async () => {
      if (!document.getElementById('view-dashboard').hidden) {
        try { await api('/api/me'); connect(); } catch { /* 401 已由事件处理 */ }
      }
    }, 3000);
  };
}

const startText = { sync: '开始同步', strm: '开始生成' };

// render 刷新整个仪表盘：配置/热重载横幅 + 两张任务卡片。
// 失败原因明细由日志卡片（按「错误」过滤）统一展示，状态卡片只显示任务进度。
function render(data) {
  // 配置不完整：优先提示补齐（比热重载更关键）。
  const cfgBanner = document.getElementById('config-banner');
  cfgBanner.hidden = data.config_ready;
  if (!data.config_ready) {
    const miss = (data.missing || []).join('、');
    cfgBanner.textContent = `⚠️ 配置不完整，同步未启动，请到「设置」补齐：${miss}`;
  }
  // 热重载中横幅：仅当配置已就绪、但同步器暂未就绪时显示。
  document.getElementById('reload-banner').hidden = data.ready || !data.config_ready;
  renderCard('sync', data.sync);
  renderCard('strm', data.strm);
}

function renderCard(name, st) {
  const card = document.getElementById(`card-${name}`);
  const q = role => card.querySelector(`[data-role=${role}]`);
  const running = !!st?.running;
  const total = st?.total || 0, done = st?.completed || 0;

  q('done').textContent = done;
  q('total').textContent = total;
  q('bar').style.width = total ? `${Math.min(100, done / total * 100)}%` : '0';

  const badge = q('badge');
  if (!st) { badge.textContent = '未就绪'; badge.className = 'badge warn'; }
  else if (running) { badge.textContent = '运行中'; badge.className = 'badge run'; }
  else { badge.textContent = '空闲'; badge.className = 'badge'; }

  const btn = q('toggle');
  btn.textContent = running ? '停 止' : startText[name];
  btn.classList.toggle('danger', running);
  btn.classList.toggle('primary', !running);
  btn.dataset.running = running ? '1' : '';
  btn.disabled = !st;
}

async function toggleTask(btn) {
  const name = btn.dataset.task;
  btn.disabled = true;
  try {
    if (btn.dataset.running) {
      await api(`/api/task/${name}`, { method: 'DELETE' });
      toast('已发送停止指令');
    } else {
      await api(`/api/task/${name}`, { method: 'POST' });
      toast('任务已启动', 'ok');
    }
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
  }
}
