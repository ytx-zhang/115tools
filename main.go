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
	// 1. 设置信号监听
	sigChan := make(chan os.Signal, 1)
	// 监听 SIGTERM (Docker 停止) 和 SIGINT (Ctrl+C)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	// 初始化配置
	open115.LoadConfig()
	// 启动 Emby 302 代理服务
	go emby302.StartEmby302()

	// 2. 设置路由
	mux := http.NewServeMux()

	// 首页
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./index.html")
	})

	// 状态查询接口
	type GlobalStatus struct {
		Sync syncFile.SyncStatus `json:"sync"`
		Strm addStrm.StrmStatus  `json:"strm"`
	}

	// SSE 实时日志推送
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				status := GlobalStatus{
					Sync: syncFile.GetStatus(),
					Strm: addStrm.GetStatus(),
				}
				data, _ := json.Marshal(status)
				fmt.Fprintf(w, "data: %s\n\n", data)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	})

	mainCtx, mainCancel := context.WithCancel(context.Background())
	// mainWg 只负责追踪顶层任务（Sync 或 AddStrm）的整体运行状态
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

	// 3. 配置 HTTP Server
	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 40 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 4. 异步处理信号，以便在收到信号时关闭服务器
	go func() {
		sig := <-sigChan
		log.Printf("收到系统信号: %v,准备退出...", sig)

		mainCancel()

		// B. 告知 HTTP 服务器停止接收新请求
		// 设置一个较短的超时，如果现有 HTTP 请求处理太慢，强制关闭
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("强制关闭 HTTP 服务器: %v", err)
		}
	}()

	// 5. 启动服务器（阻塞）
	log.Printf("服务器启动在 :8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		// http.ErrServerClosed 是调用 Shutdown 后的正常返回，不应被视为错误
		log.Fatalf("服务器异常退出: %v", err)
	}
	// 只有当服务器 Shutdown 后，代码才会执行到这里
	log.Printf("正在等待后台任务完成...")
	mainWg.Wait() // 阻塞，直到 syncFile 和 addStrm 彻底清理完数据库和文件
	log.Printf("程序已安全退出。")
}
