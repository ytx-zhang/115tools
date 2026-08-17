// 115tools 主程序：115 网盘 ↔ 本地媒体库 同步工具。
//
// 组合根（Composition Root）：main 只负责「装配一切」，不含任何业务逻辑。
// 程序运行流程看下方组装顺序——每个步骤上方一行注释说明「为什么按此顺序」。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // 内嵌时区数据库：Docker alpine 下 TZ 生效依赖它

	"github.com/ytx-zhang/115tools/internal/app"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/web"
)

func main() {
	// 1. 信号上下文：整个程序的生命周期都挂在它上面（Ctrl+C / SIGTERM 触发优雅退出）
	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 2. 命令行参数（数据目录 / 端口）
	dataDir := flag.String("data", "/app/data", "数据目录（配置文件与索引库存放处）")
	port := flag.String("port", "8080", "Web 管理面板端口")
	flag.Parse()
	configPath := *dataDir + "/config.json"
	dbPath := *dataDir + "/files.db"

	// 3. 先建日志中心（Hub），再 Setup 全局 slog——日志要在装配早期就可用
	hub := logs.NewHub()
	logs.Setup(hub)

	// 4. 加载配置（配置文件损坏才致命退出；不存在则自动创建空白骨架）
	cfg, err := config.New(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置加载失败:", err)
		os.Exit(1)
	}

	// 5. 初始化索引存储（bbolt），启动时压缩一次回收空洞页
	database, err := store.New(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "索引库初始化失败:", err)
		os.Exit(1)
	}
	defer database.Close()
	if err := database.Compact(); err != nil {
		logs.Warn(logs.ModuleSystem, "数据库压缩失败", "错误信息", err)
	}

	// 6. 创建 115 驱动（开放平台 refresh_token，纯装配无网络请求）
	api := drive.NewClient(cfg)

	// 7. 组装应用编排层（组合根本体：持全部全局依赖，串联 config/db/api/sync/logs）
	var wg sync.WaitGroup
	application := app.New(cfg, api, database, hub, appCtx, &wg)

	// 8. 注册 HTTP 路由并启动监听（管理面板 + /download 直链）
	mux := http.NewServeMux()
	web.Register(mux, web.Deps{App: application, AppCtx: appCtx})
	server := &http.Server{
		Addr:    ":" + *port,
		Handler: mux,
	}
	go func() {
		logs.Info(logs.ModuleSystem, "Web 服务启动", "地址", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logs.Error(logs.ModuleSystem, "Web 服务异常退出", "错误", err)
		}
	}()

	// 9. 应用初始化：配置校验 → 凭证验证 → 同步器构建（建目录/索引 → 启动 watch+cron 任务）
	if err := application.Initialize(); err != nil {
		logs.Error(logs.ModuleSystem, "初始化失败", "错误", err)
	} else {
		logs.Info(logs.ModuleSystem, "115tools 启动完成")
	}

	// 10. 等待退出信号，优雅关闭：先停 HTTP（拒绝新请求），再取消应用 ctx 停同步任务
	<-appCtx.Done()
	logs.Info(logs.ModuleSystem, "正在优雅关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logs.Warn(logs.ModuleSystem, "HTTP 关闭超时", "错误", err)
	}
	wg.Wait()
	logs.Info(logs.ModuleSystem, "已退出")
}
