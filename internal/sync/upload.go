package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
)

// 本文件实现上传执行层：固定大小的 worker 池 + 两种上传任务（普通文件 / .strm）。

const uploadWorkerCount = 3

// uploadJob 描述一次上传任务：把本地文件 fPath 上传到云端目录 cid 下。
type uploadJob struct {
	cid   string
	fPath string
}

// startUploadWorkers 启动 n 个常驻上传 worker，从 uploadJobs 队列消费任务。
// ctx 取消时所有 worker 退出（挂在实例的 WaitGroup 上）。
func (l *instance) startUploadWorkers(ctx context.Context, n int) {
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
					l.doUpload(ctx, job.cid, job.fPath)
				}
			}
		}()
	}
}

// uploadOneFile 把本地文件加入上传队列，由 worker 池异步执行。ctx 取消时直接丢弃任务。
func (l *instance) uploadOneFile(ctx context.Context, cid, fPath string) {
	select {
	case <-ctx.Done():
	case l.uploadJobs <- uploadJob{cid: cid, fPath: fPath}:
	}
}

// alreadyUploaded 查数据库判断是否已上传过（防重复上传产生云端副本）。
// .strm 存在即完成；普通文件比字节数；视频与同名 .strm 共存视为「替换旧视频」，放行重传。
func (l *instance) alreadyUploaded(fPath string, fileInfo os.FileInfo) bool {
	isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
	dbKey := fPath
	if !isStrm && CheckVideo(filepath.Ext(fPath), fileInfo.Size()) {
		dbKey = strings.TrimSuffix(fPath, filepath.Ext(fPath)) + ".strm"
	}
	dbFid, dbSize := l.env.DB.GetInfo(dbKey)
	if dbFid == "" {
		return false
	}
	if isStrm {
		return true
	}
	if CheckVideo(filepath.Ext(fPath), fileInfo.Size()) {
		return false
	}
	return fileInfo.Size() == dbSize
}

// doUpload 真正执行一次上传（由 worker 调用）：Stat 确认文件还在 → 查重 →
// 阈值片段拦截 → inFlight 并发去重 → upStrm/upFile 分派 → 记录日志。
func (l *instance) doUpload(ctx context.Context, cid, fPath string) {
	if err := ctx.Err(); err != nil {
		return
	}
	fileInfo, err := os.Stat(fPath)
	if err != nil {
		slog.Warn("同步的文件不存在", "文件", fPath)
		return
	}

	if l.alreadyUploaded(fPath, fileInfo) {
		slog.Debug("文件已存在云端对应记录，跳过重复上传", "文件", fPath)
		return
	}

	// 同名 .strm 已存在、且本文件是「视频扩展名但体积未达阈值」的片段：不处理、不上传，仅告警。
	// 只拦截视频扩展名；.nfo/.jpg/.srt 等伴随文件照常上传。
	isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
	isVideoExt := IsVideoExt(filepath.Ext(fPath))
	isVideo := CheckVideo(filepath.Ext(fPath), fileInfo.Size())
	if !isStrm && isVideoExt && !isVideo {
		strmKey := strings.TrimSuffix(fPath, filepath.Ext(fPath)) + ".strm"
		if l.env.DB.GetFid(strmKey) != "" {
			slog.Warn("同名 strm 已存在但该视频文件未达体积阈值，跳过上传",
				"文件", fPath, "strm", strmKey)
			return
		}
	}

	// 并发去重：替换场景下同名视频不再因 .strm 已存在而跳过，靠 inFlight 防重复上传。
	if _, loaded := l.inFlight.LoadOrStore(fPath, struct{}{}); loaded {
		slog.Debug("文件正在上传，跳过重复任务", "文件", fPath)
		return
	}
	defer l.inFlight.Delete(fPath)

	upStart := time.Now()
	if isStrm {
		err = l.upStrmTask(ctx, cid, fPath)
	} else {
		err = l.upFileTask(ctx, cid, fPath, fileInfo)
	}
	if err != nil {
		// 上传前文件被外部修改导致大小变化的错误属可自愈（后续扫描会重传），降级为 Warn 减少日志噪音。
		if errors.Is(err, drive.ErrUploadSizeChanged) {
			slog.Warn("同步跳过（文件上传前被修改，待下次扫描重传）", "文件", fPath, "错误", err, "耗时", time.Since(upStart))
		} else {
			slog.Error("同步失败", "文件", fPath, "错误", err, "耗时", time.Since(upStart))
		}
	} else {
		slog.Info("上传文件完成", "文件", fPath, "耗时", time.Since(upStart))
	}
}

