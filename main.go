package main

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
	"115tools/strmServer"
	"115tools/syncFile"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // 嵌入时区数据库，无需系统 tzdata
)

//go:embed index.html
var indexHTML embed.FS

func main() {
	setupLogger()

	// 全局信号：收到中断/终止信号时取消所有任务并优雅退出
	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// 1. 加载配置与 API 客户端
	cfg, err := config.New("/app/data/config.yaml")
	if err != nil {
		slog.Error("[CONFIG] 配置文件错误", "错误信息", err)
		return
	}

	apiClient := drive.New115Drive(cfg)

	// 2. 先注册 /download 并立即启动 HTTP 服务
	//    /download 是 Emby 播放视频的直链依赖，必须尽早可用，
	//    因此在其余较重的管理路由之前启动。
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download", strmServer.New(apiClient).RedirectToRealURL)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	wg.Go(func() {
		slog.Info("HTTP服务启动在 :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			stop()
			slog.Error("服务器异常退出", "错误信息", err)
		}
	})

	// 3. 初始化 DB 与同步器（可能较耗时）。此期间仅 /download 可用，
	//    管理/触发类路由在初始化完成后才注册，避免未完成初始化即被调用。
	boltDB, err := db.New(`/app/data/files.db`)
	if err != nil {
		slog.Error("数据库初始化失败", "错误信息", err)
		return
	}
	defer boltDB.Close()

	// 启动时压缩一次 bbolt 数据库，回收删除/重写产生的空洞页。
	if err := boltDB.Compact(); err != nil {
		slog.Warn("数据库压缩失败", "错误信息", err)
	}

	syncer, err := syncFile.New(appCtx, cfg, apiClient, boltDB, &wg)
	if err != nil {
		slog.Error("初始化同步失败", "错误信息", err)
		return
	}

	// 4. 初始化完成后，再注册其余管理/触发路由
	registerRoutes(mux, appCtx, &wg, syncer)

	// 5. 等待退出信号并优雅关闭
	<-appCtx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("强制关闭 HTTP 服务器", "错误信息", err)
	}
	slog.Info("正在等待后台任务完成...")
	wg.Wait()
	slog.Info("程序已安全退出。")
}

// registerRoutes 注册初始化完成后才开放的管理/触发类路由（不含 /download）。
func registerRoutes(mux *http.ServeMux, appCtx context.Context, wg *sync.WaitGroup, syncer *syncFile.SyncFile) {
	// 管理面板首页
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		data, _ := indexHTML.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// SSE：实时推送同步/生成状态
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		flusher, ok := w.(http.Flusher)

		send := func() {
			data := fmt.Sprintf(`{"sync":%s,"strm":%s}`,
				syncer.Cloud.Stats.GetStatus(),
				syncer.Strm.Stats.GetStatus(),
			)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if ok {
				flusher.Flush()
			}
		}

		send()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-appCtx.Done():
				return
			case <-ticker.C:
				send()
			}
		}
	})

	// 触发类接口：异步启动任务并立即返回 202
	trigger := func(task func()) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			wg.Go(task)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusAccepted)
		}
	}
	mux.HandleFunc("GET /sync", trigger(func() { syncer.StartCloudSync(appCtx) }))
	mux.HandleFunc("GET /stopsync", trigger(func() { syncer.StopCloudSync() }))
	mux.HandleFunc("GET /strm", trigger(func() { syncer.StartAddStrm(appCtx) }))
	mux.HandleFunc("GET /stopstrm", trigger(func() { syncer.StopAddStrm() }))
}

// setupLogger 根据 LOG_LEVEL 环境变量配置全局日志级别与格式。
func setupLogger() {
	levelStr := strings.ToUpper(os.Getenv("LOG_LEVEL"))
	var level slog.Level

	switch levelStr {
	case "DEBUG":
		level = slog.LevelDebug
	case "WARN":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.String(slog.TimeKey, a.Value.Time().Format("15:04:05.000"))
			}
			return a
		},
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))
}
