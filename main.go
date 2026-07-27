// 115tools 主程序：115 网盘 ↔ 本地媒体库 同步工具。
//
// main 只负责「装配」——把各包创建出来并接好线，不含任何业务逻辑：
//  1. 日志：logstream.Setup 配好 slog（stdout + 面板日志管道）；
//  2. 配置：加载 config 并创建 115 API 客户端（缺失自动降级，不致命退出）；
//  3. HTTP：先注册 /download（Emby 播放视频的直链，免鉴权）并立即监听；
//  4. 数据库：初始化 DB 并压缩一次回收空洞页；
//  5. 同步：创建 Runner 管理同步器生命周期（含热重载），提前注册管理面板；
//  6. 启动：配置就绪才启动同步器，否则面板照常可用；
//  7. 退出：收到中断信号后优雅关闭（先停 HTTP，再等全部后台协程收尾）。
package main

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
	"115tools/logstream"
	"115tools/strmServer"
	"115tools/syncFile"
	"115tools/web"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // 嵌入时区数据库，无需系统 tzdata
)

func main() {
	// 日志管道：hub 是面板「日志」卡片的数据源，显式创建并注入各处（无全局状态）。
	hub := logstream.NewHub()
	logstream.Setup(hub)

	// 全局信号：收到中断/终止信号时取消所有任务并优雅退出
	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// 1. 加载配置与 115 API 客户端。
	//    配置文件缺失会自动生成模板（降级继续运行，不致命退出）；
	//    仅当文件存在但解析损坏时才返回错误（需用户介入）。
	cfg, err := config.New("/app/data/config.yaml")
	if err != nil {
		slog.Error("[CONFIG] 配置文件损坏，无法解析", "错误信息", err)
		return
	}
	cfg.SyncEmbyIgnore() // 启动即生成/刷新 Emby 排除文件（SyncPath 就绪时生效）

	apiClient := drive.New115Drive(cfg)

	// 2. 先注册 /download 并立即启动 HTTP 服务
	//    /download 是 Emby 播放视频的直链依赖，必须尽早可用且【不做登录验证】，
	//    因此在其余管理路由之前启动、且不经过 web 包的鉴权中间件。
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

	// 3. 初始化数据库。
	boltDB, err := db.New(`/app/data/files.db`)
	if err != nil {
		slog.Error("数据库初始化失败", "错误信息", err)
		return
	}
	defer boltDB.Close()

	// 启动时压缩一次数据库，回收删除/重写产生的空洞页。
	if err := boltDB.Compact(); err != nil {
		slog.Warn("数据库压缩失败", "错误信息", err)
	}

	// 4. 创建 Runner（此时【不立即启动】同步器）。
	//    Runner 管理同步器生命周期，配置变更时热重载使其实时生效。
	//    即便配置不完整，也先创建好，使管理面板可注册并可用。
	runner := syncFile.NewRunner(appCtx, cfg, apiClient, boltDB, &wg)

	// 5. 提前注册管理面板路由（登录鉴权 + 配置 + 离线下载 + 任务触发）。
	//    与旧版不同，这里不等待同步器初始化完成：配置缺失时面板仍可用，
	//    用户可在面板补齐配置，保存后由 web 层自动拉起同步器。
	web.Register(mux, web.Deps{
		Cfg:         cfg,
		Api:         apiClient,
		AppCtx:      appCtx,
		Wg:          &wg,
		Hub:         hub,
		StatsNotify: runner.StatsNotify(),
		Syncer:      runner.Current,
		TaskCtx:     runner.TaskCtx,
		Reload:      runner.Reload,
	})

	// 6. 配置完整才启动同步器；否则仅 Warn 列出缺失项、不致命退出，
	//    面板照常可用。补齐后由 web 保存逻辑（Reload）拉起同步器。
	if cfg.IsSyncReady() {
		if err := runner.Start(); err != nil {
			slog.Error("[初始化] 同步器启动失败，请在面板检查配置或稍后重试", "错误信息", err)
		}
	} else {
		slog.Warn("[CONFIG] 配置不完整，同步未启动，请在管理面板补齐后保存",
			"缺失项", cfg.RequiredMissing())
	}

	// 7. 等待退出信号并优雅关闭
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
