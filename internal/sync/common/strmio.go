package common

import (
	"context"
	"os"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// StrmIO 云端→本地落地小模块：视频写 .strm 索引，普通文件真实下载。
// 依赖：api（下载直链）、paths（strm URL）。
// 被调用方：cloudsync / strmgen 任务（FetchAndSave，经本包 WriteStrmFile / DownloadCloudFile 落地）。
// 归 common 而非 cloudsync，因其被 cloudsync 与 strmgen 共同复用，且只编排本包已有的落地原语。
type StrmIO struct {
	api   *drive.Client
	paths *Paths
}

// NewStrmIO 构造 strmIO 小模块（依赖注入）。复用 Core 载体（api/paths 为其子集），
// 与 NewScanner / NewUploader / NewCloudOps / NewWalker 签名保持一致。
func NewStrmIO(deps *Core) *StrmIO {
	return &StrmIO{api: deps.API, paths: deps.Paths}
}

// FetchAndSave 按文件类型把云端文件落地：视频写 .strm 索引文件，普通文件真实下载。
// module 由调用方传入其模块类别（cloudsync→sync，strmgen→strm）。
// 返回落盘后的「版本号」：视频为本地 .strm 实际 mtime（Unix 秒，本地同步据此判变更），
// 普通文件为真实字节数（scanner 据 size 比对）。由落地处回读而非调用方传入时间戳，
// 避免「遍历时刻」与「下载落盘时刻」不一致导致后续本地同步误判变更→重传/挪走云视频。
func (s *StrmIO) FetchAndSave(ctx context.Context, module logs.Module, pickCode, savePath string, isVideo bool) (int64, error) {
	if isVideo {
		t0 := time.Now()
		if err := WriteStrmFile(s.paths.StrmUrl, pickCode, savePath); err != nil {
			logs.Error(module, "创建strm文件失败", "路径", savePath, "错误", err)
			return 0, err
		}
		logs.Info(module, "新增STRM文件", "路径", savePath, "耗时", time.Since(t0))
		st, serr := os.Stat(savePath)
		if serr != nil {
			return 0, serr
		}
		return st.ModTime().Unix(), nil
	}
	t0 := time.Now()
	if err := DownloadCloudFile(ctx, s.api, pickCode, savePath); err != nil {
		logs.Error(module, "下载文件失败", "路径", savePath, "错误", err)
		return 0, err
	}
	logs.Info(module, "下载文件成功", "路径", savePath, "耗时", time.Since(t0))
	st, serr := os.Stat(savePath)
	if serr != nil {
		return 0, serr
	}
	return st.Size(), nil
}
