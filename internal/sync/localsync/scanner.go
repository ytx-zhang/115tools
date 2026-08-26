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

// Scanner 本地扫描比对模块（纯比对逻辑，不含调度）。
// ⚠️ 串行约束：ScanDir 必须串行调用（当前仅 dirpool 单消费者），并发会复活跨目录双传 Bug。
// 上传并发限制在 uploader.DoUpload 内部。
type Scanner struct {
	api   *drive.Client
	db    *store.Store
	paths *common.Paths
	rules common.Rules
	up    *Uploader
	co    *CloudOps
	task  *common.Task // 本地同步进度（消费循环开头 Reset、计数在 AddUpFile 内）

	dirPool // 目录池（见 dirpool.go）
}

// NewScanner 构造 scanner 小模块（依赖注入）。
func NewScanner(deps *common.Core, up *Uploader, co *CloudOps, task *common.Task) *Scanner {
	return &Scanner{api: deps.API, db: deps.DB, paths: deps.Paths, rules: deps.Rules, up: up, co: co, task: task,
		dirCh: make(chan string, 64), pending: make(map[string]SyncSource)}
}

// ScanDir 比对 DB 与本地内容，逐项就地执行动作（删云端/上传/刷新DB），再等本批上传完。
// 两步：① DB 子项：本地已删→删云端；都在→目录 handleDir、文件 HandleFile；
// ② 本地新增项：建云端目录 / HandleFile 上传。batch 由消费者构造透传，递归下钻共享同一 wg。
func (sc *Scanner) ScanDir(ctx context.Context, currentPath string, recursive bool, batch *sync.WaitGroup) {
	sc.scanDirLocked(ctx, currentPath, recursive, batch)
	if batch != nil {
		batch.Wait() // 等本批投递的上传全部完成
	}
}

// scanDirLocked 实际扫描逻辑（顶层 ScanDir 与递归下钻共用）。
func (sc *Scanner) scanDirLocked(ctx context.Context, currentPath string, recursive bool, batch *sync.WaitGroup) {
	logs.Debug(logs.ModuleSync, "扫描本地文件", "处理目录", currentPath)
	start := time.Now()
	defer func() {
		logs.Debug(logs.ModuleSync, "本地文件扫描完成", "处理目录", currentPath, "耗时", time.Since(start))
	}()

	if context.Cause(ctx) != nil {
		return
	}

	localFiles, err := readLocalDir(currentPath, sc.rules, sc.paths.CacheDir)
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
		if context.Cause(ctx) != nil {
			break
		}
		name, dbFid, dbSize := ch.Name, ch.Fid, ch.Size
		entry, exists := localFiles[name]
		fullPath := filepath.Join(currentPath, name)

		if !exists {
			logs.Debug(logs.ModuleSync, "扫描发现本地已删",
				"路径", fullPath, "DB大小", dbSize)
			sc.HandleFile(ctx, batch, fullPath, dbFid, dbSize, nil) // fileInfo==nil → 删云端
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
			sc.HandleFile(ctx, batch, fullPath, dbFid, dbSize, nil)
			continue
		}
		sc.HandleFile(ctx, batch, fullPath, dbFid, dbSize, fileInfo)
	}

	for name, entry := range localFiles {
		if context.Cause(ctx) != nil {
			break
		}
		fullPath := filepath.Join(currentPath, name)
		if entry.IsDir() {
			sc.handleDir(ctx, fullPath, recursive, batch)
			continue
		}
		fileInfo, err := entry.Info()
		if err != nil {
			logs.Warn(logs.ModuleSync, "读取新增文件信息失败，跳过", "路径", fullPath, "错误", err)
			continue
		}
		sc.HandleFile(ctx, batch, fullPath, "", 0, fileInfo) // 本地新增：DB 无记录，统一走 HandleFile
	}
}

// handleDir 处理目录项：新增目录先建云端目录，recursive 时递归下钻（共享同一 batch wg）。
func (sc *Scanner) handleDir(ctx context.Context, fullPath string, recursive bool, batch *sync.WaitGroup) {
	if _, err := sc.co.AddCloudFolder(ctx, fullPath); err != nil {
		logs.Error(logs.ModuleSync, "创建云端目录失败", "路径", fullPath, "错误", err)
		return
	}
	if !recursive {
		return
	}
	sc.scanDirLocked(ctx, fullPath, recursive, batch)
}

