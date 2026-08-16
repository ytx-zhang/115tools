// Package localsync 实现「本地 → 云端」同步：本地扫描/上传/云端清理/文件监听。
package localsync

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// CloudOps 云端目录/文件操作小模块（建目录、清理）。
// 依赖：api（云端增删移）、db（FID 索引读写）、paths（回收目录 FID）。
type CloudOps struct {
	api   *drive.Client
	db    *store.Store
	paths *common.Paths
}

// NewCloudOps 构造 cloudOps 小模块（依赖注入）。
func NewCloudOps(deps *common.SyncDeps) *CloudOps {
	return &CloudOps{api: deps.API, db: deps.DB, paths: deps.Paths}
}

// AddCloudFolder 逐级确保云端目录存在并写入数据库。
// 每层先查 DB：已有则复用 FID；DB 缺失但云端已存在（GetDirInfo 命中）则复用云端 FID；
// 仅当 DB 与云端都不存在时才 CreateFolder。返回末级 FID，调用方无需再 SaveRecord。
//
// ⚠️ DB 判定必须与云端判定兜底结合：DB key 与云端路径可能因尾斜杠等格式差异不一致
// （SaveRecord 的 key 与逐级拼出的 cur 不同），纯查 DB 会把「已存在的云端目录」误判为
// 缺失而重复创建（115 报 20004 目录已存在）。GetDirInfo 以规范化路径查询，作为权威判定。
func (co *CloudOps) AddCloudFolder(ctx context.Context, path string) (string, error) {
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
		if info, err := co.api.GetDirInfo(ctx, cur); err == nil {
			parentFid = info.Fid
			co.db.SaveRecord(cur, info.Fid, store.SizeDir)
			continue
		}
		fid, err := co.api.CreateFolder(ctx, parentFid, seg)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", cur, err)
		}
		parentFid = fid
		co.db.SaveRecord(cur, fid, store.SizeDir)
	}
	return parentFid, nil
}

// CloudCleanTask 批量清理本地已删路径对应的云端项：
// 分类交给 classifyCleanPaths：.strm→MoveFile 到 TempFid（保留视频）；目录→先搬子 .strm 视频
// 再 DeleteFile；普通文件→DeleteFile。最后 BatchClearPaths 清库。
func (co *CloudOps) CloudCleanTask(ctx context.Context, fPaths []string, workPath string) error {
	if len(fPaths) == 0 {
		return nil
	}
	t0 := time.Now()

	moveFids, deleteFids := co.classifyCleanPaths(fPaths)

	if len(moveFids) > 0 {
		if err := co.api.MoveFile(ctx, strings.Join(moveFids, ","), co.paths.TempFid); err != nil {
			return fmt.Errorf("[%s]: 批量移动云端视频失败: %w", workPath, err)
		}
	}

	if len(deleteFids) > 0 {
		if err := co.api.DeleteFile(ctx, strings.Join(deleteFids, ",")); err != nil {
			return fmt.Errorf("[%s]: 批量删除云端项失败: %w", workPath, err)
		}
	}

	if len(fPaths) == 1 {
		logs.Info(logs.ModuleSync, "清理过时文件", "路径", fPaths[0], "耗时", time.Since(t0))
	} else {
		logs.Info(logs.ModuleSync, "清理过时文件", "目标目录", workPath, "文件数", len(fPaths), "耗时", time.Since(t0))
	}
	co.db.BatchClearPaths(fPaths)

	return nil
}

// classifyCleanPaths 把待清理路径按类型分为「移动（.strm 及其目录下的子 .strm）」与
// 「删除（普通文件/目录）」两类 FID 列表。纯分类无副作用，云端执行由 CloudCleanTask 负责。
func (co *CloudOps) classifyCleanPaths(fPaths []string) (moveFids, deleteFids []string) {
	appendMove := func(fid string) {
		if fid != "" {
			moveFids = append(moveFids, fid)
		}
	}

	for _, fPath := range fPaths {
		fid, size := co.db.GetInfo(fPath)
		if fid == "" {
			continue
		}

		if common.IsStrmPath(fPath) {
			appendMove(fid)
		} else if size == store.SizeDir {
			for _, vf := range co.db.ListStrmFids(fPath) {
				appendMove(vf)
			}
			deleteFids = append(deleteFids, fid)
		} else {
			deleteFids = append(deleteFids, fid)
		}
	}
	// 去重：同一视频可能在 .strm 目录与其子项中被重复收集（O(n²) → O(n log n)）。
	// 注意：本工具链 slices.Sort 返回 no value（标准库签名），不能链式传入 slices.Compact；
	// 须分两行：先排序，再原地压缩去重。
	slices.Sort(moveFids)
	moveFids = slices.Compact(moveFids)
	return moveFids, deleteFids
}
