package syncFile

import (
	"115tools/db"
	"115tools/open115"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var cloudCancelFunc context.CancelCauseFunc
var syncLocker sync.RWMutex

func StartCloudSync(parentCtx context.Context) {
	if stats.running.CompareAndSwap(false, true) {
		syncLocker.Lock()
		start := time.Now()
		defer func() {
			slog.Info("云端文件同步完成", "总数", stats.total.Load(), "耗时", time.Since(start))
			syncLocker.Unlock()
			stats.running.Store(false)
			cloudCancelFunc(nil)
		}()
		stats.Reset()
		var ctx context.Context
		ctx, cloudCancelFunc = context.WithCancelCause(parentCtx)
		slog.Info("开始同步云端文件...")
		var wg sync.WaitGroup
		wg.Go(func() { cloudSync(ctx, syncPath, syncFid, &wg) })
		wg.Wait()
	}
}

func StopCloudSync() {
	if stats.running.Load() && cloudCancelFunc != nil {
		cloudCancelFunc(fmt.Errorf("用户请求停止同步"))
	}
}
func cloudSync(ctx context.Context, cloudPath string, cloudFID string, wg *sync.WaitGroup) {
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return
	}

	_, count, folderCount, err := open115.FolderInfo(ctx, cloudPath)
	if err != nil {
		slog.Error("获取文件夹信息失败", "路径", cloudPath, "错误信息", err)
		return
	}
	cloudTotal := count + folderCount
	dbTotal := db.GetTotalCount(cloudPath)
	if cloudTotal == dbTotal {
		return
	}

	items, err := open115.FileList(ctx, cloudFID)
	if err != nil {
		slog.Error("获取文件列表失败", "路径", cloudPath, "错误信息", err)
		return
	}

	for _, item := range items {
		if err := context.Cause(ctx); err != nil {
			return
		}
		fid := item.Get("fid").String()
		pc := item.Get("pc").String()
		if item.Get("aid").String() != "1" {
			continue
		}

		fullPath := filepath.Join(cloudPath, item.Get("fn").String())
		if item.Get("fc").String() == "0" {
			if db.GetFid(fullPath) == "" {
				_ = os.MkdirAll(fullPath, 0755)
				db.SaveRecord(fullPath, fid, -1)
			}
			wg.Go(func() {
				cloudSync(ctx, fullPath, fid, wg)
			})
			continue
		}

		isStrm := item.Get("isv").Int() == 1
		savedPath := fullPath
		saveSize := item.Get("fs").Int()

		if isStrm {
			savedPath = strings.TrimSuffix(fullPath, filepath.Ext(fullPath)) + ".strm"
			saveSize = time.Now().Unix()
		}

		dbFid := db.GetFid(savedPath)
		if dbFid != "" && dbFid != fid {
			if err := open115.DeleteFile(ctx, fid); err != nil {
				markFailed(fmt.Sprintf("[%s] 清理云端冗余项失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
			}
			continue
		}
		if dbFid != "" {
			continue
		}
		stats.total.Add(1)
		if isStrm {
			if err := open115.SaveStrmFile(pc, fid, savedPath); err != nil {

				slog.Error("创建strm文件失败", "文件", savedPath, "错误", err)
				markFailed(fmt.Sprintf("[%s] 创建strm文件失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
				continue
			}
			slog.Info("新增STRM文件", "文件", savedPath)
		} else {
			if err := open115.DownloadFile(ctx, pc, savedPath); err != nil {
				slog.Error("下载文件失败", "文件", savedPath, "错误", err)
				markFailed(fmt.Sprintf("[%s] 下载文件失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
				continue
			}
			slog.Info("下载文件成功", "文件", savedPath)
		}
		db.SaveRecord(savedPath, fid, saveSize)
		stats.completed.Add(1)
	}
}
