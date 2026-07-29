// sse.js —— SSE 公共连接：重连防护、会话校验、视图可见性守卫。
// dashboard.js 与 logs.js 共用，消除重复的 EventSource 重连样板。
import { api } from './api.js';

// connectSSE 打开一条 SSE 连接，内聚重连/会话/守卫逻辑。
// 调用方仅需提供 onMessage（必须）与 onOpen（可选）渲染回调。
// 返回 close 函数，离开视图时调用以主动断开。
export function connectSSE(url, { onMessage, onOpen }) {
  let es = null;
  let seq = 0; // 本连接序号，防重连回调误关已重建的新连接

  function open() {
    es?.close();
    es = new EventSource(url);
    const errSeq = ++seq;
    es.onmessage = e => {
      try { onMessage(JSON.parse(e.data)); } catch { /* 忽略损坏帧 */ }
    };
    if (onOpen) es.onopen = onOpen;
    es.onerror = () => {
      es?.close();
      es = null;
      setTimeout(async () => {
        if (errSeq !== seq) return; // 期间已重建过，放弃本次重连
        if (!document.getElementById('view-dashboard').hidden) {
          try { await api('/api/me'); open(); } catch { /* 401 已由事件处理 */ }
        }
      }, 3000);
    };
  }

  open();
  return () => { seq++; es?.close(); es = null; };
}
