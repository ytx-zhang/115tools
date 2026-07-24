package syncFile

import (
	"115tools/db"
	"115tools/syncFile/core"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// 本文件是启动时的「初始化编排」：在三个功能模块启动前，
// 必须先把数据库索引和回收目录 FID 准备好（由 New() 顺序调用）。

// initRoot 准备主同步目录的数据库索引。
//
// 两种情形：
//   - 数据库已有主目录记录（非首次运行）→ 直接取出 FID，秒级返回；
//   - 首次运行 → 全量扫描云端目录树，把每个文件/目录的 路径→FID 写入数据库
//     （后续 local 的扫描对比、cloud 的计数跳过优化都依赖这份索引）。
//
// 首次扫描若被中止（如热重载/退出），会清掉写了一半的索引，
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

	// 非首次运行：数据库里已有主目录记录，直接用，无需扫描。
	dbFid := s.env.DB.GetFid(s.env.Paths.SyncPath)
	if dbFid != "" {
		s.env.Paths.SyncFid = dbFid
		return nil
	}

	var isCloudScan atomic.Bool
	isCloudScan.Store(true)
	defer isCloudScan.Store(false)
	slog.Info("初次运行，开始初始化云端数据库...")
	info, err := s.env.API.GetDirInfo(ctx, s.env.Paths.SyncPath)
	if err != nil {
		return err
	}
	s.env.Paths.SyncFid = info.Fid
	s.env.DB.SaveRecord(s.env.Paths.SyncPath, s.env.Paths.SyncFid, db.SizeDir)

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
		if isCloudScan.Load() {
			slog.Error("云端扫描被中止，正在清理数据库", "错误信息", err)
			s.env.DB.BatchClearPaths([]string{s.env.Paths.SyncPath})
		}
		return err
	}

	slog.Info("[初始化] 云端数据库初始化完成")
	return nil
}

// initTemp 准备云端回收目录的 FID（删除/替换文件时先移入这里，保留反悔余地）。
// 同样优先用数据库缓存，没有才查询云端。
func (s *SyncFile) initTemp(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		slog.Warn("[任务中止] Temp目录初始化", "错误信息", err)
		return err
	}
	dbFid := s.env.DB.GetFid(s.env.Paths.TempPath)
	if dbFid != "" {
		s.env.Paths.TempFid = dbFid
		return nil
	}
	info, err := s.env.API.GetDirInfo(ctx, s.env.Paths.TempPath)
	if err != nil {
		return err
	}
	s.env.Paths.TempFid = info.Fid
	s.env.DB.SaveRecord(s.env.Paths.TempPath, s.env.Paths.TempFid, db.SizeDir)
	return nil
}