// ──── 两种上传任务 ────

// upFileTask 上传普通文件。视频（CheckVideo 命中）上传完成后本地原件被删除，
// 原地替换为 .strm 索引文件（播放走云端直链）——「本地不存视频、Emby 照常播放」的核心机制。
func (l *instance) upFileTask(ctx context.Context, cid, fPath string, fileInfo os.FileInfo) error {
	info, err := l.env.API.UploadFile(ctx, fPath, cid, "", "")
	if err != nil {
		return err
	}
	cloudFid := info.Fid
	size := fileInfo.Size()
	savePath := fPath
	ext := filepath.Ext(fPath)
	if CheckVideo(ext, size) {
		savePath = strings.TrimSuffix(fPath, ext) + ".strm"
		// 该视频此前已有旧版本在云端（同名 .strm 有记录）→ 先把旧文件移入回收目录
		if dbFid := l.env.DB.GetFid(savePath); dbFid != "" {
			if err := l.env.API.MoveFile(ctx, dbFid, l.env.Paths.TempFid); err != nil {
				return fmt.Errorf("[%s]: 清理旧视频失败: %w", savePath, err)
			}
		}
		if err := l.env.SaveStrmFile(info.PickCode, cloudFid, savePath); err != nil {
			return fmt.Errorf("[%s]: 写入strm文件失败: %w", savePath, err)
		}
		if err := os.Remove(fPath); err != nil {
			return fmt.Errorf("[%s]: 删除视频文件失败: %w", fPath, err)
		}
		size = time.Now().Unix() // .strm 用时间戳当「版本号」记入数据库
	}
	l.env.DB.SaveRecord(savePath, cloudFid, size)
	return nil
}

// upStrmTask 处理本地新增的 .strm 文件（用户从别处拷来的索引）：不重新上传视频本体，
// 而是按 pickcode 找到云端已有视频 → 移动到目标目录 → 改回原名 → 重写本地 .strm。
func (l *instance) upStrmTask(ctx context.Context, cid, fPath string) error {
	pickcode, fid := ExtractPickcode(fPath)
	if pickcode == "" {
		return fmt.Errorf("[%s]: 无pickcode", fPath)
	}
	if fid == "" {
		info, err := l.env.API.GetDownloadUrl(ctx, pickcode, "115tools")
		if err != nil {
			return fmt.Errorf("[%s]: 获取fid失败: %w", fPath, err)
		}
		fid = info.Fid
	}

	if err := l.env.API.MoveFile(ctx, fid, cid); err != nil {
		return fmt.Errorf("[%s]: 移动云端视频失败: %w", fPath, err)
	}

	origName := strings.TrimSuffix(filepath.Base(fPath), ".strm")
	newName, err := l.env.API.UpdateFile(ctx, fid, origName)
	if err != nil {
		return fmt.Errorf("[%s]: 云端改名失败: %w", fPath, err)
	}
	if newName != origName+filepath.Ext(newName) {
		if _, err := l.env.API.UpdateFile(ctx, fid, origName+filepath.Ext(newName)); err != nil {
			return fmt.Errorf("[%s]: 云端扩展名修复失败: %w", fPath, err)
		}
	}

	if err := l.env.SaveStrmFile(pickcode, fid, fPath); err != nil {
		return fmt.Errorf("[%s]: 文件写入失败: %w", fPath, err)
	}
	l.env.DB.SaveRecord(fPath, fid, time.Now().Unix())
	return nil
}
