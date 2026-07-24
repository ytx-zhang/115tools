package web

import "net/http"

// 本文件是 SSE（Server-Sent Events，服务器推送）长连接的共用写样板，
// 供 /api/status（任务状态）与 /api/logs（运行日志）两个接口复用。
//
// 【可靠性铁律】以下每一条都踩过坑，修改时务必逐条保留：
//  1. 浏览器 EventSource 收到首个数据块（含注释帧）后才触发 onopen，
//     所以连接建立后要【先发 ": connected" 注释帧】再发送/回放数据；
//  2. 每 15 秒发 ": ping" 心跳注释帧，否则中间代理/网关因空闲超时断开（504）；
//     注释帧以 ":" 开头，浏览器忽略其内容，仅用于保活；
//  3. 写失败（w.Write 返回 error）必须立即 return 关闭流，【绝不再 Flush】——
//     向已损坏的 HTTP/2 流写异常帧会触发 ERR_HTTP2_PROTOCOL_ERROR，
//     并波及同一条连接上的其他 SSE 接口（两个接口多路复用同一条连接）；
//  4. 不使用 ResponseController.SetWriteDeadline（HTTP/2 下会波及整条连接）；
//  5. 不设置 Connection 头（HTTP/2 非法的逐跳头）；
//  6. http.Flusher.Flush() 没有返回值，流是否断开只能靠 w.Write 的 error 判断。

// sseWriter 封装一条 SSE 连接的写操作。
// 所有写方法返回 false 均表示「连接已断」，调用方应立即 return 结束处理函数。
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// newSSEWriter 初始化 SSE 响应头并返回写器。
// 返回 ok=false 表示该 ResponseWriter 不支持流式推送（极少见，调用方应报错返回）。
func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	// X-Accel-Buffering: no 告诉 Nginx 等反代不要缓冲，实时下发。
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	return &sseWriter{w: w, flusher: flusher}, true
}

// writeData 写出一帧数据（"data: <payload>\n\n"）。
func (s *sseWriter) writeData(payload string) bool {
	if _, err := s.w.Write([]byte("data: " + payload + "\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}

// writeComment 写出一帧注释（": <msg>\n\n"，浏览器忽略内容）。
// 用于 connected 确认与 ping 心跳（铁律 1、2）。
func (s *sseWriter) writeComment(msg string) bool {
	if _, err := s.w.Write([]byte(":" + msg + "\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}
