// Package syncFile 是文件同步功能的根包（门面）：装配 core/local/cloud/strm
// 四个子模块，对外提供统一调用入口。生命周期由 Runner 管理（热重载）。
//
// 本包文件：syncFile.go（组合器+New+初始化编排+定时任务+门面方法）、runner.go（生命周期）。
// 子模块：local（本地→云端）/cloud（云端→本地）/strm（批量生成.strm）/core（共享零件）。
package syncFile

import (
	"context"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/syncFile/cloud"
	"github.com/ytx-zhang/115tools/internal/syncFile/core"
	"github.com/ytx-zhang/115tools/internal/syncFile/local"
	"github.com/ytx-zhang/115tools/internal/syncFile/strm"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SyncFile 是三个功能模块的组合器，也是 web 层唯一的调用入口（门面）。
type SyncFile struct {
	env   *core.Env
	local *local.Local
	cloud *cloud.Cloud
	strm  *strm.Strm
}

// New 装配并初始化整个同步系统。onChange 是状态变更回调（由 Runner 注入，
// 内部组装完整状态快照并广播，详见 core.TaskStats）；wg 挂接所有后台协程保证优雅退出。
func New(ctx context.Context, cfg *config.Config, api *drive.Open115, boltDB *db.DB, wg *sync.WaitGroup, onChange func(), oldSyncPath string) (*SyncFile, error) {
	env := core.NewEnv(cfg, api, boltDB)
	s := &SyncFile{env: env}

	if err := s.ensureDirs(ctx); err != nil {
		return nil, err
	}
	// initRoot 扫描云端建库索引，local 的扫描对比依赖这些记录
	if err := s.initRoot(ctx, oldSyncPath); err != nil {
		return nil, err
	}
	if err := s.initTemp(ctx); err != nil {
		return nil, err
	}

	s.local = local.New(env)
	s.cloud = cloud.New(env, onChange)
	s.strm = strm.New(env, onChange)

	s.local.Start(ctx, wg)
	wg.Go(func() { s.cronSync(ctx) })
	return s, nil
}

// ──── 初始化编排 ────

// initRoot 建立主同步目录的数据库索引（以云端 FID 为准）。
// FID 一致则复用索引；不一致或首次运行则全量扫描。
// 扫描被中止时清理半成品索引，保证下次完整扫描。
func (s *SyncFile) initRoot(parentCtx context.Context, oldSyncPath string) error {
	if err := context.Cause(parentCtx); err != nil {
		slog.Warn("[任务中止] 初始化同步", "错误信息", err)
		return err
	}
	ctx, cancel := context.WithCancelCause(parentCtx)
	defer cancel(nil)
	stopWithErr := func(err error) { cancel(err) }

	info, err := s.env.API.GetDirInfo(ctx, s.env.Paths.SyncPath)
	if err != nil {
		return err
	}
	cloudFid := info.Fid
	s.env.Paths.SyncFid = cloudFid

	dbFid := s.env.DB.GetFid(s.env.Paths.SyncPath)
	if dbFid != "" && dbFid == cloudFid {
		return nil // 复用索引
	}

	scanStarted := true
	if dbFid == "" {
		slog.Info("初次运行，开始初始化云端数据库...")
	} else {
		slog.Info("[初始化] 云端目录 FID 已变更，将清空旧索引并重新全量扫描",
			"旧FID", dbFid, "新FID", cloudFid)
		s.env.DB.BatchClearPaths([]string{s.env.Paths.SyncPath})
	}
	s.env.DB.SaveRecord(s.env.Paths.SyncPath, cloudFid, db.SizeDir)

	var scanErr error
	defer func() {
		if scanErr != nil {
			s.env.DB.BatchClearPaths([]string{s.env.Paths.SyncPath})
		}
	}()
	// 新索引构建成功后清理旧同步根：失败不删，避免丢数据；
	// 旧目录是新目录子树时只删不重叠分支，保留刚重建的新索引。
	defer func() {
		if scanErr == nil {
			s.env.DB.ClearOldRoot(oldSyncPath, s.env.Paths.SyncPath)
		}
	}()

	// 遍历云端目录树建索引：视频记为 .strm 路径（本地已存在指向同 FID 的 .strm 时用 mtime）
	scanErr = s.env.WalkCloud(ctx, s.env.Paths.SyncPath, s.env.Paths.SyncFid, core.Visitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			s.env.DB.SaveRecord(path, fid, db.SizeDir)
			return true, nil
		},
		VisitFile: func(_ context.Context, path, fid, _ string, e core.Entry) error {
			saveSize := e.Size
			if e.IsVideo {
				path = strings.TrimSuffix(path, filepath.Ext(path)) + ".strm"
				saveSize = 0
				if info, err := os.Stat(path); err == nil {
					if _, localFid := core.ExtractPickcode(path); localFid == fid {
						saveSize = info.ModTime().Unix()
					}
				}
			}
			s.env.DB.SaveRecord(path, fid, saveSize)
			return nil
		},
	}, stopWithErr)
	if scanErr != nil {
		cancel(scanErr)
	}

	if err := context.Cause(ctx); err != nil {
		if scanStarted {
			slog.Error("云端扫描被中止，正在清理数据库", "错误信息", err)
		}
		return err
	}

	slog.Info("[初始化] 云端数据库初始化完成")
	return nil
}

