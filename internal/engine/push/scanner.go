package push

import (
	"context"
	"os"
	"path/filepath"
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
func (sc *Scanner) ScanDir(ctx context.Context, currentPath string, batch *UpBatch) {
	sc.scanDir(ctx, currentPath, batch)
	batch.Wait() // 等本批投递的上传全部完成
}

// scanDir 扫描单个目录（根目录用 Info 保证全量扫描可见，子目录用 Debug 防刷屏）。
// 只负责「本地现状 ↔ 索引」对表与派发，全部判定与动作下沉到 HandleEntry。
func (sc *Scanner) scanDir(ctx context.Context, currentPath string, batch *UpBatch) {
	logf := journal.Debug
	if currentPath == sc.paths.LocalDir {
		logf = journal.Info
	}
	logf(ctx, "扫描本地文件", "处理目录", currentPath)
	start := time.Now()
	defer func() {
		logf(ctx, "本地文件扫描完成", "处理目录", currentPath, "耗时", time.Since(start))
	}()

	// 目录级取消检查：避免已取消时还白读一次目录与索引（逐项的取消检查在 HandleEntry）。
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

	// 索引有记录的：本地仍在则比对，本地已删则 entry 为 nil（map 取不到即零值）。
	for _, ch := range dbChildren {
		entry := localFiles[ch.Name]
		delete(localFiles, ch.Name)
		sc.HandleEntry(ctx, batch, filepath.Join(currentPath, ch.Name), ch.Fid, ch.Size, entry)
	}

	// 剩下的都是索引无记录的本地新增。
	for name, entry := range localFiles {
		sc.HandleEntry(ctx, batch, filepath.Join(currentPath, name), "", 0, entry)
	}
}

// HandleEntry 是「该不该动作、动什么」的唯一收敛点：比对索引记录与本地现状后就地执行。
// 目录/文件分流、取消、类型不符、读取失败等判定全在此处，ScanDir 下钻与 Watcher 直传共用。
//
// entry == nil 表示本地已不存在；dbFid == "" 表示索引无记录（本地新增）。
func (sc *Scanner) HandleEntry(ctx context.Context, batch *UpBatch, fullPath, dbFid string, dbSize int64, entry os.DirEntry) {
	if context.Cause(ctx) != nil {
		return
	}
	switch {
	case entry == nil:
		sc.handleMissing(ctx, fullPath, dbFid)
	case entry.IsDir():
		sc.handleDir(ctx, batch, fullPath, dbFid, dbSize)
	default:
		fileInfo, err := entry.Info()
		if err != nil {
			journal.Warn(ctx, "读取文件信息失败，按本地已删除处理", "路径", fullPath, "错误", err)
			sc.handleMissing(ctx, fullPath, dbFid)
			return
		}
		sc.handleFile(ctx, batch, fullPath, dbFid, dbSize, fileInfo)
	}
}

// handleMissing 处理本地已不存在：清云端残留（索引无记录则无事可做）。
func (sc *Scanner) handleMissing(ctx context.Context, fullPath, dbFid string) {
	if dbFid == "" {
		return
	}
	journal.Info(ctx, "本地已不存在，清理云端", "路径", fullPath)
	sc.cleanCloud(ctx, fullPath)
}

// handleDir 处理目录项：建云端目录后递归下钻（共享同一 batch）。
// 类型不符（索引记的是文件）以本地为准：先清掉云端同名文件，再按目录建（与 handleFile 的反方向对称）。
func (sc *Scanner) handleDir(ctx context.Context, batch *UpBatch, fullPath, dbFid string, dbSize int64) {
	if dbFid != "" && dbSize != index.SizeDir {
		journal.Info(ctx, "本地为目录、云端为同名文件，以本地为准清理云端文件", "路径", fullPath)
		sc.cleanCloud(ctx, fullPath)
	}
	if _, err := sc.co.AddCloudFolder(ctx, fullPath); err != nil {
		journal.Error(ctx, "创建云端目录失败", "路径", fullPath, "错误", err)
		return
	}
	sc.scanDir(ctx, fullPath, batch)
}

// handleFile 比对索引与本地文件并就地执行动作（删云端/上传/刷新索引）。
func (sc *Scanner) handleFile(ctx context.Context, batch *UpBatch, fullPath, dbFid string, dbSize int64, fileInfo os.FileInfo) {
	// 类型不符（索引记的是目录）以本地为准：CloudCleanTask 会先把目录下子 strm 移入回收目录再删空目录，
	// 有兜底保护；清完即视为本地新增，直接上传。
	if dbFid != "" && dbSize == index.SizeDir {
		journal.Info(ctx, "本地为文件、云端为同名目录，以本地为准清理云端目录", "路径", fullPath)
		sc.cleanCloud(ctx, fullPath)
		sc.enqueueUpload(ctx, batch, fullPath)
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

// enqueueUpload 入队一个已判定「需上传」的文件：父目录 FID 缺失时按本地路径补建云端目录再取。
// （监听直传可能落在新目录上；全量扫描走 handleDir 已建好父目录，这里是快路径。）
// 文件是否仍存在由 Uploader.DoUpload 上传前再兜一次，此处不重复 stat。
func (sc *Scanner) enqueueUpload(ctx context.Context, batch *UpBatch, fPath string) {
	parentDir := filepath.Dir(fPath)
	parentFid := sc.idx.GetFid(ctx, parentDir)
	if parentFid == "" {
		fid, err := sc.co.AddCloudFolder(ctx, parentDir)
		if err != nil {
			journal.Warn(ctx, "无法获取父目录 FID，跳过", "路径", fPath, "错误", err)
			return
		}
		parentFid = fid
	}
	sc.up.AddUpFile(ctx, batch, parentFid, fPath)
}

// fileInfoEntry 把 os.FileInfo 适配成 os.DirEntry：监听直传只有 stat 结果，没有目录项。
type fileInfoEntry struct {
	name string
	info os.FileInfo
}

func (e fileInfoEntry) Name() string               { return e.name }
func (e fileInfoEntry) IsDir() bool                { return e.info.IsDir() }
func (e fileInfoEntry) Type() os.FileMode          { return e.info.Mode().Type() }
func (e fileInfoEntry) Info() (os.FileInfo, error) { return e.info, nil }

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
