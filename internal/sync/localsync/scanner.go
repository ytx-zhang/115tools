package localsync

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// Scanner 本地扫描比对小模块（纯比对逻辑）。
// 依赖：api/db/paths/rules（判定与路径）、up/co（上传执行器与云端清理器）。上传并发限制在 uploader.DoUpload 内部。
// 串行约束：ScanDir 必须由调用方串行调用（当前仅 dirpool ConsumeLoop 单消费者调用：
// 取一个目录→wg.Wait 本批传完→再取下一个），并发调用会复活跨目录双传 Bug（同一文件被两路扫描同时判定为新增而双传）。
//
// 目录调度（哪些目录要扫、串行消化、驱动 running 状态）由单独的目录池负责，见 dirpool.go。
// 本模块只负责「比对一个给定目录并就地投递上传动作」，不含任何调度职责。
type Scanner struct {
	api   *drive.Client
	db    *store.Store
	paths *common.Paths
	rules common.Rules
	up    *Uploader
	co    *CloudOps
	task  *common.Task // 本地同步进度任务（消费循环开头 Reset、计数统一在 uploader.AddUpFile 内完成）

	dirPool // 目录池：待处理目录通道 + 去重 pending（见 dirpool.go）
}

// NewScanner 构造 scanner 小模块（依赖注入）。
func NewScanner(deps *common.SyncDeps, up *Uploader, co *CloudOps, task *common.Task) *Scanner {
	return &Scanner{api: deps.API, db: deps.DB, paths: deps.Paths, rules: deps.Rules, up: up, co: co, task: task,
		dirPool: dirPool{dirCh: make(chan string, 64)}}
}

// ScanDir 对比数据库记录与本地实际内容，逐个交给 HandleFile 直接执行动作（删云端/上传/刷新DB）。
// 幂等；由调用方保证串行（当前仅 dirpool ConsumeLoop 单消费者串行调用），无需全局互斥锁。两步：
// ① DB 子项：本地已删→删云端；都在→目录走 handleDir、文件走 HandleFile；
// ② 本地新增项：建云端目录 / 走 HandleFile 上传。
// ⚠️ 删除（CloudCleanTask）与上传（AddUpFile）已在 HandleFile 内直接执行；
// 扫描只投递任务，不阻塞等待上传，故本函数无返回值、不统计上传数量。
// ⚠️ 批次等待（batch *sync.WaitGroup）在扫描完成后进行：串行模型下同一时刻仅一个 ScanDir 在跑，
// 无需全局锁；扫描完即开始等待本批上传，消费者下一轮 ScanDir 取新目录时本批上传已（或仍在）完成，互不干扰。
// batch 由消费者（ConsumeLoop）为本次目录构造并透传，递归下钻的 HandleFile 共享同一 wg；
// 消费末尾 wg.Wait() 即覆盖「扫描 + 本批上传完」。
func (sc *Scanner) ScanDir(ctx context.Context, currentPath string, recursive bool, batch *sync.WaitGroup) {
	sc.scanDirLocked(ctx, currentPath, recursive, batch)
	// 等待本批（batch）投递的上传全部完成，正常归零。
	if batch != nil {
		batch.Wait()
	}
}

