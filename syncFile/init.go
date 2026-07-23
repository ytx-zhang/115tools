package syncFile

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
	"context"
	"sync"
	"time"
)

// ──── 子组件类型 ────

// pathConfig 缓存所有已初始化的云端路径及其对应的 FID。
// 在 New() 中设置，之后只读。
type pathConfig struct {
	SyncPath string        // 主同步目录
	SyncFid  string        // 主同步目录的云端 FID
	TempPath string        // 回收/临时目录
	TempFid  string        // 回收目录的云端 FID
	StrmPath string        // STRM 生成起始目录
	StrmUrl  string        // STRM 文件中的 302 直链 URL
	Settle   time.Duration // 本地同步静默窗口：监听事件后等待该时长内无新事件才同步
}

// localSyncState 本地文件同步子系统的运行时状态。
type localSyncState struct {
	mu    sync.Mutex
	paths map[string]time.Time // 待处理路径 → 最后事件时间（settle 静默判定用）
	ch    chan struct{}        // 触发同步的信号 channel
}

// CloudSyncState 云端文件同步子系统的运行时状态。
// Stats 字段导出供 main.go 的 SSE 接口读取进度。
type CloudSyncState struct {
	cancelFunc context.CancelCauseFunc
	Stats      taskStats
}

// StrmGenState STRM 生成子系统的运行时状态。
// Stats 字段导出供 main.go 的 SSE 接口读取进度。
type StrmGenState struct {
	cancelFunc context.CancelCauseFunc
	moveFidsMu sync.Mutex // 保护 moveFids 的并发追加
	moveFids   []string
	Stats      taskStats
}

// ──── SyncFile 协调器 ────

// SyncFile 协调本地文件同步、云端文件同步、STRM 生成三个子系统。
//
// 【生命周期】
//  1. New() → initRoot 扫描云端建立 DB 索引 → initTemp 获取回收目录 FID
//     → 启动本地同步 worker + 文件监听器 + 定时全量同步（cronSync）
//  2. 本地同步（始终运行）→ 文件监听器触发 → addLocalSyncTask → localSync → localScan
//  3. 云端同步（HTTP 或定时触发）→ StartCloudSync → WalkCloud 遍历比较 DB 与云端差异
//  4. STRM 生成（HTTP 触发）→ StartAddStrm → WalkCloud 遍历生成 .strm
//  5. 定时全量同步 → cronSync 每 12 小时同时触发本地 + 云端
//
// 【并发约定】
//   - 本地与云端同步不用锁互斥，靠幂等避让：本地事件经 settle 静默窗口后延迟处理，
//     而云端同步「先写文件、紧接着写 DB」，待本地处理时 DB 记录已就位，
//     processPath 基于「本地 stat + DB 记录」判定为无变化，不会把云端刚下载的文件反向上传。
//   - sem 是云端 API 调用并发额度（容量 5），WalkCloud 遍历各目录节点时共用。
//   - 本地上传由固定 worker 池（uploadWorkerCount 个常驻协程）消费 uploadJobs 队列执行。
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

	// ── 本地上传并发控制 ──
	uploadJobs chan uploadJob // 固定 worker 池的任务队列；uploadWorkerCount 个常驻 worker 消费

	// ── 云端文件同步 ──
	Cloud CloudSyncState

	// ── STRM 生成 ──
	Strm StrmGenState
}

// settleDuration 将配置中的秒数转为静默窗口；0 或负数使用默认 15 秒。
func settleDuration(secs int) time.Duration {
	if secs <= 0 {
		return 15 * time.Second
	}
	return time.Duration(secs) * time.Second
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
			Settle:   settleDuration(cfg.SettleSeconds),
		},

		sem:        make(chan struct{}, 5),
		uploadJobs: make(chan uploadJob, 64),

		local: localSyncState{
			paths: make(map[string]time.Time),
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
	wg.Go(func() { s.startUploadWorkers(ctx, uploadWorkerCount) })
	wg.Go(func() { s.cronSync(ctx) })
	return s, nil
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
