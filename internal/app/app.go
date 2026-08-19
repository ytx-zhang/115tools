// Package app 是应用编排层（组合根本体，原 init/broker 重组）。
//
// 职责：
//   - 持有全部全局依赖（配置/驱动/索引/日志中心/同步器/token 守护）；
//   - 提供 main 启动装配与 web 层交互的唯一入口；
//   - 编排：Initialize（校验→验证→构建同步器）、ApplyConfig（热更新→重建同步器）、
//     Snapshot（状态聚合）、任务启停。
//
// 薄代理按职责拆文件：offline.go（离线下载透传）、hub.go（日志中心代理），
// web 层只能经本包交互（依赖单向 web → app → drive/sync/logs）。
//
// 设计：app 是组合根——它知道各模块怎么协作；sync/web/drive 之间不直接互相调用。
package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/status"
	"github.com/ytx-zhang/115tools/internal/store"
	synclib "github.com/ytx-zhang/115tools/internal/sync"
)

// App 聚合所有模块，充当前后端交互的唯一入口。
type App struct {
	cfg     *config.Config
	API     *drive.Client // 115 驱动实例（启动时构造，固定不变；导出供 web 直链重定向器按类型引用）
	db      *store.Store
	hub     *logs.Hub
	syncer  *synclib.Syncer
	appCtx  context.Context
	appWg   *sync.WaitGroup
	initErr atomic.Pointer[string] // 初始化错误信息（单值替换，无锁）
}

// New 构造 App 并注入状态回调到同步器。
func New(cfg *config.Config, api *drive.Client, database *store.Store, hub *logs.Hub, appCtx context.Context, appWg *sync.WaitGroup) *App {
	b := &App{
		cfg:    cfg,
		API:    api,
		db:     database,
		hub:    hub,
		appCtx: appCtx,
		appWg:  appWg,
	}
	b.syncer = synclib.NewSyncer(appCtx, cfg, api, database, appWg)
	b.syncer.SetStatusCallback(b.publishStatus)
	// 启动 refresh_token 常驻刷新守护：配置了 token 即持续刷新防止过期
	drive.StartRefreshDaemon(appCtx, cfg)
	return b
}

// ──── 生命周期 ────

// Initialize 启动时完整初始化流程：配置校验 → 凭证验证 → 同步器构建。
func (b *App) Initialize() error {
	status := b.cfg.Status()
	if !status.Ready {
		msg := "配置不完整：" + strings.Join(status.Missing, "、")
		logs.Error(logs.ModuleSystem, "初始化失败", "原因", msg)
		return b.failInit(msg)
	}

	start := time.Now()

	api := b.API
	logs.Info(logs.ModuleSystem, "验证登录凭证...")
	// Verify 内部调 /open/user/info 验证 token，并带出账户概况
	info, err := api.Verify(b.appCtx, "")
	if err != nil {
		msg := "登录凭证验证失败: " + err.Error()
		logs.Error(logs.ModuleSystem, "初始化失败", "原因", msg)
		return b.failInit(msg)
	}
	logs.Info(logs.ModuleSystem, "登录凭证验证通过", "账户", info.String(), "耗时", time.Since(start).String())

	syncStart := time.Now()
	walked, err := b.syncer.Initialize()
	if err != nil {
		return b.failInit(err.Error())
	}
	logs.Info(logs.ModuleSystem, "同步器初始化完成", "构建索引", walked, "耗时", time.Since(syncStart).String())

	logs.Info(logs.ModuleSystem, "初始化完成", "总耗时", time.Since(start).String())
	b.setInitErr("")
	return nil
}