// scanDirLocked 实际扫描逻辑（供顶层 ScanDir 与递归下钻共用）。
func (sc *Scanner) scanDirLocked(ctx context.Context, currentPath string, recursive bool, batch *sync.WaitGroup) {
	logs.Debug(logs.ModuleSync, "扫描本地文件", "处理目录", currentPath)
	start := time.Now()
	defer func() {
		logs.Debug(logs.ModuleSync, "本地文件扫描完成", "处理目录", currentPath, "耗时", time.Since(start))
	}()

	if err := ctx.Err(); err != nil {
		return
	}

	localFiles, err := readLocalDir(currentPath, sc.rules)
	if err != nil {
		if os.IsNotExist(err) {
			logs.Debug(logs.ModuleSync, "本地目录已不存在，兜底清理云端残留", "路径", currentPath)
			if cerr := sc.co.CloudCleanTask(ctx, currentPath); cerr != nil {
				logs.Debug(logs.ModuleSync, "本地目录已删除，兜底清理云端时部分项已处理", "路径", currentPath, "错误", cerr)
			}
			return
		}
		logs.Error(logs.ModuleSync, "读取本地目录失败", "路径", currentPath, "错误", err)
		return
	}

	dbChildren := sc.db.ScanChildren(ctx, currentPath)

	for _, ch := range dbChildren {
		if err := ctx.Err(); err != nil {
			break
		}
		name, dbFid, dbSize := ch.Name, ch.Fid, ch.Size
		entry, exists := localFiles[name]
		fullPath := filepath.Join(currentPath, name)

		if !exists {
			logs.Debug(logs.ModuleSync, "扫描发现本地已删",
				"路径", fullPath, "DB大小", dbSize)
			// 本地已删 → 直接删云端（DB 由 CloudCleanTask 末尾清）
			sc.HandleFile(ctx, batch, fullPath, dbFid, dbSize, nil)
			continue
		}
		delete(localFiles, name)

		if entry.IsDir() {
			if dbSize == store.SizeDir {
				sc.handleDir(ctx, fullPath, recursive, batch)
			}
			continue
		}
		fileInfo, ferr := entry.Info()
		if ferr != nil {
			logs.Warn(logs.ModuleSync, "读取文件信息失败，按本地已删除处理", "路径", fullPath, "错误", ferr)
			// 读失败当已删 → 直接删云端（用 fullPath 而非 dbFid，便于 CloudCleanTask 反查）
			sc.HandleFile(ctx, batch, fullPath, dbFid, dbSize, nil)
			continue
		}
		sc.HandleFile(ctx, batch, fullPath, dbFid, dbSize, fileInfo)
	}

	for name, entry := range localFiles {
		if err := ctx.Err(); err != nil {
			break
		}
		fullPath := filepath.Join(currentPath, name)
		if entry.IsDir() {
			sc.handleDir(ctx, fullPath, recursive, batch)
			continue
		}
		// 本地新增文件：DB 无记录（dbFid/dbSize 空），统一走 HandleFile（含视频阈值护栏）
		fileInfo, err := entry.Info()
		if err != nil {
			logs.Warn(logs.ModuleSync, "读取新增文件信息失败，跳过", "路径", fullPath, "错误", err)
			continue
		}
		sc.HandleFile(ctx, batch, fullPath, "", 0, fileInfo)
	}
}

// handleDir 处理一个目录项：新增目录（DB 无记录）先 AddCloudFolder 建云端目录，
// recursive 模式下再递归下钻扫描子树。
func (sc *Scanner) handleDir(ctx context.Context, fullPath string, recursive bool, batch *sync.WaitGroup) {
	if _, err := sc.co.AddCloudFolder(ctx, fullPath); err != nil {
		logs.Error(logs.ModuleSync, "创建云端目录失败", "路径", fullPath, "错误", err)
		return
	}
	if !recursive {
		return
	}
	sc.scanDirLocked(ctx, fullPath, recursive, batch) // 递归下钻，复用同一 batch wg（串行模型无并发双传风险）
}

