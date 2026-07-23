package syncFile

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ──── 上传 worker 池 ────

// uploadWorkerCount 固定上传 worker 数量：最多同时有这么多上传协程运行。
const uploadWorkerCount = 3

// uploadJob 描述一次上传任务：将本地文件 fPath 上传到云端目录 cid 下。
type uploadJob struct {
	ctx   context.Context
	cid   string
	fPath string
}

// startUploadWorkers 启动固定数量的上传 worker，常驻从 uploadJobs 消费任务。
// ctx 取消时所有 worker 退出。
func (s *SyncFile) startUploadWorkers(ctx context.Context, n int) {
	for range n {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-s.uploadJobs:
					if !ok {
						return
					}
					s.doUpload(job.ctx, job.cid, job.fPath)
				}
			}
		}()
	}
}

// uploadOneFile 将本地文件加入上传队列，由固定 worker 池异步执行上传。
// 调用方（worker / localScan）立即返回、继续处理其余路径，上传在后台以最多 uploadWorkerCount 并发进行。
func (s *SyncFile) uploadOneFile(ctx context.Context, cid, fPath string) {
	select {
	case <-ctx.Done():
	case s.uploadJobs <- uploadJob{ctx: ctx, cid: cid, fPath: fPath}:
	}
}

// doUpload 真正执行一次上传（由 worker 调用），完成 os.Stat → upStrmTask/upFileTask → SaveRecord → 日志。
func (s *SyncFile) doUpload(ctx context.Context, cid, fPath string) {
	if err := ctx.Err(); err != nil {
		return
	}
	fileInfo, err := os.Stat(fPath)
	if err != nil {
		slog.Warn("同步的文件不存在", "文件", fPath)
		return
	}

	isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
	if isStrm {
		err = s.upStrmTask(ctx, cid, fPath)
	} else {
		err = s.upFileTask(ctx, cid, fPath, fileInfo)
	}
	if err != nil {
		slog.Error("同步失败", "文件", fPath, "错误", err)
	} else {
		slog.Info("上传文件完成", "文件", fPath)
	}
}

// ──── 上传任务实现 ────

func (s *SyncFile) upFileTask(ctx context.Context, cid, fPath string, fileInfo os.FileInfo) error {
	info, err := s.api.UploadFile(ctx, fPath, cid, "", "")
	if err != nil {
		return err
	}
	cloudFid := info.Fid
	size := fileInfo.Size()
	savePath := fPath
	ext := filepath.Ext(fPath)
	if checkVideo(ext, size) {
		savePath = strings.TrimSuffix(fPath, ext) + ".strm"
		dbFid := s.db.GetFid(savePath)
		if dbFid != "" {
			if err := s.api.MoveFile(ctx, dbFid, s.paths.TempFid); err != nil {
				return fmt.Errorf("[%s]: 清理旧视频失败: %w", savePath, err)
			}
		}
		if err := s.saveStrmFile(info.PickCode, cloudFid, savePath); err != nil {
			return fmt.Errorf("[%s]: 写入strm文件失败: %w", savePath, err)
		}
		if err := os.Remove(fPath); err != nil {
			return fmt.Errorf("[%s]: 删除视频文件失败: %w", fPath, err)
		}
		size = time.Now().Unix()
	}
	s.db.SaveRecord(savePath, cloudFid, size)
	return nil
}

func (s *SyncFile) upStrmTask(ctx context.Context, cid, fPath string) error {
	pickcode, fid := extractPickcode(fPath)
	if pickcode == "" {
		return fmt.Errorf("[%s]: 无pickcode", fPath)
	}
	if fid == "" {
		info, err := s.api.GetDownloadUrl(ctx, pickcode, "115tools")
		if err != nil {
			return fmt.Errorf("[%s]: 获取fid失败: %w", fPath, err)
		}
		fid = info.Fid
	}

	if err := s.api.MoveFile(ctx, fid, cid); err != nil {
		return fmt.Errorf("[%s]: 移动云端视频失败: %w", fPath, err)
	}

	// 恢复云端文件名（上传后名称为 pickcode，需改回原名）
	origName := strings.TrimSuffix(filepath.Base(fPath), ".strm")
	newName, err := s.api.UpdateFile(ctx, fid, origName)
	if err != nil {
		return fmt.Errorf("[%s]: 云端改名失败: %w", fPath, err)
	}
	// UpdateFile 可能只改了文件名没改扩展名，补齐
	if newName != origName+filepath.Ext(newName) {
		if _, err := s.api.UpdateFile(ctx, fid, origName+filepath.Ext(newName)); err != nil {
			return fmt.Errorf("[%s]: 云端扩展名修复失败: %w", fPath, err)
		}
	}

	if err := s.saveStrmFile(pickcode, fid, fPath); err != nil {
		return fmt.Errorf("[%s]: 文件写入失败: %w", fPath, err)
	}
	s.db.SaveRecord(fPath, fid, time.Now().Unix())
	return nil
}
