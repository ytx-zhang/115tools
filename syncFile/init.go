package syncFile

import (
	"115tools/config"
	"115tools/db"
	"115tools/open115"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	syncPath string
	syncFid  string
	tempPath string
	tempFid  string

	sem = make(chan struct{}, 5)
)

func InitSync(parentCtx context.Context) (err error) {
	slog.Info("开始初始化...")

	config := config.Get()
	syncPath = config.SyncPath
	tempPath = config.TempPath

	if syncPath == "" {
		return fmt.Errorf("SyncPath 未设置")
	}

	if syncFid, err = initRoot(parentCtx); err != nil {
		return
	}

	if tempFid, err = initTemp(parentCtx); err != nil {
		return
	}
	localSync(parentCtx, syncPath, syncFid)

	slog.Info("初始化结束")
	return
}

func initRoot(parentCtx context.Context) (fid string, err error) {
	if err = context.Cause(parentCtx); err != nil {
		slog.Warn("[任务中止] 初始化同步", "错误信息", err)
		return
	}
	var ctx context.Context
	ctx, cancel := context.WithCancelCause(parentCtx)
	defer cancel(nil)
	stopWithErr := func(err error) {
		cancel(err)
	}
	fid = db.GetFid(syncPath)
	if fid != "" {
		return
	}
	var isCloudScan atomic.Bool
	isCloudScan.Store(true)
	defer isCloudScan.Store(false)
	slog.Info("初次运行，开始初始化云端数据库...")
	if fid, _, _, err = open115.FolderInfo(ctx, syncPath); err != nil {
		return
	}
	db.SaveRecord(syncPath, fid, -1)
	var wg sync.WaitGroup
	wg.Go(func() {
		cloudScan(ctx, syncPath, fid, &wg, stopWithErr)
	})
	wg.Wait()

	if err = context.Cause(ctx); err != nil {
		if isCloudScan.Load() {
			slog.Error("云端扫描被中止，正在清理数据库", "错误信息", err)
			db.BatchClearPaths([]string{syncPath})
		}
		return
	}

	slog.Info("[初始化] 云端数据库初始化完成")
	return
}
func initTemp(ctx context.Context) (fid string, err error) {
	if err = context.Cause(ctx); err != nil {
		slog.Warn("[任务中止] Temp目录初始化", "错误信息", err)
		return
	}
	fid = db.GetFid(tempPath)
	if fid != "" {
		return
	}
	fid, _, _, err = open115.FolderInfo(ctx, tempPath)
	if err != nil {
		return
	}
	db.SaveRecord(tempPath, fid, -1)
	return
}

func cloudScan(ctx context.Context, cloudPath, cloudFid string, wg *sync.WaitGroup, stop func(error)) {
	select {
	case <-ctx.Done():
		return
	case sem <- struct{}{}:
		defer func() { <-sem }()
	}
	start := time.Now()
	defer func() {
		slog.Info("[初始化] 文件夹处理完成", "路径", cloudPath, "耗时", time.Since(start))
	}()
	list, err := open115.FileList(ctx, cloudFid)
	if err != nil {
		stop(fmt.Errorf("[初始化] 获取云端列表[%s]失败: %w", cloudPath, err))
		return
	}

	for _, item := range list {
		if err := context.Cause(ctx); err != nil {
			return
		}
		itemFid := item.Get("fid").String()
		if item.Get("aid").String() != "1" {
			continue
		}
		fullPath := filepath.Join(cloudPath, item.Get("fn").String())
		if item.Get("fc").String() == "0" {
			go db.SaveRecord(fullPath, itemFid, -1)
			wg.Go(func() {
				cloudScan(ctx, fullPath, itemFid, wg, stop)
			})
			continue
		}
		savePath := fullPath
		saveSize := item.Get("fs").Int()
		if item.Get("isv").Int() == 1 {
			savePath = strings.TrimSuffix(fullPath, filepath.Ext(fullPath)) + ".strm"
			saveSize = 0
			if info, err := os.Stat(savePath); err == nil {
				content, err := os.ReadFile(savePath)
				if err == nil {
					if _, localFid := extractPickcode(string(content)); localFid == itemFid {
						saveSize = info.ModTime().Unix()
					}
				}
			}
		}
		go db.SaveRecord(savePath, itemFid, saveSize)
	}
}