// HandleFile 处理一个文件：直接比对 DB 记录与本地文件并**就地执行动作**（不再返回待办）。
// 动作分支（删云端/上传/刷新DB 均在此内完成）：
//   - 本地不可用（fileInfo==nil）→ 删云端 fid + 清 DB；
//   - 本地新增（dbFid==""）→ 直接上传；
//   - 视频同名 .strm 已在库且 <阈值(10MB) → 跳过（避免残缺小视频，v3 需求）；
//   - .strm：mtime 未变 → 直接跳过（DB 本就有记录）；mtime 变 → 删旧视频 + 重传；
//   - 普通文件：size 未变 → 直接跳过（DB 本就有记录）；size 变 → 上传；
//   - 视频：size 变且达阈值 → 上传（覆盖旧视频）；无 .strm 记录 → 直接上传。
//
// 本函数无返回值：是否触发上传由内部直接执行（enqueueUpload），调用方无需计数。
// 两条路径（ScanDir 下钻、watch.uploadVideo）共用同一入口，避免判定逻辑分裂。
// batch 为本次目录任务的批次 wg（扫描下钻透传同一 wg；视频直传传独立 wg 或 nil 不入批）。
func (sc *Scanner) HandleFile(ctx context.Context, batch *sync.WaitGroup, fullPath, dbFid string, dbSize int64, fileInfo os.FileInfo) {
	ext := filepath.Ext(fullPath)
	isStrm := common.IsStrmPath(fullPath)
	isVideo := sc.rules.IsVideoExt(ext)

	if fileInfo == nil {
		// 本地已删但 DB 也无记录（如上传视频转 .strm 时 os.Remove 触发的删除事件）：
		// 无事可做，直接返回，避免无意义的清理日志。
		if dbFid == "" {
			return
		}
		// 本地不可用/已删且有 DB 记录 → 直接删云端（DB 由 CloudCleanTask 末尾清）
		if cerr := sc.co.CloudCleanTask(ctx, fullPath); cerr != nil {
			logs.Error(logs.ModuleSync, "云端删除失败", "路径", fullPath, "错误", cerr)
		}
		return
	}

	if dbFid == "" {
		// 本地新增、DB 无记录 → 直接上传（视频/普通皆然）
		sc.enqueueUpload(ctx, batch, fullPath)
		return
	}

	if isVideo {
		// 同名 .strm 已在库：未达体积阈值则跳过（已达阈值仍走下方上传逻辑覆盖旧视频）
		strmKey := common.VideoToStrmPath(fullPath)
		if fid, _ := sc.db.GetInfo(strmKey); fid != "" && !sc.rules.CheckVideo(ext, fileInfo.Size()) {
			logs.Debug(logs.ModuleSync, "同名 strm 已在库且视频未达体积阈值，跳过上传", "路径", fullPath, "strm", strmKey)
			return
		}
	}

	if isStrm {
		if fileInfo.ModTime().Unix() == dbSize {
			return // mtime 未变 → 本就有记录，无需重写
		}
		// mtime 变 → 删旧视频 + 重传
		if cerr := sc.co.CloudCleanTask(ctx, fullPath); cerr != nil {
			logs.Error(logs.ModuleSync, "云端删除失败", "路径", fullPath, "错误", cerr)
		}
		sc.enqueueUpload(ctx, batch, fullPath)
		return
	}

	if fileInfo.Size() == dbSize {
		return // size 未变 → 本就有记录，无需重写
	}
	// size 变 → 先删云端旧同名文件再重传：115 允许目录内同名文件并存，
	// 只「先传新」不清旧会残留两个同名不同大小的文件（CloudCleanTask 按类型删除）。
	if cerr := sc.co.CloudCleanTask(ctx, fullPath); cerr != nil {
		logs.Error(logs.ModuleSync, "云端删除失败", "路径", fullPath, "错误", cerr)
	}
	sc.enqueueUpload(ctx, batch, fullPath) // size 变 → 上传
}

// enqueueUpload 入队一个已判定「需上传」的文件（投递即返回，不堵塞）。
// 入队前做存在性复查（判定到入队期间文件可能被删）。
// batch 透传给 uploader.AddUpFile：非 nil 时本任务计入批次 wg，消费者末尾 Wait 覆盖本批上传。
// 同一文件若已在传/排队，uploader 内部 inFlight 去重，不会双传。
func (sc *Scanner) enqueueUpload(ctx context.Context, batch *sync.WaitGroup, fPath string) {
	if _, err := os.Stat(fPath); err != nil {
		logs.Warn(logs.ModuleSync, "同步的文件不存在，跳过", "路径", fPath)
		return
	}
	parentFid := sc.db.GetFid(filepath.Dir(fPath))
	if parentFid == "" {
		logs.Warn(logs.ModuleSync, "无法获取父目录FID，跳过", "路径", fPath)
		return
	}
	sc.up.AddUpFile(ctx, batch, parentFid, fPath)
}

// readLocalDir 读取目录内容到 map（文件名→DirEntry）。命中上传排除名单的项直接跳过。
func readLocalDir(path string, rules common.Rules) (map[string]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	m := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		if rules.IsUploadExcluded(e.Name()) {
			continue
		}
		m[e.Name()] = e
	}
	return m, nil
}
