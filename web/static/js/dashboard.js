// dashboard.js —— 任务状态实时展示（SSE）与启停控制。
import { api, toast } from './api.js';
import { connectSSE } from './sse.js';
import { initLogs, stopLogs } from './logs.js';

let closeStatus = null; // 状态 SSE 的关闭函数

export function initDashboard() {
  closeStatus = connectSSE('/api/status', { onMessage: render });
  initLogs();
  document.querySelectorAll('[data-role=toggle]').forEach(btn => {
    btn.onclick = () => toggleTask(btn);
  });
}

export function stopDashboard() {
  closeStatus?.();
  closeStatus = null;
  stopLogs();
}

const startText = { sync: '开始同步', strm: '开始生成' };

function render(data) {
  const cfgBanner = document.getElementById('config-banner');
  cfgBanner.hidden = data.config_ready;
  if (!data.config_ready) {
    cfgBanner.textContent = `⚠️ 配置不完整，同步未启动，请到「设置」补齐：${(data.missing || []).join('、')}`;
  }
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
