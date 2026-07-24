// Package web 提供管理面板的 HTTP 层：
// 登录鉴权、配置管理、离线下载、同步任务触发与状态推送、静态资源。
// 注意：/download 直链接口不在本包注册（位于 main，供 Emby 免验证使用）。
package web

import (
	"115tools/config"
	"115tools/drive"
	"115tools/logstream"
	"115tools/syncFile"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// Deps 由 main 注入的依赖。
type Deps struct {
	Cfg    *config.Config
	Api    *drive.Open115
	AppCtx context.Context
	Wg     *sync.WaitGroup

	// Hub 为运行日志 SSE 共享的日志缓冲（由 main 创建并注入，避免全局状态）。
	Hub *logstream.Hub
	// StatsNotify 是状态变更通知通道（由 syncRunner 创建并跨热重载共享），
	// 供 /api/status SSE 阻塞监听。
	StatsNotify <-chan struct{}

	// Syncer 返回当前同步器实例；配置热重载期间/失败时可能为 nil。
	Syncer func() *syncFile.SyncFile
	// TaskCtx 返回触发任务所用 ctx（随热重载被取消，绑定当前同步器生命周期）。
	TaskCtx func() context.Context
	// Reload 热重载同步器，使路径类配置实时生效（同步阻塞，调用方自行异步）。
	Reload func()
}

// Server 管理面板 HTTP 服务。
type Server struct {
	Deps
	sessions sessionStore
}

// Register 注册全部管理路由到 mux。
func Register(mux *http.ServeMux, d Deps) *Server {
	s := &Server{Deps: d, sessions: newSessionStore()}

	s.registerStatic(mux)

	// 公开接口（登录本身与会话探测无需鉴权）
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/me", s.handleMe)

	// 受保护接口
	protected := map[string]http.HandlerFunc{
		"POST /api/logout":          s.handleLogout,
		"GET /api/status":           s.handleStatus,
		"GET /api/logs":             s.handleLogs,
		"POST /api/logs/clear":      s.handleLogsClear,
		"POST /api/task/{name}":     s.handleTaskStart,
		"DELETE /api/task/{name}":   s.handleTaskStop,
		"GET /api/config":           s.handleGetConfig,
		"PUT /api/config":           s.handleSaveConfig,
		"GET /api/offline/tasks":    s.handleOfflineTasks,
		"GET /api/offline/quota":    s.handleOfflineQuota,
		"POST /api/offline/add":     s.handleOfflineAdd,
		"POST /api/offline/torrent": s.handleOfflineTorrent,
		"POST /api/offline/delete":  s.handleOfflineDelete,
		"POST /api/offline/clear":   s.handleOfflineClear,
	}
	for pattern, h := range protected {
		mux.Handle(pattern, s.protect(h))
	}
	return s
}

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 输出统一错误结构。
func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, a...)})
}

// readJSON 解析请求体 JSON，限制 1MB 防滥用。
func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// clientIP 返回请求的真实客户端 IP。支持反代场景：优先取反代写入的
// X-Forwarded-For（首个条目）、X-Real-IP、X-Client-IP，最后回退到
// TCP 对端地址（去掉可能的端口）。反代应确保这些头可信（如仅接受来自内网上游）。
func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		// X-Forwarded-For 可能为逗号分隔的多跳列表，取首个（原始客户端）。
		if before, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(before)
		}
		return xff
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	if xcip := strings.TrimSpace(r.Header.Get("X-Client-IP")); xcip != "" {
		return xcip
	}
	// 回退到 TCP 对端地址，去掉端口与 IPv6 方括号，如 [::1]:53666 或 10.10.10.1:53666
	addr := r.RemoteAddr
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		addr = addr[:i]
	}
	addr = strings.Trim(addr, "[]")
	return addr
}
