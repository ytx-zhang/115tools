// 115tools 主程序：115 网盘 ↔ 本地媒体库 同步工具。
// main 只负责装配各包，不含业务逻辑。
package main

import (
	"context"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	initbr "github.com/ytx-zhang/115tools/internal/init"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/web"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
)

func main() {
	hub := logs.NewHub()
	logs.Setup(hub)

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// 1. 加载配置与 115 API 客户端（配置文件损坏才致命退出）
	cfg, err := config.New("/app/data/config.yaml")
	if err != nil {
		logs.Error(logs.ModuleSystem, "配置文件损坏", "错误信息", err)
		return
	}
	apiClient := drive.New115Drive(cfg)

	// 2. /download 直链先注册并立即监听（Emby 依赖，免鉴权）
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download", drive.NewRedirector(apiClient).RedirectToRealURL)
	server := &http.Server{Addr: ":8080", Handler: mux}
	wg.Go(func() {
		logs.Info(logs.ModuleSystem, "HTTP服务启动在 :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			stop()
			logs.Error(logs.ModuleSystem, "服务器异常退出", "错误信息", err)
		}
	})

	// 3. 初始化数据库（启动时压缩一次回收空洞页）
	boltDB, err := db.New(`/app/data/files.db`)
	if err != nil {
		logs.Error(logs.ModuleSystem, "数据库初始化失败", "错误信息", err)
		return
	}
	defer boltDB.Close()
	if err := boltDB.Compact(); err != nil {
		logs.Warn(logs.ModuleSystem, "数据库压缩失败", "错误信息", err)
	}

	// 4. 创建 Broker（聚合所有模块，统一编排初始化与前后端交互）
	br := initbr.New(cfg, apiClient, boltDB, hub, appCtx, &wg)
	web.Register(mux, web.Deps{Broker: br, AppCtx: appCtx, Wg: &wg})

	// 5. 统一初始化（配置校验 → Token 验证 → 同步器重建）
	if err := br.Initialize(); err != nil {
		logs.Warn(logs.ModuleSystem, "初始化未完成", "原因", err)
		// 不退出——管理面板仍可用，初始化错误经 SSE 推前端展示
	}

	// 6. 优雅关闭
	<-appCtx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logs.Warn(logs.ModuleSystem, "强制关闭 HTTP 服务器", "错误信息", err)
	}
	logs.Debug(logs.ModuleSystem, "正在等待后台任务完成...")
	wg.Wait()
	logs.Info(logs.ModuleSystem, "程序已安全退出。")
}
