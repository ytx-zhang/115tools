package local

import (
	"115tools/db"
	"115tools/syncFile/core"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// moveChunk 单次 MoveFile 请求的视频 FID 上限，避免逗号串过长。
const moveChunk = 500

// 本文件是本地同步模块专用的两个云端写操作：建目录、批量清理。
// 它们只服务于「本地 → 云端」方向的业务，因此放在本模块内而非 core。

// AddCloudFolder 确保云端目录存在并返回其 FID，是「本地 → 云端」唯一的建目录入口。
// 纯云端操作，不写数据库——FID 准确性由各调用方 / 初始化阶段负责：
//
//   - 扫描 / 监控发现的新目录由调用方 SaveRecord 落库；
//
//   - 初始化阶段 sync_path/strm_path/temp_path 的 FID 由 initRoot/initTemp
//     经 GetDirInfo 实时核对回填（SyncFid 落库、TempFid 仅内存），无需在此写库。
//
//   - currentCID 非空：已知父目录 FID，直接在其下创建 path 的末级目录（扫描热路径用，
//     单步、不逐级 GetDirInfo，保持全量扫描 O(N)）；
//
//   - currentCID 为空("")：仅知道根相对路径，从根 "0" 逐级 GetDirInfo 确认、缺失再建
//     （初始化 / watcher 补建祖先目录用）。注意：是否「已存在」完全依赖该层 GetDirInfo
//     成功判定；若某层 GetDirInfo 因网络/权限瞬时失败会被误判为不存在而 AddFolder 出同名
//     目录，故严格意义上非幂等。初始化阶段目录一般已存在或仅新建一次，风险极低。
func AddCloudFolder(ctx context.Context, env *core.Env, currentCID, path string) (string, error) {
	if currentCID != "" {
		fid, err := env.API.AddFolder(ctx, currentCID, filepath.Base(path))
		if err != nil {
			return "", fmt.Errorf("[%s]: 创建云端文件夹失败: %w", path, err)
		}
		slog.Info("创建云端目录", "路径", path, "云端FID", fid)
		return fid, nil
	}

	// 起点：115 根目录 FID 恒为 "0"，无需经 get_info 反推（根目录 get_info 返回子项数组）。
	parentFid := "0"
	cur := ""
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" {
			continue // 前导或重复的 "/" 会产生空段，跳过
		}
		cur = cur + "/" + seg
		if info, err := env.API.GetDirInfo(ctx, cur); err == nil {
			parentFid = info.Fid // 该层已存在，继续向下
			continue
		}
		fid, err := env.API.AddFolder(ctx, parentFid, seg)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", cur, err)
		}
		slog.Info("创建云端目录", "路径", cur, "云端FID", fid)
		parentFid = fid
	}
	return parentFid, nil
}

// cloudCleanTask 批量清理「本地已删除」路径对应的云端项，分三类处理：
//   - .strm 文件 → 数据库里存的就是云端视频 FID，直接 MoveFile 到 TempFid，永久保留反悔余地；
//   - 目录 → 先递归把目录下所有 .strm 对应的云端视频 MoveFile 到 TempFid，
//     再 DeleteFile 让目录（连同其余普通文件/子目录壳）进 115 回收站；
//   - 普通文件 → 直接 DeleteFile 进 115 回收站。
//
// 先把视频挪走再删目录，保证「有价值视频」落在自己管理的 TempFid（不随回收站过期），
// 而目录外壳/strm 指针/普通文件自然进 115 回收站限期清理，TempFid 不会被目录树污染。
// 最后统一清理数据库记录（BatchClearPaths 连子项一起删）。workPath 仅用于错误定位。
func (l *Local) cloudCleanTask(ctx context.Context, fPaths []string, workPath string) error {
	if len(fPaths) == 0 {
		return nil
	}

	var moveFids []string   // 要挪到 TempFid 的云端视频 FID
	var deleteFids []string // 直接删（进 115 回收站）的 FID：普通文件 + 目录

	// appendMove 去重追加，避免目录递归与单 strm 文件重复同一视频 FID
	appendMove := func(fid string) {
		if fid != "" && !slices.Contains(moveFids, fid) {
			moveFids = append(moveFids, fid)
		}
	}

	for _, fPath := range fPaths {
		fid, size := l.env.DB.GetInfo(fPath)
		if fid == "" {
			// 云端没记录，说明之前没同步成功过，跳过
			continue
		}

		isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
		if isStrm {
			// strm 文件：数据库存的 fid 就是云端视频，直接挪 TempFid
			appendMove(fid)
		} else if size == db.SizeDir {
			// 目录：先把目录下所有 strm 对应的云端视频挪 TempFid，目录本身稍后删
			for _, vf := range l.env.DB.ListStrmFids(fPath) {
				appendMove(vf)
			}
			deleteFids = append(deleteFids, fid)
		} else {
			// 普通文件：直接删（进回收站）
			deleteFids = append(deleteFids, fid)
		}
	}

	joined := strings.Join(fPaths, ",")

	// 1. 批量移动云端视频到临时目录（分批，避免单次请求过长）
	if len(moveFids) > 0 {
		for start := 0; start < len(moveFids); start += moveChunk {
			end := min(start+moveChunk, len(moveFids))
			chunk := moveFids[start:end]
			t0 := time.Now()
			if err := l.env.API.MoveFile(ctx, strings.Join(chunk, ","), l.env.Paths.TempFid); err != nil {
				return fmt.Errorf("[%s]: 批量移动云端视频失败: %w", workPath, err)
			}
			slog.Info("移动云端视频到临时目录", "路径", joined, "数量", len(chunk), "耗时", time.Since(t0))
		}
	}

	// 2. 批量删除（普通文件 + 目录，进 115 回收站）
	if len(deleteFids) > 0 {
		t0 := time.Now()
		if err := l.env.API.DeleteFile(ctx, strings.Join(deleteFids, ",")); err != nil {
			return fmt.Errorf("[%s]: 批量删除云端项失败: %w", workPath, err)
		}
		slog.Info("删除云端项(进回收站)", "路径", joined, "数量", len(deleteFids), "耗时", time.Since(t0))
	}

	// 3. 清理数据库记录（连子项一起清）
	slog.Debug("清理数据库索引", "路径", joined, "数量", len(fPaths))
	l.env.DB.BatchClearPaths(fPaths)

	return nil
}