// initTemp 查询云端回收目录 FID，只存内存（不落库，避免 temp_path 变更后用过期 FID）。
func (s *SyncFile) initTemp(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		slog.Warn("[任务中止] Temp目录初始化", "错误信息", err)
		return err
	}
	info, err := s.env.API.GetDirInfo(ctx, s.env.Paths.TempPath)
	if err != nil {
		return err
	}
	s.env.Paths.TempFid = info.Fid
	return nil
}

// ensureDirs 建好配置里的目录：双栖路径(sync/strm)建本地+云端，temp 只建云端。
func (s *SyncFile) ensureDirs(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	dirs := []struct {
		path  string
		local bool
	}{
		{s.env.Paths.SyncPath, true},
		{s.env.Paths.StrmPath, true},
		{s.env.Paths.TempPath, false},
	}
	for _, d := range dirs {
		if strings.TrimSpace(d.path) == "" {
			continue
		}
		if d.local {
			if err := os.MkdirAll(d.path, 0755); err != nil {
				return fmt.Errorf("[初始化] 创建本地目录失败 %s: %w", d.path, err)
			}
		}
		if _, err := local.AddCloudFolder(ctx, s.env, "", d.path); err != nil {
			return fmt.Errorf("[初始化] 创建云端目录失败 %s: %w", d.path, err)
		}
	}
	return nil
}

// ──── 定时任务 ────

// cronSync 定时全量同步：每 CronInterval 触发本地全量扫描+云端同步。
// cron.enabled=false 时挂起空转，仅依赖文件监听。
func (s *SyncFile) cronSync(ctx context.Context) {
	if !s.env.CronEnabled {
		slog.Info("[定时] 定时全量同步已关闭（配置 cron.enabled=false），仅依赖本地文件监听")
		<-ctx.Done()
		return
	}
	interval := s.env.CronInterval
	slog.Info("[定时] 定时全量同步已启用", "间隔", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			slog.Debug("触发定时全量同步任务")
			s.local.FullScan(ctx)
			s.cloud.Start(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// ──── web 层调用的门面方法 ────

func (s *SyncFile) StartCloudSync(ctx context.Context) { s.cloud.Start(ctx) }
func (s *SyncFile) StopCloudSync()                     { s.cloud.Stop() }
func (s *SyncFile) StartAddStrm(ctx context.Context)   { s.strm.Start(ctx) }
func (s *SyncFile) StopAddStrm()                       { s.strm.Stop() }

// LocalFullScan 触发一次本地全量扫描（用于保存上传排除规则后联动清理云端存量）。
func (s *SyncFile) LocalFullScan(ctx context.Context) { s.local.FullScan(ctx) }

// RegenerateStrmFiles 在 StrmUrl 变更后，用新 URL 重写本地所有 .strm 文件内容。
// 遍历 SyncPath 与 StrmPath 两棵树下全部 .strm；解析失败/非本工具生成的跳过，不影响其它文件。
// 纯本地 IO，不请求云端（用 ExtractPickcode 反向解析旧文件取 pickcode/fid）。
func (s *SyncFile) RegenerateStrmFiles(ctx context.Context) {
	roots := []string{s.env.Paths.SyncPath, s.env.Paths.StrmPath}
	var done int
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.EqualFold(filepath.Ext(p), ".strm") {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			pc, fid := core.ExtractPickcode(p)
			if pc == "" || fid == "" {
				slog.Warn("[STRM] 跳过无法解析的 strm 文件", "文件", p)
				return nil
			}
			if err := s.env.SaveStrmFile(pc, fid, p); err != nil {
				slog.Error("[STRM] 重写 strm 失败", "文件", p, "错误", err)
				return nil
			}
			done++
			return nil
		})
	}
	slog.Info("[STRM] StrmUrl 变更后重写本地 strm 完成", "数量", done)
}

// StatusView 是推送给前端的完整状态快照。
type StatusView struct {
	Ready       bool               `json:"ready"`
	ConfigReady bool               `json:"config_ready"`
	Missing     []string           `json:"missing"`
	Sync        *core.TaskProgress `json:"sync"`
	Strm        *core.TaskProgress `json:"strm"`
}

// StatusSnapshot 返回当前进度快照（仅 Sync/Strm，Ready 等由 web 层填充）。
func (s *SyncFile) StatusSnapshot() *StatusView {
	return &StatusView{Sync: s.cloud.Status(), Strm: s.strm.Status()}
}
