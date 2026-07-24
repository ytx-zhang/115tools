package local

import (
	"115tools/syncFile/core"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本文件实现上传执行层：固定大小的 worker 池 + 两种上传任务（普通文件 / .strm）。

// uploadWorkerCount 常驻上传 worker 数量：最多同时有 3 个上传在进行。
// 太小会拖慢大批量入库，太大则容易触发 115 的限流。
const uploadWorkerCount = 3

// uploadJob 描述一次上传任务：把本地文件 fPath 上传到云端目录 cid 下。
// ctx 跟随触发它的任务上下文（热重载/停止时任务随之取消）。
type uploadJob struct {
	ctx   context.Context
	cid   string
	fPath string
}

// startUploadWorkers 启动 n 个常驻上传 worker，从 uploadJobs 队列消费任务。
// ctx 取消时所有 worker 退出（由 Start 调用，挂在模块的 WaitGroup 上）。
func (l *Local) startUploadWorkers(ctx context.Context, n int) {
	for range n {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-l.uploadJobs:
					if !ok {
						return
					}
					l.doUpload(job.ctx, job.cid, job.fPath)
				}
			}
		}()
	}
}

// uploadOneFile 把本地文件加入上传队列，由 worker 池异步执行上传。
// 调用方（队列 worker / scanDir）立即返回、继续处理其余路径，
// 上传在后台以最多 uploadWorkerCount 路并发进行。ctx 取消时直接丢弃任务。
func (l *Local) uploadOneFile(ctx context.Context, cid, fPath string) {
	select {
	case <-ctx.Done():
	case l.uploadJobs <- uploadJob{ctx: ctx, cid: cid, fPath: fPath}:
	}
}

// doUpload 真正执行一次上传（由上传 worker 调用）：
// os.Stat 确认文件还在 → 按类型分派 upStrmTask/upFileTask → 记录结果日志。
func (l *Local) doUpload(ctx context.Context, cid, fPath string) {
	if err := ctx.Err(); err != nil {
		return
	}
	fileInfo, err := os.Stat(fPath)
	if err != nil {
		// 排队期间文件又被删了，属正常情况，无需报错
		slog.Warn("同步的文件不存在", "文件", fPath)
		return
	}

	isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
	if isStrm {
		err = l.upStrmTask(ctx, cid, fPath)
	} else {
		err = l.upFileTask(ctx, cid, fPath, fileInfo)
	}
	if err != nil {
		slog.Error("同步失败", "文件", fPath, "错误", err)
	} else {
		slog.Info("上传文件完成", "文件", fPath)
	}
}

// ──── 两种上传任务 ────

// upFileTask 上传普通文件。若文件是视频（CheckVideo 命中），上传完成后：
// 本地视频文件会被删除，原地替换为一个 .strm 索引文件（播放走云端直链），
// 这就是「本地不存视频、Emby 照常播放」的核心机制。
func (l *Local) upFileTask(ctx context.Context, cid, fPath string, fileInfo os.FileInfo) error {
	info, err := l.env.API.UploadFile(ctx, fPath, cid, "", "")
	if err != nil {
		return err
	}
	cloudFid := info.Fid
	size := fileInfo.Size()
	savePath := fPath
	ext := filepath.Ext(fPath)
	if core.CheckVideo(ext, size) {
		savePath = strings.TrimSuffix(fPath, ext) + ".strm"
		// 若该视频此前已有旧版本在云端（同名 .strm 有记录），先把旧文件移入回收目录
		if dbFid := l.env.DB.GetFid(savePath); dbFid != "" {
			if err := l.env.API.MoveFile(ctx, dbFid, l.env.Paths.TempFid); err != nil {
				return fmt.Errorf("[%s]: 清理旧视频失败: %w", savePath, err)
			}
		}
		// 写入 .strm 索引（内容是指向本程序 /download 的 URL）
		if err := l.env.SaveStrmFile(info.PickCode, cloudFid, savePath); err != nil {
			return fmt.Errorf("[%s]: 写入strm文件失败: %w", savePath, err)
		}
		// 视频已上云，本地原件删除，只留 .strm
		if err := os.Remove(fPath); err != nil {
			return fmt.Errorf("[%s]: 删除视频文件失败: %w", fPath, err)
		}
		size = time.Now().Unix() // .strm 用时间戳当「版本号」记入数据库
	}
	l.env.DB.SaveRecord(savePath, cloudFid, size)
	return nil
}

// upStrmTask 处理本地新增的 .strm 文件（例如用户从别处拷来的索引）。
// 它不重新上传视频本体，而是利用 115 的「秒传/移动」能力：
// 按 pickcode 找到云端已有视频 → 移动到目标目录 → 改回原名 → 重写本地 .strm。
func (l *Local) upStrmTask(ctx context.Context, cid, fPath string) error {
	pickcode, fid := core.ExtractPickcode(fPath)
	if pickcode == "" {
		return fmt.Errorf("[%s]: 无pickcode", fPath)
	}
	if fid == "" {
		// .strm 里没存 fid 时，用一次下载直链查询换取 fid
		info, err := l.env.API.GetDownloadUrl(ctx, pickcode, "115tools")
		if err != nil {
			return fmt.Errorf("[%s]: 获取fid失败: %w", fPath, err)
		}
		fid = info.Fid
	}

	// 把云端视频移动到目标目录（与本地 .strm 所在位置对应）
	if err := l.env.API.MoveFile(ctx, fid, cid); err != nil {
		return fmt.Errorf("[%s]: 移动云端视频失败: %w", fPath, err)
	}

	// 恢复云端文件名：秒传上来的文件名可能是 pickcode，需改回原始片名
	origName := strings.TrimSuffix(filepath.Base(fPath), ".strm")
	newName, err := l.env.API.UpdateFile(ctx, fid, origName)
	if err != nil {
		return fmt.Errorf("[%s]: 云端改名失败: %w", fPath, err)
	}
	// UpdateFile 可能只改了主名没带上扩展名，补一次完整文件名
	if newName != origName+filepath.Ext(newName) {
		if _, err := l.env.API.UpdateFile(ctx, fid, origName+filepath.Ext(newName)); err != nil {
			return fmt.Errorf("[%s]: 云端扩展名修复失败: %w", fPath, err)
		}
	}

	// 重写本地 .strm（补全 fid 参数），并记录到数据库
	if err := l.env.SaveStrmFile(pickcode, fid, fPath); err != nil {
		return fmt.Errorf("[%s]: 文件写入失败: %w", fPath, err)
	}
	l.env.DB.SaveRecord(fPath, fid, time.Now().Unix())
	return nil
}
