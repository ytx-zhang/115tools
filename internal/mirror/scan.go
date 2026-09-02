package mirror

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/store"
	"golang.org/x/sync/errgroup"
)

// ScanCloud 拉取云端目录树，返回一份按路径升序的扁平快照。
//
// 列表请求是 I/O 密集，子目录用 errgroup 并发拉取（并发度由 drive 包级限流器自然节流）；
// 全部条目收集完再统一排序返回，使下游 PlanCloud 拿到确定的输入顺序。
//
// localCount 为本地目录树的「子树条目数」统计（由 LocalTreeCount 产出，nil 表示不跳过）：
// 非 nil 时，若某云端目录的递归条目数（GetDirInfo count）与对应本地目录一致，视为已同步，
// 跳过该目录的列表拉取与递归——大库二次同步的提速手段，纯本地 I/O 判定，不读索引。
// 代价：目录内「等量替换」的变更会被跳过，属已知取舍。
//
// root 为已预先获取的根目录信息（EnsureCloudDir 或预演的 GetDirInfo 结果）；非 nil 时，
// 根目录那次「数量一致跳过」判定直接复用它，不再对根重复发 GetDirInfo——与 EnsureCloudDir 查重的去重。
func ScanCloud(ctx context.Context, api *drive.Client, paths Paths, localCount map[string]int64, root *drive.DirInfo) (CloudTree, error) {
	tree := CloudTree{RootPath: CleanCloudPath(paths.CloudDir), RootFid: paths.CloudFid}

	var (
		mu    = &sync.Mutex{}
		items []CloudItem
	)
	var walk func(ctx context.Context, path, fid string) error
	walk = func(ctx context.Context, path, fid string) error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if localCount != nil {
			localPath := MapCloudToLocal(paths.LocalDir, paths.CloudDir, path)
			// 根目录：复用调用方已查到的 root，避免重复 GetDirInfo；其余目录照常查询
			var info *drive.DirInfo
			if fid == tree.RootFid && root != nil {
				info = root
			} else {
				info, _ = api.GetDirInfo(ctx, path)
			}
			if info != nil && info.FileCount+info.FolderCount == localCount[localPath] {
				return nil // 本地云端数量一致 → 已同步，跳过该目录的遍历
			}
		}

		list, err := api.GetFileList(ctx, fid)
		if err != nil {
			return err
		}

		g, gctx := errgroup.WithContext(ctx)
		for _, it := range list {
			full := filepath.Join(path, it.Name)
			item := CloudItem{
				Path:     full,
				Fid:      it.Fid,
				PickCode: it.PickCode,
				Size:     it.Size,
				IsDir:    it.IsDir,
				IsVideo:  it.IsVideo,
			}
			mu.Lock()
			items = append(items, item)
			mu.Unlock()

			if it.IsDir {
				g.Go(func() error { return walk(gctx, full, it.Fid) })
			}
		}
		return g.Wait()
	}

	if err := walk(ctx, tree.RootPath, tree.RootFid); err != nil {
		return CloudTree{}, err
	}
	slices.SortFunc(items, func(a, b CloudItem) int { return strings.Compare(a.Path, b.Path) })
	tree.Items = items
	return tree, nil
}

// LocalTreeCount 递归统计本地目录树，返回「每个本地目录 → 子树条目数（文件+目录，不含自身）」。
//
// 供 download 同步的「本地云端数量一致则跳过遍历」判定使用：纯本地 I/O，不读索引，
// 纯下载任务（不建库）同样生效。排除透传缓存目录与上传排除名单（与 readLocalDir 同口径），
// 否则缓存等差异会让数量比较永远不成立、跳过优化失效。
// 本地根不存在时返回空 map（全新 pull → 本地 0 vs 云端 count>0 → 不跳过 → 全量下载）。
//
// 已知取舍：云端 count 是全量递归计数（不排除任何条目），若某目录存在「本地有、但命中
// 排除名单」的条目（这些通常也会被镜像到云端），本地计数会少算而比较恒不等、该目录跳过
// 优化失效——属可接受的有效性损失，跳过失败时安全回退为全量遍历。
func LocalTreeCount(ctx context.Context, localRoot string, rules Rules, excludeDir string) (map[string]int64, error) {
	counts := make(map[string]int64)
	var walk func(dir string) (int64, error)
	walk = func(dir string) (int64, error) {
		if err := context.Cause(ctx); err != nil {
			return 0, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return 0, nil
			}
			return 0, err
		}
		var n int64
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			if excludeDir != "" && full == excludeDir {
				continue
			}
			if rules.Excluded(e.Name()) {
				continue
			}
			n++
			if e.IsDir() {
				sub, err := walk(full)
				if err != nil {
					return 0, err
				}
				n += sub
			}
		}
		counts[dir] = n
		return n, nil
	}
	if _, err := walk(localRoot); err != nil {
		return nil, err
	}
	return counts, nil
}

// BuildIndex 首次启动（或索引被清空后）遍历云端树，把「本地路径 ↔ 云端」映射写入索引。
//
// 只记元数据不落地文件。视频的索引键按任务开关决定：开启 STRM 生成记 .strm 键，
// 关闭则按实体文件记 —— 与 PlanLocal 的键换算规则保持一致，否则会出现「云端已索引、
// 本地却被当新增重传」的错位。
func BuildIndex(ctx context.Context, api *drive.Client, st *store.Store, paths Paths, rules Rules, toStrm bool) error {
	tree, err := ScanCloud(ctx, api, paths, nil, nil) // 初始化必须全量遍历
	if err != nil {
		return err
	}
	for _, it := range tree.Items {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		local := MapCloudToLocal(paths.LocalDir, paths.CloudDir, it.Path)
		switch {
		case it.IsDir:
			st.Put(ctx, local, store.Record{Fid: it.Fid, Kind: store.KindDir})
		case it.IsVideo || rules.IsVideoExt(it.Path):
			path := local
			if toStrm {
				path = VideoToStrmPath(local)
			}
			st.Put(ctx, path, store.Record{Fid: it.Fid, PickCode: it.PickCode, Kind: store.KindStrm, Size: it.Size})
		default:
			st.Put(ctx, local, store.Record{Fid: it.Fid, PickCode: it.PickCode, Kind: store.KindFile, Size: it.Size})
		}
	}
	return nil
}
