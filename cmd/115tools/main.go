// 115tools 主程序：115 网盘 ↔ 本地媒体库 同步工具。
//
// 组合根（Composition Root）：main 只负责「装配一切」，不含业务逻辑。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // 内嵌时区数据库：Docker alpine 下 TZ 生效依赖它

	"github.com/ytx-zhang/115tools/internal/cache"
	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/engine"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/webui"
)

func main() {
	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dataDir := flag.String("data", "/app/data", "数据目录（配置与数据库存放处）")
	port := flag.String("port", "8080", "Web 管理面板端口")
	flag.Parse()

	setupLogging()

	// 1. 配置（全局设置 + 任务集合；v1 结构在此自动迁移并备份）
	cfg, err := conf.New(filepath.Join(*dataDir, "config.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置加载失败:", err)
		os.Exit(1)
	}

	// 2. 持久化库（sync.db：索引 + 活动流）。程序只认 sync.db，不再处理任何旧数据文件。
	st, err := store.New(filepath.Join(*dataDir, "sync.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "数据文件初始化失败:", err)
		os.Exit(1)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			slog.Warn("关闭数据文件失败", "错误", cerr)
		}
	}()
	if err := st.Compact(appCtx); err != nil {
		slog.WarnContext(appCtx, "数据文件压缩失败", "错误", err)
	}

	// 3. 115 客户端（纯装配，无网络请求）
	api := drive.NewClient(cfg)

	// 4. 透传缓存（cache_dir 全局可配；未配置时兜底 <dataDir>/cache）
	cacheDir := cfg.Settings.CacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(*dataDir, "cache")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "缓存目录创建失败:", err)
		os.Exit(1)
	}
	localCache := cache.New(cacheDir, time.Duration(cfg.Settings.CacheRetentionDays)*24*time.Hour)
	go localCache.StartCleaner(appCtx, cache.SweepInterval)

	// 5. 任务引擎（全局单队列）+ 状态广播中心
	hub := webui.NewHub()
	var wg sync.WaitGroup
	eng := engine.New(api, cfg, st, cacheDir, localCache, hub.Publish, appCtx, &wg)

	// 6. Web 服务
	mux := http.NewServeMux()
	server := webui.Register(mux, webui.Deps{
		AppCtx: appCtx,
		Conf:   cfg,
		Engine: eng,
		Store:  st,
		Pan:    api,
		Cache:  localCache,
		Hub:    hub,
	})

	// 7. Web 服务先启动：初始化可能耗时数分钟（首次构建云端索引），不能挡住端口监听
	httpServer := &http.Server{Addr: ":" + *port, Handler: mux}
	go func() {
		slog.InfoContext(appCtx, "Web 服务启动", "地址", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.ErrorContext(appCtx, "Web 服务异常退出", "错误", err)
		}
	}()

	// 8. 初始化（后台执行：配置完备且凭证有效才启动引擎，过程与失败都进程序日志）
	go func() {
		if err := bootstrap(appCtx, cfg, api, eng); err != nil {
			server.ReportInitError("初始化失败: " + err.Error())
		}
	}()

	// 9. 等待退出信号，优雅关闭
	<-appCtx.Done()
	slog.InfoContext(appCtx, "正在优雅关闭...")
	eng.Shutdown()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.WarnContext(appCtx, "HTTP 关闭超时", "错误", err)
	}
	wg.Wait()
	slog.InfoContext(appCtx, "已退出")
}

// bootstrap 启动时的完整初始化：配置校验 → 凭证验证 → 刷新守护 → 引擎启动。
func bootstrap(ctx context.Context, cfg *conf.Config, api *drive.Client, eng *engine.Engine) error {
	if !cfg.Status().Ready {
		return fmt.Errorf("配置不完整：%s", strings.Join(cfg.Status().Missing, "、"))
	}
	if _, err := api.Verify(ctx, ""); err != nil {
		return fmt.Errorf("登录凭证验证失败: %w", err)
	}
	// 令牌刷新是请求路径上的懒刷新（见 drive/auth.go），无需常驻守护
	return eng.EnsureRunning()
}

// setupLogging 配置 slog → stdout（docker 负责存储与轮转）。
// 级别跟随环境变量 LOG_LEVEL（DEBUG/INFO/WARN/ERROR，缺省/非法回退 INFO）。
func setupLogging() {
	var lvl slog.Level
	switch strings.ToUpper(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "DEBUG":
		lvl = slog.LevelDebug
	case "WARN", "WARNING":
		lvl = slog.LevelWarn
	case "ERROR":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))
}