// HandleFile 比对 DB 与本地文件并就地执行动作（删云端/上传/刷新DB）。
// 分支：
//   - 本地不可用（fileInfo==nil）→ 删云端 fid + 清 DB；
//   - 本地新增（dbFid==""）→ 直接上传；
//   - 视频同名 .strm 已在库且 <阈值(10MB) → 跳过（v3 需求：避免残缺小视频）；
//   - .strm：mtime 未变 → 跳过；变 → 删旧视频 + 重传；
//   - 普通文件：size 未变 → 跳过；变 → 先删云端旧同名再上传（防同名并存）；
//   - 视频：size 变且达阈值 → 上传覆盖。
//
// 两条路径（ScanDir 下钻、watch.uploadVideo）共用同一入口，避免判定逻辑分裂。
func (sc *Scanner) HandleFile(ctx context.Context, batch *sync.WaitGroup, fullPath, dbFid string, dbSize int64, fileInfo os.FileInfo) {
	ext := filepath.Ext(fullPath)
	isStrm := common.IsStrmPath(fullPath)
	isVideo := sc.rules.IsVideoExt(ext)

	if fileInfo == nil {
		// 本地已删且 DB 也无记录（如上传视频转 .strm 时 os.Remove 触发的删除事件）：无事可做。
		if dbFid == "" {
			return
		}
		if cerr := sc.co.CloudCleanTask(ctx, fullPath); cerr != nil {
			logs.Error(logs.ModuleSync, "云端删除失败", "路径", fullPath, "错误", cerr)
		}
		return
	}

	if dbFid == "" {
		sc.enqueueUpload(ctx, batch, fullPath) // 本地新增 → 直接上传
		return
	}

	if isVideo {
		// 同名 .strm 已在库：未达体积阈值则跳过（已达阈值仍走下方覆盖逻辑）
		strmKey := common.VideoToStrmPath(fullPath)
		if fid, _ := sc.db.GetInfo(strmKey); fid != "" && !sc.rules.CheckVideo(ext, fileInfo.Size()) {
			logs.Debug(logs.ModuleSync, "同名 strm 已在库且视频未达体积阈值，跳过上传", "路径", fullPath, "strm", strmKey)
			return
		}
	}

	if isStrm {
		if fileInfo.ModTime().Unix() == dbSize {
			return // mtime 未变 → 本就有记录
		}
		// mtime 变：先判定 pickcode 是否未变（解析出的 fid 与 DB 一致）。
		// pc 未变 ⇒ 云端视频没变，仅规范化链接 + 刷新 DB mtime，绝不走
		// 「清旧视频 + 重传」——否则会把云端视频误挪回收目录且搬不回。
		if matched, rewrote, mt := common.NormalizeOwnedStrm(sc.paths.StrmUrl, fullPath, dbFid, fileInfo.ModTime().Unix()); matched {
			if rewrote {
				logs.Debug(logs.ModuleSync, "规范化STRM链接", "路径", fullPath)
			}
			sc.db.SaveRecord(fullPath, dbFid, mt)
			return
		}
		// pickcode 变 / 无法解析 ⇒ 旧链接失效，走「清旧视频 + 重传」
		if cerr := sc.co.CloudCleanTask(ctx, fullPath); cerr != nil {
			logs.Error(logs.ModuleSync, "云端删除失败", "路径", fullPath, "错误", cerr)
		}
		sc.enqueueUpload(ctx, batch, fullPath)
		return
	}

	if fileInfo.Size() == dbSize {
		return // size 未变 → 本就有记录
	}
	// size 变 → 先删云端旧同名再重传：115 允许同名并存，先传新不清旧会残留两个同名不同大小文件。
	if cerr := sc.co.CloudCleanTask(ctx, fullPath); cerr != nil {
		logs.Error(logs.ModuleSync, "云端删除失败", "路径", fullPath, "错误", cerr)
	}
	sc.enqueueUpload(ctx, batch, fullPath)
}

// enqueueUpload 入队一个已判定「需上传」的文件（投递即返回，不堵塞）。
// 入队前做存在性复查（判定到入队期间文件可能被删）；同文件在传/排队由 uploader 内 inFlight 去重。
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

// readLocalDir 读取目录到 map（文件名→DirEntry），跳过上传排除名单项与本地缓存根目录。
func readLocalDir(path string, rules common.Rules, cacheDir string) (map[string]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	m := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		// ⚠️ 跳过本地缓存根目录（<SyncPath>/.cache）：缓存放于源同挂载点以便原子 rename，
		// 绝不能让周期扫描把它当成新增视频重新上传。
		if filepath.Join(path, e.Name()) == cacheDir {
			continue
		}
		if rules.IsUploadExcluded(e.Name()) {
			continue
		}
		m[e.Name()] = e
	}
	return m, nil
}
