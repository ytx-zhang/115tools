package cloudsync

import (
	"context"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// StrmIO 云端→本地落地小模块：视频写 .strm 索引，普通文件真实下载。
// 依赖：api（下载直链）、paths（strm URL）。
// 被调用方：cloudsync/strmgen 任务（FetchAndSave，经 common.WriteStrmFile / DownloadCloudFile 落地）。
type StrmIO struct {
	api   *drive.Client
	paths *common.Paths
}

// NewStrmIO 构造 strmIO 小模块（依赖注入）。
func NewStrmIO(api *drive.Client, paths *common.Paths) *StrmIO {
	return &StrmIO{api: api, paths: paths}
}

// FetchAndSave 按文件类型把云端文件落地：视频写 .strm 索引文件，普通文件真实下载。
// module 由调用方传入其模块类别（cloudsync→sync，strmgen→strm）。
func (s *StrmIO) FetchAndSave(ctx context.Context, module logs.Module, pickCode, savePath string, isVideo bool) error {
	if isVideo {
		t0 := time.Now()
		if err := common.WriteStrmFile(s.paths.StrmUrl, pickCode, savePath); err != nil {
			logs.Error(module, "创建strm文件失败", "路径", savePath, "错误", err)
			return err
		}
		logs.Info(module, "新增STRM文件", "路径", savePath, "耗时", time.Since(t0))
		return nil
	}
	t0 := time.Now()
	if err := common.DownloadCloudFile(ctx, s.api, pickCode, savePath); err != nil {
		logs.Error(module, "下载文件失败", "路径", savePath, "错误", err)
		return err
	}
	logs.Info(module, "下载文件成功", "路径", savePath, "耗时", time.Since(t0))
	return nil
}
