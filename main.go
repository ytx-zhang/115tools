package main

import (
	"115tools/addStrm"
	"115tools/emby302"
	"115tools/open115"
	"115tools/strmServer"
	"115tools/syncFile"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	open115.LoadConfig()

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
		fetchStatus := func() string {
			status := GlobalStatus{
				Sync: syncFile.GetStatus(),
				Strm: addStrm.GetStatus(),
			}
			data, _ := json.Marshal(status)
			return string(data)
		}
		send := func(jsonStr string) {
			fmt.Fprintf(w, "data: %s\n\n", jsonStr)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		lastJSON := fetchStatus()
		send(lastJSON)

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				currentJSON := fetchStatus()
				if currentJSON != lastJSON {
					send(currentJSON)
					lastJSON = currentJSON
				}
			}
		}
	})

	mainCtx, mainCancel := context.WithCancelCause(context.Background())
	var mainWg sync.WaitGroup
	mux.HandleFunc("GET /download", strmServer.RedirectToRealURL)

	mux.HandleFunc("GET /sync", func(w http.ResponseWriter, r *http.Request) {
		syncFile.StartSync(mainCtx, &mainWg)
		w.Header().Set("Cache-Control", "no-store")
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
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 40 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		sig := <-sigChan
		mainCancel(fmt.Errorf("收到系统信号: %v,准备退出...", sig))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("强制关闭 HTTP 服务器: %v", err)
		}
	}()

	log.Printf("服务器启动在 :8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务器异常退出: %v", err)
	}

	log.Printf("正在等待后台任务完成...")
	mainWg.Wait()
	log.Printf("程序已安全退出。")
}
