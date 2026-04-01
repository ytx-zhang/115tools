package main

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
	"115tools/emby302"
	"115tools/strmServer"
	"115tools/syncFile"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
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
	wg.Go(func() {
		startNginx(ctx)
	})
	cfg, err := config.New("/app/data/config.yaml")
	if err != nil {
		slog.Error("[CONFIG] 配置文件错误", "错误信息", err)
		return
	}
	apiClient := drive.New115Drive(cfg)

	mux := http.NewServeMux()
	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	strmServer := strmServer.New(apiClient)
	mux.HandleFunc("GET /download", strmServer.RedirectToRealURL)

	wg.Go(func() {
		slog.Info("HTTP服务启动在 :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			mainCancel()
			slog.Error("服务器异常退出", "错误信息", err)
			return
		}
	})
	embyURL := os.Getenv("EMBY_URL")
	if embyURL != "" {
		wg.Go(func() {
			emby302.StartEmby302(ctx, embyURL)
		})
	}

	boltDB, err := db.New(`/app/data/files.db`)
	if err != nil {
		mainCancel()
		slog.Error("数据库初始化失败", "错误信息", err)
		return
	}
	defer boltDB.Close()

	syncFile, err := syncFile.New(ctx, cfg, apiClient, boltDB, &wg)
	if err != nil {
		slog.Error("初始化同步失败", "错误信息", err)
		return
	}

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
				syncFile.CloudSyncStats.GetStatus(),
				syncFile.AddStrmStats.GetStatus(),
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
		wg.Go(func() {
			syncFile.StartCloudSync(ctx)
		})
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /stopsync", func(w http.ResponseWriter, r *http.Request) {
		wg.Go(func() {
			syncFile.StopCloudSync()
		})
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /strm", func(w http.ResponseWriter, r *http.Request) {
		wg.Go(func() {
			syncFile.StartAddStrm(ctx)
		})
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /stopstrm", func(w http.ResponseWriter, r *http.Request) {
		wg.Go(func() {
			syncFile.StopAddStrm()
		})
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
				return slog.String(slog.TimeKey, a.Value.Time().Format("15:04:05.000"))
			}
			return a
		},
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))
}

// startNginx 负责启动并监控 Nginx 进程
func startNginx(ctx context.Context) {
	// 确保缓存目录存在
	os.MkdirAll("/app/data/cache", 0755)

	// -g "daemon off;" 确保 Nginx 前台运行，这样 Go 才能捕获其输出
	cmd := exec.CommandContext(ctx, "nginx", "-g", "daemon off;")

	// 将 Nginx 的标准输出和错误输出直接导向 Go 的标准输出
	// 这样在 docker logs 里就能直接看到 Nginx 日志
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	slog.Debug("[NGINX] 正在启动缓存代理...")
	if err := cmd.Start(); err != nil {
		slog.Error("[NGINX] 启动失败", "错误", err)
		return
	}
	slog.Info("[NGINX] 启动完成")

	// 进程启动后，cmd.Wait 会在进程结束或 ctx 被取消时返回
	// CommandContext 会在 ctx Done 时自动向 Nginx 发送 SIGKILL
	// 如果想更优雅，可以在 ctx.Done() 后手动发送 SIGQUIT，但通常 SIGTERM 足够
	err := cmd.Wait()
	if err != nil && !strings.Contains(err.Error(), "signal: killed") {
		slog.Warn("[NGINX] 进程已退出", "状态", err)
	} else {
		slog.Info("[NGINX] 进程已安全关闭")
	}
}
