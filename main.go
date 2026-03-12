package main

import (
	"115tools/addStrm"
	"115tools/config"
	"115tools/db"
	"115tools/emby302"
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
)

//go:embed index.html
var indexHTML embed.FS

func main() {
	setupLogger()
	ctx, mainCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer mainCancel()
	var wg sync.WaitGroup

	if err := config.LoadConfig(ctx, "/app/data/config.yaml"); err != nil {
		slog.Error("加载配置失败", "错误信息", err)
		return
	}
	wg.Go(func() { config.StartRefresh(ctx) })

	mux := http.NewServeMux()
	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}
	mux.HandleFunc("GET /download", strmServer.RedirectToRealURL)

	wg.Go(func() {
		slog.Info("HTTP服务启动在 :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("服务器异常退出", "错误信息", err)
		}
	})

	wg.Go(func() { emby302.StartEmby302(ctx) })

	db.Init()
	defer db.Close()

	if err := syncFile.InitSync(ctx); err != nil {
		slog.Error("初始化同步失败", "错误信息", err)
		return
	}
	wg.Go(func() { syncFile.StartWatch(ctx, &wg) })

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		data, _ := indexHTML.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		f, ok := w.(http.Flusher)
		send := func() {
			data := fmt.Sprintf(`{"sync":%s,"strm":%s}`,
				syncFile.GetStatus(),
				addStrm.GetStatus(),
			)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if ok {
				f.Flush()
			}
		}
		send()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				send()
			}
		}
	})

	mux.HandleFunc("GET /sync", func(w http.ResponseWriter, r *http.Request) {
		wg.Go(func() { syncFile.StartCloudSync(ctx) })
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /stopsync", func(w http.ResponseWriter, r *http.Request) {
		syncFile.StopCloudSync()
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /strm", func(w http.ResponseWriter, r *http.Request) {
		wg.Go(func() { addStrm.StartAddStrm(ctx) })
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /stopstrm", func(w http.ResponseWriter, r *http.Request) {
		addStrm.StopAddStrm()
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("强制关闭 HTTP 服务器", "错误信息", err)
	}
	slog.Info("正在等待后台任务完成...")
	wg.Wait()
	slog.Info("程序已安全退出。")
}
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
				return slog.String(slog.TimeKey, a.Value.Time().Format("15:04:05"))
			}
			return a
		},
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))
}
