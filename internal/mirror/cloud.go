package mirror

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/store"
)

// EnsureCloudDir 逐级确保云端绝对路径存在（每级 CreateFolder，同名自动复用），返回末级 FID。
func EnsureCloudDir(ctx context.Context, api *drive.Client, path string) (string, error) {
	parentFid := "0"
	cur := ""
	for seg := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		cur += "/" + seg
		fid, err := api.CreateFolder(ctx, parentFid, seg, cur)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", cur, err)
		}
		parentFid = fid
	}
	return parentFid, nil
}

// EnsureLocalDir 确保本地子目录对应的云端目录存在并写索引，返回其 FID。
//
// 以已解析的云端根 FID 为起点逐级向下建；索引命中则快路径跳过（不发 API）。
// 本地相对根的部分即云端相对路径（两条路径可不同名，但层级一致）。
func EnsureLocalDir(ctx context.Context, api *drive.Client, idx *store.Store, paths Paths, localPath string) (string, error) {
	sep := string(filepath.Separator)
	rel := RelToRoot(paths.LocalDir, localPath, filepath.Separator)
	if rel == "" {
		return paths.CloudFid, nil
	}

	parentFid := paths.CloudFid
	curLocal := paths.LocalDir
	curCloud := paths.CloudDir
	for _, seg := range strings.Split(rel, sep) {
		if seg == "" {
			continue
		}
		curLocal = filepath.Join(curLocal, seg)
		curCloud += "/" + seg
		if fid := idx.Fid(ctx, curLocal); fid != "" {
			parentFid = fid
			continue
		}
		fid, err := api.CreateFolder(ctx, parentFid, seg, curCloud)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", curCloud, err)
		}
		parentFid = fid
		idx.Put(ctx, curLocal, store.Record{Fid: fid, Kind: store.KindDir})
	}
	return parentFid, nil
}

// Retire 把云端视频批量移入回收目录（可找回）。fids 会被拼成逗号分隔一次调用。
func Retire(ctx context.Context, api *drive.Client, paths Paths, fids []string, localPath string) error {
	if len(fids) == 0 {
		return nil
	}
	err := api.MoveFile(ctx, strings.Join(fids, ","), paths.TempFid, localPath)
	return err
}

// Drop 删除云端文件或目录（fids 逗号分隔批量）。
func Drop(ctx context.Context, api *drive.Client, fids []string, localPath string) error {
	if len(fids) == 0 {
		return nil
	}
	return api.DeleteFile(ctx, strings.Join(fids, ","), localPath)
}

// CollectStrmFids 收集待清理路径下的云端视频 FID：
// .strm 自身取它的记录；目录下先递归取出全部子 .strm，再删空目录。
//
// 返回（待移动的视频 FID，待删除的 FID）。
func CollectStrmFids(ctx context.Context, idx *store.Store, path string) (move, del []string) {
	rec, ok := idx.Get(ctx, path)
	if !ok {
		return nil, nil
	}
	switch rec.Kind {
	case store.KindStrm:
		return []string{rec.Fid}, nil
	case store.KindDir:
		return idx.ListStrmFids(ctx, path), []string{rec.Fid}
	default:
		return nil, []string{rec.Fid}
	}
}
