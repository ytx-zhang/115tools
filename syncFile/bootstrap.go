package syncFile

import (
	"115tools/db"
	"115tools/syncFile/core"
	"115tools/syncFile/local"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// 本文件是启动时的「初始化编排」：在三个功能模块启动前，
// 必须先把数据库索引和回收目录 FID 准备好（由 New() 顺序调用）。

// initRoot 准备主同步目录的数据库索引。
//
// 设计要点：以云端为准。每次启动都先向 115 查询主目录的当前 FID，
// 再与数据库中记录的 FID 比对，决定「复用索引」还是「清空重扫」：
//   - 一致（非首次运行）→ 直接复用既有索引，秒级返回；
//   - 不一致（云端目录被手动重建、配置改了 sync_path 等）→ 清空旧索引后全量重扫；
//   - 数据库为空（首次运行）→ 全量扫描建库。
//
// 这样配置改动、云端目录被重建等情况不会再沿用陈旧（可能已失效）的索引。
//
// 首次/重扫若被中止（如热重载/退出），会清掉写了一半的索引，
// 保证下次启动重新完整扫描，不留下「看似建好实则残缺」的数据。
func (s *SyncFile) initRoot(parentCtx context.Context) error {
	if err := context.Cause(parentCtx); err != nil {
		slog.Warn("[任务中止] 初始化同步", "错误信息", err)
		return err
	}
	ctx, cancel := context.WithCancelCause(parentCtx)
	defer cancel(nil)
	stopWithErr := func(err error) {
		cancel(err)
	}

	// 总是先读云端：以 115 当前目录为准，避免沿用旧的本地记录。
	info, err := s.env.API.GetDirInfo(ctx, s.env.Paths.SyncPath)
	if err != nil {
		return err
	}
	cloudFid := info.Fid
	s.env.Paths.SyncFid = cloudFid

	// 再取数据库里记录的旧 FID，用于比对。
	dbFid := s.env.DB.GetFid(s.env.Paths.SyncPath)

	// 数据库已有主目录记录，且云端 FID 与之一致 → 秒级复用，无需扫描。
	if dbFid != "" && dbFid == cloudFid {
		return nil
	}

	// 走到这里说明：要么首次运行（dbFid 空），要么云端 FID 已变化（需重建索引）。
	// scanStarted 标记已进入云端扫描分支，扫描被中止时据此清理残留索引。
	// 注意：TempPath 的历史孤儿记录清理不在此处，统一交给 initTemp 负责
	//（TempPath 不在 SyncPath 扫描树内，initRoot 无需管它）。
	scanStarted := true

	if dbFid == "" {
		slog.Info("初次运行，开始初始化云端数据库...")
	} else {
		// FID 变化：清空旧索引后重新全量扫描。
		slog.Info("[初始化] 云端目录 FID 已变更，将清空旧索引并重新全量扫描",
			"旧FID", dbFid, "新FID", cloudFid)
		s.env.DB.BatchClearPaths([]string{s.env.Paths.SyncPath})
	}

	// 记录根目录自身，供后续相对路径拼接与扫描定位。
	s.env.DB.SaveRecord(s.env.Paths.SyncPath, cloudFid, db.SizeDir)

	// 批量写入器：扫描产生的大量写入合并为少量数据库事务；遍历成功后才落盘。
	// 注意：仅在扫描成功时 Flush——若扫描失败，下方会 BatchClearPaths 清理数据库，
	// 此时若仍执行 Flush 会把半成品缓冲写回，破坏清理。
	writer := db.NewBatchWriter(s.env.DB, 0)
	var scanErr error
	defer func() {
		if scanErr == nil {
			writer.Flush()
		}
	}()

	// 遍历整棵云端目录树，把每一项写进索引：
	//   - 目录直接记录；
	//   - 视频记录为 .strm 路径：若本地已存在内容指向同一 FID 的 .strm，
	//     大小值用其修改时间（视为「已同步过」），否则记 0（触发后续重新生成）。
	scanErr = s.env.WalkCloud(ctx, s.env.Paths.SyncPath, s.env.Paths.SyncFid, core.Visitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			writer.Put(path, fid, db.SizeDir)
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
			writer.Put(path, fid, saveSize)
			return nil
		},
	}, stopWithErr)
	if scanErr != nil {
		cancel(scanErr)
	}

	if err := context.Cause(ctx); err != nil {
		if scanStarted {
			slog.Error("云端扫描被中止，正在清理数据库", "错误信息", err)
			s.env.DB.BatchClearPaths([]string{s.env.Paths.SyncPath})
		}
		return err
	}

	slog.Info("[初始化] 云端数据库初始化完成")
	return nil
}

// initTemp 准备云端回收目录的 FID（删除/替换文件时先移入这里，保留反悔余地）。
//
// 设计要点：与旧版「优先读数据库缓存」不同，这里每次启动都向 115 实时查询，
// 结果只回填内存（core.Paths.TempFid），不再写入数据库。原因：temp_path 可能
// 被配置改动、云端手动重建，落库反而会用到陈旧（可能已失效）的 FID。
func (s *SyncFile) initTemp(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		slog.Warn("[任务中止] Temp目录初始化", "错误信息", err)
		return err
	}

	// 每次都读云端，结果仅存内存。
	info, err := s.env.API.GetDirInfo(ctx, s.env.Paths.TempPath)
	if err != nil {
		return err
	}
	s.env.Paths.TempFid = info.Fid
	return nil
}

// ensureDirs 在初始化编排之前，把配置里写好的目录选项都建好：
//   - 本地镜像目录（sync_path / strm_path）用 os.MkdirAll 建本地目录；
//   - 云端目录（sync_path / strm_path / temp_path）用 AddCloudFolder 逐层建（见 syncFile/local 包）。
//
// 这样用户只需在面板填好路径、无需手动去 115 建目录，同步即可启动。
//
// 关于哪些目录要建本地：
//   - sync_path / strm_path 是「云端路径即本地镜像路径」的双栖路径——
//     云端遍历出的路径串直接用作本地 .strm 落盘路径、本地的 fswatcher 也监听该路径，
//     故两者都需在本地建目录。
//   - temp_path 是纯云端回收目录（只作为云端 FID 移动目标，无本地落盘含义），
//     故只为它建云端目录、不建本地目录。
//
// 任一目录为空（未配置）则跳过。本地 MkdirAll 天然幂等；云端 AddCloudFolder 依赖
// GetDirInfo 判定「已存在」，GetDirInfo 瞬时失败会误建同名目录，严格意义上非幂等
// （初始化阶段目录一般已存在或仅新建一次，风险极低）。
func (s *SyncFile) ensureDirs(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	// path=目录路径；local=是否同时需在本地建目录（双栖镜像路径为 true）。
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
		// 本地镜像目录：缺了就建（云端路径串直接作为本地路径）。
		if d.local {
			if err := os.MkdirAll(d.path, 0755); err != nil {
				return fmt.Errorf("[初始化] 创建本地目录失败 %s: %w", d.path, err)
			}
		}
		// 云端目录：逐级确保存在（含缺失的祖先目录），FID 由 initRoot/initTemp 经云端核对回填。
		if _, err := local.AddCloudFolder(ctx, s.env, "", d.path); err != nil {
			return fmt.Errorf("[初始化] 创建云端目录失败 %s: %w", d.path, err)
		}
	}
	return nil
}
