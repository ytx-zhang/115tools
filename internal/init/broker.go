// Package broker 作为前后端中间件，统一管理初始化流程和模块交互。
// Broker 聚合配置、API、数据库、日志中心、同步器，对外暴露 Initialize/ApplyConfig/Snapshot 等编排方法。
package broker

import (
	"context"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	synclib "github.com/ytx-zhang/115tools/internal/sync"
	"strings"
	"sync"
	"time"
)

// Broker 聚合所有模块，充当前后端交互的唯一入口。
type Broker struct {
	cfg     *config.Config
	api     *drive.Open115
	db      *db.DB
	hub     *logs.Hub
	syncer  *synclib.Syncer
	appCtx  context.Context
	appWg   *sync.WaitGroup
	mu      sync.Mutex
	initErr string
}

// New 构造 Broker 并注入状态回调到同步器。
func New(cfg *config.Config, api *drive.Open115, database *db.DB, hub *logs.Hub, appCtx context.Context, appWg *sync.WaitGroup) *Broker {
	b := &Broker{
		cfg:    cfg,
		api:    api,
		db:     database,
		hub:    hub,
		appCtx: appCtx,
		appWg:  appWg,
	}
	b.syncer = synclib.NewSyncer(appCtx, cfg, api, database, appWg, hub)
	b.syncer.SetStatusCallback(b.publishStatus)
	return b
}

// ──── 生命周期 ────

// Initialize 启动时完整初始化流程：配置校验 → 凭证验证 → 同步器构建。
func (b *Broker) Initialize() error {
	status := b.cfg.Status()
	if !status.Ready {
		msg := "配置不完整：" + strings.Join(status.Missing, "、")
		b.setInitErr(msg)
		b.publishStatus()
		logs.Error(logs.ModuleSystem, "初始化失败", "原因", msg)
		return fmt.Errorf("配置不完整: %v", status.Missing)
	}

	start := time.Now()

	logs.Info(logs.ModuleSystem, "验证登录凭证...")
	if err := b.api.VerifyToken(b.appCtx); err != nil {
		msg := "登录凭证验证失败: " + err.Error()
		b.setInitErr(msg)
		b.publishStatus()
		logs.Error(logs.ModuleSystem, "初始化失败", "原因", msg)
		return fmt.Errorf("%s", msg)
	}
	logs.Info(logs.ModuleSystem, "登录凭证验证通过", "耗时", time.Since(start).String())

	syncStart := time.Now()
	walked, err := b.syncer.Initialize()
	if err != nil {
		msg := err.Error()
		b.setInitErr(msg)
		b.publishStatus()
		return err
	}
	logs.Info(logs.ModuleSystem, "同步器初始化完成", "构建索引", walked, "耗时", time.Since(syncStart).String())

	logs.Info(logs.ModuleSystem, "初始化完成", "总耗时", time.Since(start).String())
	b.setInitErr("")
	return nil
}

// ApplyConfig 保存配置并重建同步器：路径变更时 Broker 清理旧 DB 记录，sync 专注于业务逻辑。
func (b *Broker) ApplyConfig(ctx context.Context, req config.Editable) error {
	syncStart := time.Now()

	oldSyncPath := b.cfg.SyncPath
	oldTempPath := b.cfg.TempPath
	oldStrmPath := b.cfg.StrmPath

	// 空 refresh_token 表示前端未修改（保持现有 token），仅在提供新值时验证
	if req.RefreshToken != "" {
		if err := b.api.VerifyAndApplyRefreshToken(ctx, req.RefreshToken); err != nil {
			return fmt.Errorf("凭证验证失败: %w", err)
		}
	}
	if err := b.cfg.Update(req); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	// 路径变更 → Broker 清理旧 DB 记录
	if oldSyncPath != "" && oldSyncPath != b.cfg.SyncPath {
		logs.Info(logs.ModuleSync, "同步根目录变更，清理旧数据库记录", "旧值", oldSyncPath)
		b.db.BatchClearPaths([]string{oldSyncPath})
	}
	if oldTempPath != "" && oldTempPath != b.cfg.TempPath {
		logs.Info(logs.ModuleSync, "回收目录变更，清理旧数据库记录", "旧值", oldTempPath)
		b.db.BatchClearPaths([]string{oldTempPath})
	}
	if oldStrmPath != "" && oldStrmPath != b.cfg.StrmPath {
		logs.Info(logs.ModuleSync, "STRM目录变更，清理旧数据库记录", "旧值", oldStrmPath)
		b.db.BatchClearPaths([]string{oldStrmPath})
	}

	walked, err := b.syncer.Initialize()
	if err != nil {
		msg := err.Error()
		b.setInitErr(msg)
		b.publishStatus()
		return err
	}
	logs.Info(logs.ModuleSystem, "配置已更新，同步器已重建", "构建索引", walked, "耗时", time.Since(syncStart).String())

	b.setInitErr("")
	return nil
}

// ──── 状态快照 ────

