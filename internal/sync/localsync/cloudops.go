// Package localsync 实现「本地 → 云端」同步：本地扫描/上传/云端清理/文件监听。
package localsync

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// CloudOps 云端目录/文件操作模块（建目录、清理）。
type CloudOps struct {
	api   *drive.Client
	db    *store.Store
	paths *common.Paths
	mu    sync.Mutex // 串行化 AddCloudFolder（见下方注释）
}

// NewCloudOps 构造 cloudOps 小模块（依赖注入）。
func NewCloudOps(deps *common.Core) *CloudOps {
	return &CloudOps{api: deps.API, db: deps.DB, paths: deps.Paths}
}

// AddCloudFolder 逐级确保云端目录存在并写库，返回末级 FID。
// 每层先查 DB；DB 缺失则直接 CreateFolder（幂等：云端同名已存在时自动回查 GetDirInfo 复用 FID）。
// ⚠️ 不前置 GetDirInfo：全新目录必然查不到，只会多打一次注定失败的 API 并刷 ERROR 日志；
// CreateFolder 的幂等分支（遇「该目录名称已存在」回查复用）已覆盖 DB 与云端不一致的场景。
// ⚠️ 并发去重：本地扫描走目录池串行调用，但 watcher 对每个视频事件并发调 uploadVideo →
// 多个 goroutine 可能同时为同一父目录 miss DB → 重复 CreateFolder → 打满 2/s API 限流。
// 用互斥锁串行化：第一个 goroutine 建目录+写库后，后续调用直接 DB 命中走快路径，不发冗余 API 请求。
func (co *CloudOps) AddCloudFolder(ctx context.Context, path string) (string, error) {
	co.mu.Lock()
	defer co.mu.Unlock()
	return co.addCloudFolder(ctx, path)
}

func (co *CloudOps) addCloudFolder(ctx context.Context, path string) (string, error) {
	parentFid := "0"
	cur := ""
	for seg := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		cur = cur + "/" + seg
		if fid := co.db.GetFid(cur); fid != "" {
			parentFid = fid
			continue
		}
		// DB 缺失：直接创建。CreateFolder 幂等——云端同名已存在时内部回查复用 FID，
		// 不重复创建；全新目录时也不再走注定失败的 GetDirInfo 刷 ERROR。
		fid, err := co.api.CreateFolder(ctx, parentFid, seg, cur)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", cur, err)
		}
		parentFid = fid
		co.db.SaveRecord(cur, fid, store.SizeDir)
	}
	return parentFid, nil
}

// CloudCleanTask 清理单个本地已删/已变路径对应的云端项：
// 按类型分类后执行——.strm→MoveFile 到 TempFid（保留视频）；目录→先搬子 .strm 视频再 DeleteFile；
// 普通文件→DeleteFile。最后 BatchClearPaths 清库。
func (co *CloudOps) CloudCleanTask(ctx context.Context, fPath string) error {
	t0 := time.Now()

	moveFids, deleteFids := co.classifyCleanPath(fPath)

	if len(moveFids) > 0 {
		if err := co.api.MoveFile(ctx, strings.Join(moveFids, ","), co.paths.TempFid, fPath); err != nil {
			return fmt.Errorf("[%s]: 批量移动云端视频失败: %w", fPath, err)
		}
	}

	if len(deleteFids) > 0 {
		if err := co.api.DeleteFile(ctx, strings.Join(deleteFids, ","), fPath); err != nil {
			return fmt.Errorf("[%s]: 批量删除云端项失败: %w", fPath, err)
		}
	}

	logs.Info(logs.ModuleSync, "清理过时文件", "路径", fPath, "耗时", time.Since(t0))
	co.db.BatchClearPaths([]string{fPath})

	return nil
}

// classifyCleanPath 把待清理路径分为「移动（.strm 及其目录下子 .strm）」与「删除（普通文件/目录）」两类 FID。
func (co *CloudOps) classifyCleanPath(fPath string) (moveFids, deleteFids []string) {
	fid, size := co.db.GetInfo(fPath)
	if fid == "" {
		return nil, nil
	}

	if common.IsStrmPath(fPath) {
		return []string{fid}, nil
	}
	if size == store.SizeDir {
		// 目录：先搬走子 .strm 指向的视频，再删目录本身
		return co.db.ListStrmFids(fPath), []string{fid}
	}
	return nil, []string{fid}
}