// ApplyConfig 保存配置并重建同步器：路径变更时清理旧 DB 记录，sync 专注于业务逻辑。
func (b *App) ApplyConfig(ctx context.Context, req config.Editable) error {
	syncStart := time.Now()
	logs.Info(logs.ModuleSystem, "开始应用配置")

	oldSyncPath := b.cfg.SyncPath
	oldTempPath := b.cfg.TempPath
	oldStrmPath := b.cfg.StrmPath
	oldStrmUrl := b.cfg.StrmUrl

	// 验证新 refresh_token（空表示未修改，跳过）
	if err := b.verifyCredential(ctx, req); err != nil {
		return err
	}
	if err := b.cfg.Update(req); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	// 路径变更 → 清理旧 DB 记录
	b.clearMovedPath("同步根目录", oldSyncPath, b.cfg.SyncPath)
	b.clearMovedPath("回收目录", oldTempPath, b.cfg.TempPath)
	b.clearMovedPath("STRM目录", oldStrmPath, b.cfg.StrmPath)

	walked, err := b.syncer.Initialize()
	if err != nil {
		return b.failInit(err.Error())
	}
	logs.Info(logs.ModuleSystem, "配置已更新，同步器已重建", "构建索引", walked, "耗时", time.Since(syncStart).String())

	// strm_url 变更 → 批量把本地 strm 链接规范化到新前缀。重写保留文件 mtime
	// （WriteStrmFile 覆盖恢复），Emby 不会重扫媒体库、本地同步不会误判变更重传。
	// 失败不阻塞配置保存，只记日志。
	if oldStrmUrl != b.cfg.StrmUrl {
		scanned, rewrote, rerr := b.syncer.RewriteStrmLinks(ctx)
		if rerr != nil {
			logs.Error(logs.ModuleSystem, "批量规范化STRM链接失败", "错误", rerr)
		} else {
			logs.Info(logs.ModuleSystem, "批量规范化STRM链接完成", "扫描", scanned, "重写", rewrote)
		}
	}

	b.setInitErr("")
	return nil
}

// verifyCredential 校验待保存的 refresh_token；为空表示未修改，直接跳过。
func (b *App) verifyCredential(ctx context.Context, req config.Editable) error {
	if req.RefreshToken == "" {
		return nil
	}
	// Verify 校验新 rt 并刷新持久化，同时返回账户概况供打印
	info, err := drive.NewClient(b.cfg).Verify(ctx, req.RefreshToken)
	if err != nil {
		return fmt.Errorf("凭证验证失败: %w", err)
	}
	logs.Info(logs.ModuleSystem, "凭证更新成功", "账户", info.String())
	return nil
}

// clearMovedPath 目录配置发生变更时清理旧路径的 DB 记录（旧值为空视为首次配置，不清理）。
func (b *App) clearMovedPath(label, oldPath, newPath string) {
	if oldPath == "" || oldPath == newPath {
		return
	}
	logs.Info(logs.ModuleSync, label+"变更，清理旧数据库记录", "旧值", oldPath)
	b.db.BatchClearPaths([]string{oldPath})
}

// ──── 状态快照 ────

// Snapshot 聚合所有模块状态返回快照。
// ⚠️ 返回 status.StatusData（唯一快照类型，无重复 StatusView）；
// JSON 字段名（config_ready/missing/init_error/sync/strm）为前端依赖，勿动。
func (b *App) Snapshot() *status.StatusData {
	st := b.cfg.Status()
	snap := &status.StatusData{
		ConfigReady: st.Ready,
		Missing:     st.Missing,
		InitError:   b.getInitErr(),
	}
	if cloud, strm, local, ok := b.syncer.CurrentStatus(); ok {
		// CurrentStatus 返回的 TaskStatus 每次新建（Task.Status() 内部分配），可直接赋引用
		snap.Sync = cloud
		snap.Strm = strm
		snap.Local = local
	}
	return snap
}

// failInit 记录初始化错误、推送前端状态并返回同文案 error，收敛各失败分支的三步样板。
func (b *App) failInit(msg string) error {
	b.setInitErr(msg)
	b.publishStatus()
	return fmt.Errorf("%s", msg)
}

func (b *App) setInitErr(msg string) {
	b.initErr.Store(&msg)
}

func (b *App) getInitErr() string {
	if p := b.initErr.Load(); p != nil {
		return *p
	}
	return ""
}

// publishStatus 组装快照并通过 LogStatus 推送前端。
func (b *App) publishStatus() {
	logs.LogStatus(b.Snapshot())
}

// ──── 任务控制 ────

// StartTask 启动手动任务（name="sync" 云端同步 / "strm" STRM生成）。
func (b *App) StartTask(name string) error {
	return b.syncer.StartTask(name)
}

// StopTask 停止手动任务。
func (b *App) StopTask(name string) {
	b.syncer.StopTask(name)
}

// ──── 配置代理 ────

// ConfigSnapshot 返回可编辑配置快照（供前端读写）。
func (b *App) ConfigSnapshot() config.Editable {
	return b.cfg.Snapshot()
}

// GetAuth 返回配置的登录凭据（username, passwordHash）。
func (b *App) GetAuth() (string, string) {
	return b.cfg.GetAuth()
}

// AuthRequired 是否需要登录（未配置密码时 false，直接跳过认证）。
func (b *App) AuthRequired() bool {
	user, _ := b.cfg.GetAuth()
	return user != ""
}
