// Package web 提供管理面板 HTTP 层：登录鉴权、配置、离线下载、任务触发与状态推送。
// /download 直链接口不在本包注册（位于 main，供 Emby 免验证使用）。
// 所有业务逻辑经 init.Broker 代理，web 不直接导入 sync/config/drive。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/init"
	"github.com/ytx-zhang/115tools/internal/logs"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

//go:embed all:static
var staticFS embed.FS

// Deps 由 main 注入的依赖。Broker 聚合所有模块，web 仅通过它交互。
type Deps struct {
	Broker *broker.Broker
	AppCtx context.Context
	Wg     *sync.WaitGroup
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

	// 公开接口（登录/会话探测无需鉴权）
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/me", s.handleMe)

	// 受保护接口
	protected := map[string]http.HandlerFunc{
		"POST /api/logout":          s.handleLogout,
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

// registerStatic 注册前端页面与静态资源（公开访问；接口层单独鉴权）。
func (s *Server) registerStatic(mux *http.ServeMux) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		logs.Error(logs.ModuleWeb, "静态资源目录缺失", "错误信息", err)
		return
	}
	indexData, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		logs.Error(logs.ModuleWeb, "读取 index.html 失败", "错误信息", err)
		indexData = []byte("<h1>index.html missing</h1>")
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexData)
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
}

// ──── HTTP 辅助 ────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, a...)})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	return dec.Decode(v)
}

// clientIP 返回真实客户端 IP（支持反代：XFF/X-Real-IP/X-Client-IP → RemoteAddr）。
func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if before, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(before)
		}
		return xff
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	addr := r.RemoteAddr
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		addr = addr[:i]
	}
	return strings.Trim(addr, "[]")
}
