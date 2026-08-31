// Package webui 提供 HTTP 层：管理面板（登录鉴权、配置、任务中心、离线下载、本地缓存）
// 与 SSE 状态流。依赖经组合根注入，不反向依赖其它包。
package webui

import (
	"context"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ytx-zhang/115tools/internal/cache"
	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/engine"
	"github.com/ytx-zhang/115tools/internal/index"
	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
	"github.com/ytx-zhang/115tools/internal/relay"
)

//go:embed all:static
var staticFS embed.FS

// Deps 组合根注入的依赖。
type Deps struct {
	AppCtx  context.Context
	Conf    *conf.Config
	Engine  *engine.Engine
	Journal *journal.Store
	Pan     *pan.Client
	Cache   *cache.Cache
	Index   *index.Index
	Hub     *Hub
}

// Server 管理面板 HTTP 服务。
type Server struct {
	Deps
	sessions sessionStore
	initErr  atomic.Pointer[string]

	reloadMu      sync.Mutex // 串行化后台的「启动引擎 + 重建任务」
	reloadPending bool       // 已有待执行的重建（连续保存合并为一轮）
}

// Register 注册全部路由到 mux。
func Register(mux *http.ServeMux, d Deps) *Server {
	s := &Server{Deps: d}
	s.registerStatic(mux)

	// /download 直链（Emby 依赖，免鉴权）
	redirector := relay.NewRedirector(d.Pan, d.Cache)
	mux.Handle("GET /download", http.HandlerFunc(redirector.RedirectToRealURL))

	// 公开接口
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/version", handleVersion)

	// 受保护接口
	protected := map[string]http.HandlerFunc{
		"POST /api/logout":                    s.handleLogout,
		"GET /api/overview":                   s.handleOverview,
		"GET /api/events":                     s.handleEvents,
		"GET /api/settings":                   s.handleGetSettings,
		"PUT /api/settings":                   s.handleSaveSettings,
		"GET /api/tasks":                      s.handleListTasks,
		"POST /api/tasks":                     s.handleCreateTask,
		"PUT /api/tasks/{id}":                 s.handleUpdateTask,
		"DELETE /api/tasks/{id}":              s.handleDeleteTask,
		"POST /api/tasks/{id}/start":          s.handleStartTask,
		"POST /api/tasks/{id}/stop":           s.handleStopTask,
		"GET /api/tasks/{id}/runs":            s.handleTaskRuns,
		"DELETE /api/tasks/{id}/runs":         s.handleClearTaskRuns,
		"GET /api/tasks/{id}/runs/{seq}/logs": s.handleTaskRunLogs,
		"GET /api/system-logs":                s.handleSystemLogs,
		"DELETE /api/system-logs":             s.handleClearSystemLogs,
		"GET /api/offline/tasks":              s.handleOfflineTasks,
		"GET /api/offline/quota":              s.handleOfflineQuota,
		"POST /api/offline/add":               s.handleOfflineAdd,
		"POST /api/offline/torrent":           s.handleOfflineTorrent,
		"POST /api/offline/delete":            s.handleOfflineDelete,
		"POST /api/offline/clear":             s.handleOfflineClear,
		"GET /api/cache":                      s.handleCacheList,
		"POST /api/cache/delete":              s.handleCacheDelete,
	}
	for pattern, h := range protected {
		mux.Handle(pattern, s.protect(h))
	}
	return s
}

// SetInitError 设置初始化错误（供 SSE 推送）。
func (s *Server) SetInitError(msg string) { s.initErr.Store(&msg) }

// ReportInitError 上报初始化/重建失败：落横幅文案 + 进程序日志 + 广播（前端顶部立即显示）。
func (s *Server) ReportInitError(msg string) {
	s.SetInitError(msg)
	journal.Error(s.AppCtx, msg)
	if s.Hub != nil {
		s.Hub.Publish()
	}
}

// startEngineAsync 后台启动引擎并重建任务单元（保存配置后调用，不阻塞 HTTP 请求）。
//
// 引擎初始化/重建可能耗时数分钟（首次构建云端索引），挂在保存请求上会让页面卡死；
// 进度与结果走程序日志 + SSE（任务卡片显示「初始化中」，失败时顶部横幅）。
// 已有待执行的重建时直接合并返回——配置已落盘，待执行那轮会读到最新配置。
func (s *Server) startEngineAsync() {
	// 配置已完备：确保常驻令牌刷新守护在跑。覆盖「启动时不 Ready、之后经 UI 保存 token」的路径
	//（该路径不绕 bootstrap，否则守护永不拉起）；StartRefreshDaemon 幂等，重复调用无副作用。
	pan.StartRefreshDaemon(s.AppCtx, s.Conf)

	s.reloadMu.Lock()
	if s.reloadPending {
		s.reloadMu.Unlock()
		return
	}
	s.reloadPending = true
	s.reloadMu.Unlock()

	go func() {
		defer func() {
			s.reloadMu.Lock()
			s.reloadPending = false
			s.reloadMu.Unlock()
		}()
		if err := s.Engine.EnsureRunning(); err != nil {
			s.ReportInitError("初始化失败: " + err.Error())
			return
		}
		if err := s.Engine.ReloadAll(); err != nil {
			s.ReportInitError("重建任务失败: " + err.Error())
		}
	}()
}

func (s *Server) getInitError() string {
	if p := s.initErr.Load(); p != nil {
		return *p
	}
	return ""
}

// registerStatic 注册前端页面与静态资源（no-cache + 内容指纹 ETag）。
func (s *Server) registerStatic(mux *http.ServeMux) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		journal.Error(context.Background(), "静态资源目录缺失", "错误", err)
		return
	}
	indexData, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		journal.Error(context.Background(), "读取 index.html 失败", "错误", err)
		indexData = []byte("<h1>index.html missing</h1>")
	}

	// index.html 已在上面读过，walk 时直接复用，避免重复 I/O 与重复 SHA-1
	etags := make(map[string]string, 16)
	_ = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data := indexData
		if path != "index.html" {
			var rerr error
			if data, rerr = fs.ReadFile(sub, path); rerr != nil {
				return rerr
			}
		}
		h := sha1.Sum(data)
		etags[path] = `"` + hex.EncodeToString(h[:]) + `"`
		return nil
	})

	indexHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		etag := etags["index.html"]
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if _, err := w.Write(indexData); err != nil {
			journal.Warn(r.Context(), "写入首页响应失败", "错误", err)
		}
	})
	mux.Handle("GET /{$}", indexHandler)

	fileServer := http.StripPrefix("/static/", http.FileServerFS(sub))
	staticHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if etag, ok := etags[strings.TrimPrefix(r.URL.Path, "/static/")]; ok {
			w.Header().Set("ETag", etag)
		}
		fileServer.ServeHTTP(w, r)
	})
	mux.Handle("GET /static/", staticHandler)
}

// ──── HTTP 辅助 ────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.MarshalWrite(w, v); err != nil {
		journal.Warn(context.Background(), "写入JSON响应失败", "状态码", code, "错误", err)
	}
}

func writeOK(w http.ResponseWriter, code int) {
	writeJSON(w, code, map[string]bool{"ok": true})
}

func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, a...)})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.UnmarshalRead(http.MaxBytesReader(w, r.Body, 1<<20), v)
}

// clientIP 返回真实客户端 IP（支持反代）。
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
	if before, _, ok := strings.CutLast(addr, ":"); ok {
		addr = before
	}
	return strings.Trim(addr, "[]")
}
