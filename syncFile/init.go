package syncFile

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ──── 子组件类型 ────

// pathConfig 缓存所有已初始化的云端路径及其对应的 FID。
// 在 New() 中设置，之后只读。
type pathConfig struct {
	SyncPath string // 主同步目录
	SyncFid  string // 主同步目录的云端 FID
	TempPath string // 回收/临时目录
	TempFid  string // 回收目录的云端 FID
	StrmPath string // STRM 生成起始目录
	StrmUrl  string // STRM 文件中的 302 直链 URL
}

// localSyncState 本地文件同步子系统的运行时状态。
type localSyncState struct {
	mu    sync.Mutex
	paths map[string]struct{} // 待同步的本地目录集合
	ch    chan struct{}       // 触发同步的信号 channel
}

// CloudSyncState 云端文件同步子系统的运行时状态。
// Stats 字段导出供 main.go 的 SSE 接口读取进度。
type CloudSyncState struct {
	cancelFunc context.CancelCauseFunc
	locker     sync.RWMutex
	Stats      taskStats
}

// StrmGenState STRM 生成子系统的运行时状态。
// Stats 字段导出供 main.go 的 SSE 接口读取进度。
type StrmGenState struct {
	cancelFunc context.CancelCauseFunc
	moveFids   []string
	Stats      taskStats
}

// ──── SyncFile 协调器 ────

// SyncFile 协调本地文件同步、云端文件同步、STRM 生成三个子系统。
//
// 【生命周期】
//  1. New() → initRoot 扫描云端建立 DB 索引 → initTemp 获取回收目录 FID
//            → 启动本地同步 worker + 文件监听器 + 定时全量同步（cronSync）
//  2. 本地同步（始终运行）→ 文件监听器触发 → addLocalSyncTask → localSync → localScan
//  3. 云端同步（HTTP 或定时触发）→ StartCloudSync → WalkCloud 遍历比较 DB 与云端差异
//  4. STRM 生成（HTTP 触发）→ StartAddStrm → WalkCloud 遍历生成 .strm
//  5. 定时全量同步 → cronSync 每 12 小时同时触发本地 + 云端
//
// 【并发约定】
//  - 本地同步仅在“判断某路径是否需要同步”的极短 DB 读取期间持有 Cloud.locker 读锁（RLock），
//    随后立即释放；扫描与上传等网络 I/O 均在锁外进行，避免长期持锁阻塞云端同步的写锁。
//  - 云端同步持有写锁（Lock），运行期间会短暂阻塞本地同步的读取判定。
//  - sem 是云端 API 调用并发额度（容量 5），WalkCloud 遍历各目录节点时共用。
type SyncFile struct {
	// ── 外部依赖（初始化后只读）──
	api *drive.Open115
	db  *db.DB

	// ── 路径配置（初始化后只读）──
	paths pathConfig

	// ── 云端 API 并发控制 ──
	sem chan struct{} // 容量 5

	// ── 本地文件同步 ──
	local localSyncState

	// ── 云端文件同步 ──
	Cloud CloudSyncState

	// ── STRM 生成 ──
	Strm StrmGenState
}

// New 初始化 SyncFile 并启动后台协程。
func New(ctx context.Context, cfg *config.Config, api *drive.Open115, boltDB *db.DB, wg *sync.WaitGroup) (*SyncFile, error) {
	s := &SyncFile{
		api: api,
		db:  boltDB,

		paths: pathConfig{
			SyncPath: cfg.SyncPath,
			TempPath: cfg.TempPath,
			StrmPath: cfg.StrmPath,
			StrmUrl:  cfg.StrmUrl,
		},

		sem: make(chan struct{}, 5),

		local: localSyncState{
			paths: make(map[string]struct{}),
			ch:    make(chan struct{}, 1),
		},

		Cloud: CloudSyncState{
			Stats: taskStats{failedErrors: []string{}},
		},
		Strm: StrmGenState{
			Stats: taskStats{failedErrors: []string{}},
		},
	}

	if err := s.initRoot(ctx); err != nil {
		return nil, err
	}

	if err := s.initTemp(ctx); err != nil {
		return nil, err
	}
	s.addLocalSyncTask(s.paths.SyncPath)
	wg.Go(func() { s.startLocalSyncWorker(ctx) })
	wg.Go(func() { s.watchSyncPath(ctx) })
	wg.Go(func() { s.cronSync(ctx) })
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
	dbFid := s.db.GetFid(s.paths.SyncPath)
	if dbFid != "" {
		s.paths.SyncFid = dbFid
		return nil
	}
	var isCloudScan atomic.Bool
	isCloudScan.Store(true)
	defer isCloudScan.Store(false)
	slog.Info("初次运行，开始初始化云端数据库...")
	info, err := s.api.GetDirInfo(ctx, s.paths.SyncPath)
	if err != nil {
		return err
	}
	s.paths.SyncFid = info.Fid
	s.db.SaveRecord(s.paths.SyncPath, s.paths.SyncFid, db.SizeDir)
	// 批量写入器：cloudScan 大批量写入合并为少量事务；defer 保证 wg.Wait 之后落盘。
	writer := db.NewBatchWriter(s.db, 0)
	defer writer.Flush()

	var scanErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		scanErr = s.WalkCloud(ctx, s.paths.SyncPath, s.paths.SyncFid, CloudVisitor{
			EnterDir: func(_ context.Context, path, fid string) (bool, error) {
				writer.Put(path, fid, db.SizeDir)
				return true, nil
			},
			VisitFile: func(_ context.Context, path, fid, _ string, e CloudEntry) error {
				saveSize := e.Size
				if e.IsVideo {
					path = strings.TrimSuffix(path, filepath.Ext(path)) + ".strm"
					saveSize = 0
					if info, err := os.Stat(path); err == nil {
						if _, localFid := extractPickcode(path); localFid == fid {
							saveSize = info.ModTime().Unix()
						}
					}
				}
				writer.Put(path, fid, saveSize)
				return nil
			},
		}, stopWithErr)
	})
	wg.Wait()
	if scanErr != nil {
		cancel(scanErr)
	}

	if err := context.Cause(ctx); err != nil {
		if isCloudScan.Load() {
			slog.Error("云端扫描被中止，正在清理数据库", "错误信息", err)
			s.db.BatchClearPaths([]string{s.paths.SyncPath})
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
	dbFid := s.db.GetFid(s.paths.TempPath)
	if dbFid != "" {
		s.paths.TempFid = dbFid
		return nil
	}
	info, err := s.api.GetDirInfo(ctx, s.paths.TempPath)
	if err != nil {
		return err
	}
	s.paths.TempFid = info.Fid
	s.db.SaveRecord(s.paths.TempPath, s.paths.TempFid, db.SizeDir)
	return nil
}

// acquireSlot 获取一个并发执行配额，ctx 取消时返回 false。
func (s *SyncFile) acquireSlot(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case s.sem <- struct{}{}:
		return true
	}
}

func (s *SyncFile) cronSync(ctx context.Context) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			slog.Info("触发定时全量同步任务")
			s.addLocalSyncTask(s.paths.SyncPath)
			s.StartCloudSync(ctx)
		case <-ctx.Done():
			return
		}
	}
}
