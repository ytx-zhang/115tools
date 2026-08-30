package push

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/engine/shared"
	"github.com/ytx-zhang/115tools/internal/index"
	"github.com/ytx-zhang/115tools/internal/journal"
)

// Scanner 本地扫描比对模块（纯比对逻辑，不含调度）。
// ScanDir 必须串行调用（仅 dirpool 单消费者），并发会复活跨目录双传 Bug。
type Scanner struct {
	idx   *index.Index
	paths *shared.TaskPaths
	rules shared.Rules
	up    *Uploader
	co    *CloudOps
}

// NewScanner 构造扫描模块。
func NewScanner(deps *shared.Deps, up *Uploader, co *CloudOps) *Scanner {
	return &Scanner{idx: deps.Index, paths: deps.Paths, rules: deps.Rules, up: up, co: co}
}

// ScanDir 比对索引与本地内容，逐项就地执行动作，再等本批上传完成。
func (sc *Scanner) ScanDir(ctx context.Context, currentPath string, batch *sync.WaitGroup) {
	sc.scanDir(ctx, currentPath, batch)
	batch.Wait() // 等本批投递的上传全部完成
}

// scanDir 扫描单个目录（根目录用 Info 保证全量扫描可见，子目录用 Debug 防刷屏）。
func (sc *Scanner) scanDir(ctx context.Context, currentPath string, batch *sync.WaitGroup) {
	logf := journal.Debug
	if currentPath == sc.paths.LocalDir {
		logf = journal.Info
	}
	logf(ctx, "扫描本地文件", "处理目录", currentPath)
	start := time.Now()
	defer func() {
		logf(ctx, "本地文件扫描完成", "处理目录", currentPath, "耗时", time.Since(start))
	}()

	if context.Cause(ctx) != nil {
		return
	}

	localFiles, err := readLocalDir(currentPath, sc.rules, sc.paths.CacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			journal.Debug(ctx, "本地目录已不存在，兜底清理云端残留", "路径", currentPath)
			if cerr := sc.co.CloudCleanTask(ctx, currentPath); cerr != nil {
				journal.Debug(ctx, "本地目录已删除，兜底清理云端时部分项已处理", "路径", currentPath, "错误", cerr)
			}
			return
		}
		journal.Error(ctx, "读取本地目录失败", "路径", currentPath, "错误", err)
		return
	}

	dbChildren := sc.idx.Children(ctx, currentPath)

	for _, ch := range dbChildren {
		if context.Cause(ctx) != nil {
			break
		}
		name, dbFid, dbSize := ch.Name, ch.Fid, ch.Size
		entry, exists := localFiles[name]
		fullPath := filepath.Join(currentPath, name)

		if !exists {
			journal.Info(ctx, "扫描发现本地已删", "路径", fullPath, "DB大小", dbSize)
			sc.HandleFile(ctx, batch, fullPath, dbFid, dbSize, nil)
			continue
		}
		delete(localFiles, name)

		if entry.IsDir() {
			if dbSize == index.SizeDir {
				sc.handleDir(ctx, fullPath, batch)
			}
			continue
		}
		fileInfo, ferr := entry.Info()
		if ferr != nil {
			journal.Warn(ctx, "读取文件信息失败，按本地已删除处理", "路径", fullPath, "错误", ferr)
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
			sc.handleDir(ctx, fullPath, batch)
			continue
		}
		fileInfo, err := entry.Info()
		if err != nil {
			journal.Warn(ctx, "读取新增文件信息失败，跳过", "路径", fullPath, "错误", err)
			continue
		}
		sc.HandleFile(ctx, batch, fullPath, "", 0, fileInfo)
	}
}

// handleDir 处理目录项：先建云端目录，再递归下钻（共享同一 batch wg）。
func (sc *Scanner) handleDir(ctx context.Context, fullPath string, batch *sync.WaitGroup) {
	if _, err := sc.co.AddCloudFolder(ctx, fullPath); err != nil {
		journal.Error(ctx, "创建云端目录失败", "路径", fullPath, "错误", err)
		return
	}
	sc.scanDir(ctx, fullPath, batch)
}

// HandleFile 比对索引与本地文件并就地执行动作（删云端/上传/刷新索引）。
// 是「该不该上传」的唯一收敛点，Watcher 直传与 ScanDir 下钻共用。
func (sc *Scanner) HandleFile(ctx context.Context, batch *sync.WaitGroup, fullPath, dbFid string, dbSize int64, fileInfo os.FileInfo) {
	if fileInfo == nil {
		if dbFid == "" {
			return // 本地已删且索引无记录，无事可做
		}
		sc.cleanCloud(ctx, fullPath)
		return
	}

	// 扩展名判定只在确认文件存在后才有意义（删除分支用不到）
	ext := filepath.Ext(fullPath)
	isStrm := shared.IsStrmPath(fullPath)
	isVideo := sc.rules.IsVideoExt(ext)

	if dbFid == "" {
		sc.enqueueUpload(ctx, batch, fullPath) // 本地新增
		return
	}

	if isVideo {
		strmKey := shared.VideoToStrmPath(fullPath)
		if fid, _ := sc.idx.Get(ctx, strmKey); fid != "" && !sc.rules.CheckVideo(ext, fileInfo.Size()) {
			journal.Debug(ctx, "同名 strm 已在库且视频未达体积阈值，跳过上传", "路径", fullPath, "strm", strmKey)
			return
		}
	}

	if isStrm {
		if fileInfo.ModTime().Unix() == dbSize {
			return // mtime 未变
		}
		if matched, rewrote, mt := shared.NormalizeOwnedStrm(sc.paths.StrmURL, fullPath, dbFid, fileInfo.ModTime().Unix()); matched {
			if rewrote {
				journal.Debug(ctx, "规范化 STRM 链接", "路径", fullPath)
			}
			sc.idx.Put(ctx, fullPath, dbFid, mt)
			return
		}
		sc.cleanCloud(ctx, fullPath)
		sc.enqueueUpload(ctx, batch, fullPath)
		return
	}

	if fileInfo.Size() == dbSize {
		return // size 未变
	}
	sc.cleanCloud(ctx, fullPath)
	sc.enqueueUpload(ctx, batch, fullPath)
}

// cleanCloud 清理云端同名项（删除失败仅记日志，不中断后续上传）。
func (sc *Scanner) cleanCloud(ctx context.Context, fullPath string) {
	if err := sc.co.CloudCleanTask(ctx, fullPath); err != nil {
		journal.Error(ctx, "云端删除失败", "路径", fullPath, "错误", err)
	}
}

// enqueueUpload 入队一个已判定「需上传」的文件。
func (sc *Scanner) enqueueUpload(ctx context.Context, batch *sync.WaitGroup, fPath string) {
	if _, err := os.Stat(fPath); err != nil {
		journal.Warn(ctx, "同步的文件不存在，跳过", "路径", fPath)
		return
	}
	parentFid := sc.idx.GetFid(ctx, filepath.Dir(fPath))
	if parentFid == "" {
		journal.Warn(ctx, "无法获取父目录 FID，跳过", "路径", fPath)
		return
	}
	sc.up.AddUpFile(ctx, batch, parentFid, fPath)
}

// readLocalDir 读取目录到 map（跳过上传排除项与本地缓存根目录）。
func readLocalDir(path string, rules shared.Rules, cacheDir string) (map[string]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	m := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
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
