// 115tools 主程序：115 网盘 ↔ 本地媒体库 同步工具。
// main 只负责装配各包，不含业务逻辑。
package main

import (
	"context"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logstream"
	"github.com/ytx-zhang/115tools/internal/strmServer"
	"github.com/ytx-zhang/115tools/internal/syncFile"
	"github.com/ytx-zhang/115tools/internal/web"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
)

func main() {
	hub := logstream.NewHub()
	logstream.Setup(hub)

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// 1. 加载配置与 115 API 客户端（配置文件损坏才致命退出）
	cfg, err := config.New("/app/data/config.yaml")
	if err != nil {
		slog.Error("[CONFIG] 配置文件损坏", "错误信息", err)
		return
	}
	apiClient := drive.New115Drive(cfg)

	// 2. /download 直链先注册并立即监听（Emby 依赖，免鉴权）
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download", strmServer.New(apiClient).RedirectToRealURL)
	server := &http.Server{Addr: ":8080", Handler: mux}
	wg.Go(func() {
		slog.Info("HTTP服务启动在 :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			stop()
			slog.Error("服务器异常退出", "错误信息", err)
		}
	})

	// 3. 初始化数据库（启动时压缩一次回收空洞页）
	boltDB, err := db.New(`/app/data/files.db`)
	if err != nil {
		slog.Error("数据库初始化失败", "错误信息", err)
		return
	}
	defer boltDB.Close()
	if err := boltDB.Compact(); err != nil {
		slog.Warn("数据库压缩失败", "错误信息", err)
	}

	// 4. 创建 Runner + 注册管理面板（配置不完整面板也可用）
	runner := syncFile.NewRunner(appCtx, cfg, apiClient, boltDB, &wg)
	web.Register(mux, web.Deps{
		Cfg: cfg, Api: apiClient, AppCtx: appCtx, Wg: &wg, Hub: hub, Sync: runner,
	})

	// 5. 配置完整才启动同步器，否则仅 Warn（面板补齐后由 web 保存逻辑拉起）
	if cfg.IsSyncReady() {
		if err := runner.Start(); err != nil {
			slog.Error("[初始化] 同步器启动失败", "错误信息", err)
		}
	} else {
		slog.Warn("[CONFIG] 配置不完整，同步未启动", "缺失项", cfg.RequiredMissing())
	}

	// 6. 优雅关闭
	<-appCtx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("强制关闭 HTTP 服务器", "错误信息", err)
	}
	slog.Debug("正在等待后台任务完成...")
	wg.Wait()
	slog.Info("程序已安全退出。")
}
