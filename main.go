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
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	mainCtx, mainCancel := context.WithCancelCause(context.Background())

	config.LoadConfig(mainCtx, "/app/data/config.yaml")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./index.html")
	})

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
			case <-mainCtx.Done(): // 核心：服务器关了，主动掐断日志流
				return
			case <-ticker.C:
				send()
			}
		}
	})

	mux.HandleFunc("GET /download", strmServer.RedirectToRealURL)

	var mainWg sync.WaitGroup
	mux.HandleFunc("GET /sync", func(w http.ResponseWriter, r *http.Request) {
		syncFile.StartSync(mainCtx, &mainWg)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		syncFile.StartSync(mainCtx, &mainWg)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /stopsync", func(w http.ResponseWriter, r *http.Request) {
		syncFile.StopSync()
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /strm", func(w http.ResponseWriter, r *http.Request) {
		addStrm.StartAddStrm(mainCtx, &mainWg)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /stopstrm", func(w http.ResponseWriter, r *http.Request) {
		addStrm.StopAddStrm()
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})

	go emby302.StartEmby302(mainCtx)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		sig := <-sigChan
		mainCancel(fmt.Errorf("收到系统信号: %v,准备退出...", sig))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Warn("强制关闭 HTTP 服务器", "error", err)
		}
	}()

	slog.Info("服务器启动在 :8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("服务器异常退出", "error", err)
	}

	slog.Info("正在等待后台任务完成...")
	mainWg.Wait()
	slog.Info("程序已安全退出。")
}
