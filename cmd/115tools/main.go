// 115tools 主程序：115 网盘 ↔ 本地媒体库 同步工具（v2 全新重写）。
//
// 组合根（Composition Root）：main 只负责「装配一切」，不含业务逻辑。
package main

import (
	"context"
	"flag"
	"fmt"
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
	"github.com/ytx-zhang/115tools/internal/engine"
	"github.com/ytx-zhang/115tools/internal/index"
	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
	"github.com/ytx-zhang/115tools/internal/webui"
)

func main() {
	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dataDir := flag.String("data", "/app/data", "数据目录（配置与数据库存放处）")
	port := flag.String("port", "8080", "Web 管理面板端口")
	flag.Parse()

	// 1. 配置（全局设置 + 任务集合）
	cfg, err := conf.New(filepath.Join(*dataDir, "config.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置加载失败:", err)
		os.Exit(1)
	}

	// 2. 执行历史库（journal.db）+ 日志门面安装
	hist, err := journal.New(filepath.Join(*dataDir, "journal.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "历史库初始化失败:", err)
		os.Exit(1)
	}
	journal.Setup(hist)
	defer hist.Close()

	// 3. 路径索引库（index.db）
	idx, err := index.New(filepath.Join(*dataDir, "index.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "索引库初始化失败:", err)
		os.Exit(1)
	}
	defer idx.Close()
	if err := idx.Compact(appCtx); err != nil {
		journal.Warn(appCtx, "索引库压缩失败", "错误", err)
	}

	// 4. 115 客户端（纯装配，无网络请求）
	api := pan.NewClient(cfg)

	// 5. 透传缓存（cache_dir 全局可配；未配置时兜底 <dataDir>/cache）
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

	// 6. 任务引擎 + 状态广播中心
	hub := webui.NewHub()
	var wg sync.WaitGroup
	eng := engine.New(api, idx, cfg, hist, localCache, hub.Publish, appCtx, &wg)

	// 7. Web 服务
	mux := http.NewServeMux()
	server := webui.Register(mux, webui.Deps{
		AppCtx:  appCtx,
		Conf:    cfg,
		Engine:  eng,
		Journal: hist,
		Pan:     api,
		Cache:   localCache,
		Index:   idx,
		Hub:     hub,
	})

	// 8. 初始化（配置完备且凭证有效才启动引擎）
	if err := bootstrap(appCtx, cfg, api, eng); err != nil {
		server.SetInitError(err.Error())
		journal.Error(appCtx, "初始化失败", "错误", err)
	}

	httpServer := &http.Server{Addr: ":" + *port, Handler: mux}
	go func() {
		journal.Info(appCtx, "Web 服务启动", "地址", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			journal.Error(appCtx, "Web 服务异常退出", "错误", err)
		}
	}()

	// 9. 等待退出信号，优雅关闭
	<-appCtx.Done()
	journal.Info(appCtx, "正在优雅关闭...")
	eng.Shutdown()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		journal.Warn(appCtx, "HTTP 关闭超时", "错误", err)
	}
	wg.Wait()
	journal.Info(appCtx, "已退出")
}

// bootstrap 启动时的完整初始化：配置校验 → 凭证验证 → 刷新守护 → 引擎启动。
func bootstrap(ctx context.Context, cfg *conf.Config, api *pan.Client, eng *engine.Engine) error {
	if !cfg.Status().Ready {
		return fmt.Errorf("配置不完整：%s", strings.Join(cfg.Status().Missing, "、"))
	}
	if _, err := api.Verify(ctx, ""); err != nil {
		return fmt.Errorf("登录凭证验证失败: %w", err)
	}
	pan.StartRefreshDaemon(ctx, cfg)
	return eng.EnsureRunning()
}
