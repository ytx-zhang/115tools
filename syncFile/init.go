package syncFile

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
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

type SyncFile struct {
	api *drive.Open115
	db  *db.DB

	syncPath string
	syncFid  string

	tempPath string
	tempFid  string

	strmPath string
	strmUrl  string

	sem chan struct{}

	localSyncMu    sync.Mutex
	localSyncPaths map[string]struct{}
	localSyncChan  chan struct{}

	cloudCancelFunc context.CancelCauseFunc
	cloudSyncLocker sync.RWMutex
	CloudSyncStats  taskStats

	addStrmCancelFunc context.CancelCauseFunc
	moveFids          []string
	AddStrmStats      taskStats
}

func New(ctx context.Context, cfg *config.Config, api *drive.Open115, db *db.DB, wg *sync.WaitGroup) (*SyncFile, error) {
	s := &SyncFile{
		api: api,
		db:  db,

		syncPath: cfg.SyncPath,
		tempPath: cfg.TempPath,

		strmPath: cfg.StrmPath,
		strmUrl:  cfg.StrmUrl,

		sem: make(chan struct{}, 5),

		localSyncPaths: make(map[string]struct{}),
		localSyncChan:  make(chan struct{}, 1),

		CloudSyncStats: taskStats{
			failedErrors: []string{},
		},
		AddStrmStats: taskStats{
			failedErrors: []string{},
		},
	}

	if err := s.initRoot(ctx); err != nil {
		return nil, err
	}

	if err := s.initTemp(ctx); err != nil {
		return nil, err
	}
	s.addLocalSyncTask(s.syncPath)
	wg.Go(func() {
		s.startLocalSyncWorker(ctx)
	})
	wg.Go(func() {
		s.watchSyncPath(ctx)
	})
	wg.Go(func() {
		s.cronSync(ctx)
	})
	return s, nil
}

func (s *SyncFile) initRoot(parentCtx context.Context) error {
	if err := context.Cause(parentCtx); err != nil {
		slog.Warn("[任务中止] 初始化同步", "错误信息", err)
		return err
	}
	var ctx context.Context
	ctx, cancel := context.WithCancelCause(parentCtx)
	defer cancel(nil)
	stopWithErr := func(err error) {
		cancel(err)
	}
	dbFid := s.db.GetFid(s.syncPath)
	if dbFid != "" {
		s.syncFid = dbFid
		return nil
	}
	var isCloudScan atomic.Bool
	isCloudScan.Store(true)
	defer isCloudScan.Store(false)
	slog.Info("初次运行，开始初始化云端数据库...")
	info, err := s.api.GetDirInfo(ctx, s.syncPath)
	if err != nil {
		return err
	}
	s.syncFid = info.Fid
	s.db.SaveRecord(s.syncPath, s.syncFid, -1)
	var wg sync.WaitGroup
	wg.Go(func() {
		s.cloudScan(ctx, s.syncPath, s.syncFid, &wg, stopWithErr)
	})
	wg.Wait()

	if err := context.Cause(ctx); err != nil {
		if isCloudScan.Load() {
			slog.Error("云端扫描被中止，正在清理数据库", "错误信息", err)
			s.db.BatchClearPaths([]string{s.syncPath})
		}
		return err
	}

	slog.Info("[初始化] 云端数据库初始化完成")
	return nil
}
func (s *SyncFile) initTemp(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		slog.Warn("[任务中止] Temp目录初始化", "错误信息", err)
		return err
	}
	dbFid := s.db.GetFid(s.tempPath)
	if dbFid != "" {
		s.tempFid = dbFid
		return nil
	}
	info, err := s.api.GetDirInfo(ctx, s.tempPath)
	if err != nil {
		return err
	}
	s.tempFid = info.Fid
	s.db.SaveRecord(s.tempPath, s.tempFid, -1)
	return nil
}

func (s *SyncFile) cloudScan(ctx context.Context, currentPath, currentFid string, wg *sync.WaitGroup, stop func(error)) {
	select {
	case <-ctx.Done():
		return
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	}
	start := time.Now()
	defer func() {
		slog.Debug("[初始化] 文件夹处理完成", "路径", currentPath, "耗时", time.Since(start))
	}()
	flieList, err := s.api.GetFileList(ctx, currentFid)
	if err != nil {
		stop(fmt.Errorf("[初始化] 获取云端列表[%s]失败: %w", currentPath, err))
		return
	}

	for _, cloudFile := range flieList {
		if err := context.Cause(ctx); err != nil {
			return
		}
		cloudFid := cloudFile.Fid
		savePath := filepath.Join(currentPath, cloudFile.Name)
		if cloudFile.IsDir {
			go s.db.SaveRecord(savePath, cloudFid, -1)
			wg.Go(func() {
				s.cloudScan(ctx, savePath, cloudFid, wg, stop)
			})
			continue
		}
		saveSize := cloudFile.Size
		if cloudFile.IsVideo {
			savePath = strings.TrimSuffix(savePath, filepath.Ext(savePath)) + ".strm"
			saveSize = 0
			if info, err := os.Stat(savePath); err == nil {
				if _, localFid := extractPickcode(savePath); localFid == cloudFid {
					saveSize = info.ModTime().Unix()
				}
			}
		}
		go s.db.SaveRecord(savePath, cloudFid, saveSize)
	}
}
func (s *SyncFile) cronSync(ctx context.Context) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			slog.Info("触发定时全量同步任务")
			s.addLocalSyncTask(s.syncPath)
			s.StartCloudSync(ctx)
		case <-ctx.Done():
			return
		}
	}
}
