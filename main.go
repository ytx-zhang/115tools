package main

import (
	"115tools/addStrm"
	"115tools/config"
	"115tools/emby302"
	"115tools/strmServer"
	"115tools/syncFile"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.String(slog.TimeKey, a.Value.Time().Format("15:04:05"))
			}
			return a
		},
	}
	handler := slog.NewTextHandler(os.Stdout, opts)
	slog.SetDefault(slog.New(handler))

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	ctx, mainCancel := context.WithCancelCause(context.Background())
	var wg sync.WaitGroup

	if err := config.LoadConfig(ctx, &wg, "/app/data/config.yaml"); err != nil {
		slog.Error("加载配置失败", "错误信息", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./index.html")
	})

	mux.HandleFunc("GET /download", strmServer.RedirectToRealURL)

	type GlobalStatus struct {
		Sync syncFile.TaskStatsJSON `json:"sync"`
		Strm addStrm.TaskStatsJSON  `json:"strm"`
	}
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		f, ok := w.(http.Flusher)
		send := func() {
			status := GlobalStatus{
				Sync: syncFile.GetStatus(),
				Strm: addStrm.GetStatus(),
			}
			data, _ := json.Marshal(status)
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
		wg.Go(func() { syncFile.StartSync(ctx) })
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		wg.Go(func() { syncFile.StartSync(ctx) })
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /stopsync", func(w http.ResponseWriter, r *http.Request) {
		syncFile.StopSync()
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

	wg.Go(func() { emby302.StartEmby302(ctx) })

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		sig := <-sigChan
		mainCancel(fmt.Errorf("收到系统信号: %v,准备退出...", sig))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Warn("强制关闭 HTTP 服务器", "错误信息", err)
		}
	}()

	slog.Info("服务器启动在 :8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("服务器异常退出", "错误信息", err)
	}

	slog.Info("正在等待后台任务完成...")
	wg.Wait()
	slog.Info("程序已安全退出。")
}
