package push

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/engine/kit"
	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
	"github.com/ytx-zhang/115tools/internal/vault"
)

// CloudOps 云端目录/文件操作（建目录、清理本地已删/已变路径的云端项）。
// 目录操作涉及「本地路径 ↔ 云端路径」映射：本地路径为索引主键，云端路径用于 115 API。
type CloudOps struct {
	api   *pan.Client
	vault *vault.Index
	paths *kit.TaskPaths
	mu    sync.Mutex // 串行化建目录，防并发重复 CreateFolder
}

// NewCloudOps 构造云端操作模块。
func NewCloudOps(deps *kit.Deps) *CloudOps {
	return &CloudOps{api: deps.Pan, vault: deps.Vault, paths: deps.Paths}
}

// EnsureRoot 逐级确保云端根目录（CloudDir）存在，把本地根 LocalDir 记入索引，返回根 FID。
// 用于 init 阶段：CloudDir 可能含多级（如 /115/影视），逐级 CreateFolder（幂等）建全。
func (co *CloudOps) EnsureRoot(ctx context.Context) (string, error) {
	co.mu.Lock()
	defer co.mu.Unlock()

	parentFid, err := kit.EnsureCloudDir(ctx, co.api, co.paths.CloudDir)
	if err != nil {
		return "", err
	}
	co.vault.Put(ctx, co.paths.LocalDir, parentFid, vault.SizeDir)
	return parentFid, nil
}

// AddCloudFolder 确保本地子目录对应的云端目录存在，写索引，返回末级 FID。
// 以已解析的根 FID（CloudFid）为起点逐级向下建；DB 命中则快路径跳过（不发 API）。
func (co *CloudOps) AddCloudFolder(ctx context.Context, localPath string) (string, error) {
	co.mu.Lock()
	defer co.mu.Unlock()

	// 相对本地根的部分即云端相对路径；复用 MapLocalToCloud 避免重复的相对化逻辑。
	rel := strings.TrimPrefix(
		kit.MapLocalToCloud(co.paths.LocalDir, co.paths.CloudDir, localPath),
		co.paths.CloudDir,
	)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return co.paths.CloudFid, nil
	}

	parentFid := co.paths.CloudFid
	curLocal := co.paths.LocalDir
	curCloud := co.paths.CloudDir
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		curLocal = filepath.Join(curLocal, seg)
		curCloud += "/" + seg
		if fid := co.vault.GetFid(ctx, curLocal); fid != "" {
			parentFid = fid
			continue
		}
		fid, err := co.api.CreateFolder(ctx, parentFid, seg, curCloud)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", curCloud, err)
		}
		parentFid = fid
		co.vault.Put(ctx, curLocal, fid, vault.SizeDir)
	}
	return parentFid, nil
}

// CloudCleanTask 清理本地已删/已变路径对应的云端项：.strm→移回收；目录→先搬子 strm 再删；普通→删。
func (co *CloudOps) CloudCleanTask(ctx context.Context, localPath string) error {
	t0 := time.Now()

	moveFids, deleteFids := co.classifyCleanPath(ctx, localPath)
	if len(moveFids) > 0 {
		if err := co.api.MoveFile(ctx, strings.Join(moveFids, ","), co.paths.TempFid, localPath); err != nil {
			return fmt.Errorf("[%s]: 批量移动云端视频失败: %w", localPath, err)
		}
	}
	if len(deleteFids) > 0 {
		if err := co.api.DeleteFile(ctx, strings.Join(deleteFids, ","), localPath); err != nil {
			return fmt.Errorf("[%s]: 批量删除云端项失败: %w", localPath, err)
		}
	}

	journal.Info(ctx, "清理过时文件", "路径", localPath, "耗时", time.Since(t0))
	co.vault.ClearPaths(ctx, []string{localPath})
	return nil
}

// classifyCleanPath 把待清理路径分为「移动（.strm 及其目录下子 .strm）」与「删除（普通/目录）」两类 FID。
func (co *CloudOps) classifyCleanPath(ctx context.Context, fPath string) (moveFids, deleteFids []string) {
	fid, size := co.vault.Get(ctx, fPath)
	if fid == "" {
		return nil, nil
	}
	if kit.IsStrmPath(fPath) {
		return []string{fid}, nil
	}
	if size == vault.SizeDir {
		return co.vault.ListStrmFids(ctx, fPath), []string{fid}
	}
	return nil, []string{fid}
}