// StatusView 推到前端的完整状态快照。
type StatusView struct {
	Ready       bool             `json:"ready"`
	ConfigReady bool             `json:"config_ready"`
	Missing     []string         `json:"missing,omitempty"`
	InitError   string           `json:"init_error,omitempty"`
	Sync        *logs.TaskStatus `json:"sync"`
	Strm        *logs.TaskStatus `json:"strm"`
}

// Snapshot 聚合所有模块状态返回快照。
func (b *Broker) Snapshot() *StatusView {
	status := b.cfg.Status()
	view := &StatusView{
		ConfigReady: status.Ready,
		Missing:     status.Missing,
		InitError:   b.getInitErr(),
	}
	if cloud, strm, ok := b.syncer.CurrentStatus(); ok {
		view.Ready = true
		view.Sync = &logs.TaskStatus{Running: cloud.Running, Completed: cloud.Completed, Total: cloud.Total}
		view.Strm = &logs.TaskStatus{Running: strm.Running, Completed: strm.Completed, Total: strm.Total}
	}
	return view
}

func (b *Broker) setInitErr(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.initErr = msg
}

func (b *Broker) getInitErr() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.initErr
}

// publishStatus 组装快照并通过 LogStatus 推送前端。
func (b *Broker) publishStatus() {
	snap := b.Snapshot()
	logs.LogStatus(&logs.StatusData{
		Ready:       snap.Ready,
		ConfigReady: snap.ConfigReady,
		Missing:     snap.Missing,
		InitError:   snap.InitError,
		Sync:        snap.Sync,
		Strm:        snap.Strm,
	})
}

// ──── 任务控制 ────

// StartTask 启动手动任务（name="sync" 云端同步 / "strm" STRM生成）。
func (b *Broker) StartTask(name string) error {
	return b.syncer.StartTask(name)
}

// StopTask 停止手动任务。
func (b *Broker) StopTask(name string) {
	b.syncer.StopTask(name)
}

// ──── 配置代理 ────

// ConfigSnapshot 返回可编辑配置快照（供前端读写）。
func (b *Broker) ConfigSnapshot() config.Editable {
	return b.cfg.Snapshot()
}

// GetAuth 返回配置的登录凭据（username, passwordHash）。
func (b *Broker) GetAuth() (string, string) {
	return b.cfg.GetAuth()
}

// AuthRequired 是否需要登录（未配置密码时 false，直接跳过认证）。
func (b *Broker) AuthRequired() bool {
	user, _ := b.cfg.GetAuth()
	return user != ""
}

// ──── 离线下载透传 ────

// OfflineTaskList 获取离线下载任务列表。
func (b *Broker) OfflineTaskList(ctx context.Context, page int) (*drive.OfflineTaskPage, error) {
	return b.api.OfflineTaskList(ctx, page)
}

// OfflineQuotaInfo 获取离线下载配额。
func (b *Broker) OfflineQuotaInfo(ctx context.Context) (*drive.OfflineQuota, error) {
	return b.api.OfflineQuotaInfo(ctx)
}

// AddOfflineTasks 添加离线下载链接。
func (b *Broker) AddOfflineTasks(ctx context.Context, urls []string, dirID string) ([]drive.OfflineAddResult, error) {
	return b.api.AddOfflineTasks(ctx, urls, dirID)
}

// AddTorrentTask 上传种子并添加 BT 任务。
func (b *Broker) AddTorrentTask(ctx context.Context, data []byte, name, cid, savePath string) (*drive.OfflineAddResult, error) {
	return b.api.AddTorrentTask(ctx, data, name, cid, savePath)
}

// DeleteOfflineTask 删除离线任务。
func (b *Broker) DeleteOfflineTask(ctx context.Context, infoHash string, deleteFiles bool) error {
	return b.api.DeleteOfflineTask(ctx, infoHash, deleteFiles)
}

// ClearOfflineTasks 批量清除离线任务。
func (b *Broker) ClearOfflineTasks(ctx context.Context, flag int) error {
	return b.api.ClearOfflineTasks(ctx, flag)
}

// ResolveCloudDir 把云端路径解析为目录 ID。空→strm_path，"/"→根目录("0")。
func (b *Broker) ResolveCloudDir(ctx context.Context, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = b.cfg.Snapshot().StrmPath
	}
	if path == "" || path == "/" {
		return "0", nil
	}
	info, err := b.api.GetDirInfo(ctx, path)
	if err != nil {
		return "", err
	}
	return info.Fid, nil
}

// ──── Hub 代理 ────

// Subscribe 返回日志订阅通道。
func (b *Broker) Subscribe() chan logs.Entry {
	return b.hub.Subscribe()
}

// Unsubscribe 取消日志订阅。
func (b *Broker) Unsubscribe(ch chan logs.Entry) {
	b.hub.Unsubscribe(ch)
}

// Recent 返回最近日志条目。
func (b *Broker) Recent(limit int) []logs.Entry {
	return b.hub.Recent(limit)
}

// Clear 清空日志缓冲。
func (b *Broker) Clear() {
	b.hub.Clear()
}
