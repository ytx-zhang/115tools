package mirror

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/store"
)

// Applier 执行动作清单。这是全项目唯一修改云端、本地文件与索引的地方。
type Applier struct {
	api   *drive.Client
	store *store.Store
	paths Paths
	rules Rules
	cache CacheMover
	prog  Progress
}

// NewApplier 构造执行器。参数显式列出，不用依赖包（一眼能看出执行到底需要什么）。
func NewApplier(api *drive.Client, st *store.Store, paths Paths, rules Rules, cache CacheMover, prog Progress) *Applier {
	if prog == nil {
		prog = NopProgress{}
	}
	return &Applier{api: api, store: st, paths: paths, rules: rules, cache: cache, prog: prog}
}

// Apply 按阶段顺序执行动作清单，统计写入 stats。
//
// 阶段次序（清场 → 建目录 → 上传/接管 → 下载/修正 → 归档）由 OpKind.phase() 决定，
// 同类内保持 plan 的产出顺序（稳定排序），保证父目录先于子目录创建。
//
// 单个动作失败只记日志与计数，不中断整批——一次扫描里个别文件失败不该拖垮其余文件；
// 只有 ctx 取消才会整体中止。
func (a *Applier) Apply(ctx context.Context, ops []Op, cfg LocalCfg, stats *store.Stats) error {
	ordered := slices.Clone(ops)
	slices.SortStableFunc(ordered, func(x, y Op) int { return x.Kind.phase() - y.Kind.phase() })

	a.prog.Reset(countWork(ordered))

	// 归档动作留到最后一次性批量执行（一次 API 调用搬走全部顶层项）
	var archiveFids []string
	for _, op := range ordered {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if op.Kind == OpArchive {
			if op.Fid != "" {
				archiveFids = append(archiveFids, op.Fid)
			}
			continue
		}
		a.prog.SetCurrent(op.Path)
		a.applyOne(ctx, op, cfg, stats)
	}
	return a.archiveAll(ctx, archiveFids, stats)
}

// applyOne 执行单个动作、累加统计并推进进度。
func (a *Applier) applyOne(ctx context.Context, op Op, cfg LocalCfg, stats *store.Stats) {
	var err error
	switch op.Kind {
	case OpMkdir:
		err = a.mkdir(ctx, op.Path)
	case OpUpload:
		err = a.upload(ctx, op, cfg, stats)
	case OpAdopt:
		err = a.adopt(ctx, op, stats)
	case OpDownload:
		err = a.download(ctx, op, stats)
	case OpRetire:
		err = a.retire(ctx, op, stats)
	case OpDrop:
		err = a.drop(ctx, op, stats)
	case OpNormalize:
		err = a.normalize(op)
	default:
		return
	}
	if err != nil {
		stats.Failed++
		slog.ErrorContext(ctx, "执行动作失败", "动作", op.Kind.Label(), "路径", op.Path, "错误", err)
		return
	}
	if op.Kind == OpUpload || op.Kind == OpAdopt || op.Kind == OpDownload {
		a.prog.Advance()
	}
}

// ──── 本地 → 云端 ────

// mkdir 确保本地目录对应的云端目录存在（逐级建并写索引）。
func (a *Applier) mkdir(ctx context.Context, localPath string) error {
	_, err := EnsureLocalDir(ctx, a.api, a.store, a.paths, localPath)
	return err
}

// upload 上传一个本地文件。视频且开启 STRM 时，上传后写 .strm 并处理原件（移缓存或删除）。
func (a *Applier) upload(ctx context.Context, op Op, cfg LocalCfg, stats *store.Stats) error {
	info, err := os.Stat(op.Path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.DebugContext(ctx, "待上传文件已不存在，跳过", "路径", op.Path)
			return nil
		}
		return err
	}
	size := info.Size()

	parentFid, err := EnsureLocalDir(ctx, a.api, a.store, a.paths, filepath.Dir(op.Path))
	if err != nil {
		return err
	}

	start := time.Now()
	up, err := drive.UploadHelper(ctx, a.api, op.Path, parentFid, "", "")
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "上传完成", "路径", op.Path, "耗时", time.Since(start))
	stats.Uploaded++

	// 「要不要当视频处理」由 plan 在判定时决定（op.IsVideo 综合了扩展名、体积阈值与 ToStrm 开关），
	// apply 只执行，不重新判定——否则执行期间文件变化会让实际行为偏离预演结果。
	if !op.IsVideo {
		a.store.Put(ctx, op.Path, store.Record{Fid: up.Fid, Kind: store.KindFile, Size: size})
		return nil
	}
	return a.replaceWithStrm(ctx, op, up.Fid, up.PickCode, cfg, stats)
}

// replaceWithStrm 视频上传后的收尾：归档旧云端视频（仅当确实产生了不同文件）、
// 写 .strm、处理本地原件（移入缓存或删除）、记索引。
func (a *Applier) replaceWithStrm(ctx context.Context, op Op, fid, pickCode string, cfg LocalCfg, stats *store.Stats) error {
	strmPath := VideoToStrmPath(op.Path)

	// 秒传会复用同一 FID（内容完全一致），此时不该把同一份文件误移进回收目录
	if op.ReplaceFid != "" && op.ReplaceFid != fid {
		if err := Retire(ctx, a.api, a.paths, []string{op.ReplaceFid}, strmPath); err != nil {
			return fmt.Errorf("归档旧视频失败: %w", err)
		}
		stats.Deleted++
	}

	if err := WriteStrmFile(a.paths.StrmURL, pickCode, strmPath); err != nil {
		return fmt.Errorf("写入 STRM 失败: %w", err)
	}
	stats.StrmGenerated++

	a.removeOriginal(ctx, op.Path, pickCode, cfg)
	a.store.Put(ctx, strmPath, store.Record{Fid: fid, PickCode: pickCode, Kind: store.KindStrm})
	return nil
}

// removeOriginal 处理本地原件：开启缓存则移入缓存目录，否则删除（本地不留与云端 strm 并存的双份）。
func (a *Applier) removeOriginal(ctx context.Context, path, pickCode string, cfg LocalCfg) {
	if cfg.ToCache && a.cache != nil {
		if _, err := a.cache.Move(path, pickCode); err == nil {
			return
		} else if !os.IsNotExist(err) {
			slog.WarnContext(ctx, "视频移入缓存失败，改为删除原件", "路径", path, "错误", err)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.WarnContext(ctx, "删除本地原件失败", "路径", path, "错误", err)
	}
}

// adopt 接管本地新增的 .strm：把 pickcode 指向的云端视频移入目标目录、改回原名、重写本地 strm。
func (a *Applier) adopt(ctx context.Context, op Op, stats *store.Stats) error {
	fid := op.Fid
	if fid == "" {
		var err error
		if fid, err = drive.PickcodeToID(op.PickCode); err != nil {
			return fmt.Errorf("解析 pickcode 失败: %w", err)
		}
	}

	parentFid, err := EnsureLocalDir(ctx, a.api, a.store, a.paths, filepath.Dir(op.Path))
	if err != nil {
		return err
	}
	if err := a.api.MoveFile(ctx, fid, parentFid, op.Path); err != nil {
		return fmt.Errorf("移动云端视频失败: %w", err)
	}

	origName := strings.TrimSuffix(filepath.Base(op.Path), ".strm")
	newName, err := a.api.RenameFile(ctx, fid, origName, op.Path)
	if err != nil {
		return fmt.Errorf("云端改名失败: %w", err)
	}
	// 115 的 rename 可能自行补上扩展名，导致与本地 strm 名不一致，需要再修一次
	if ext := filepath.Ext(newName); ext != "" && newName != origName+ext {
		if _, err := a.api.RenameFile(ctx, fid, origName+ext, op.Path); err != nil {
			return fmt.Errorf("云端扩展名修复失败: %w", err)
		}
	}

	if err := WriteStrmFile(a.paths.StrmURL, op.PickCode, op.Path); err != nil {
		return fmt.Errorf("重写 STRM 失败: %w", err)
	}
	a.store.Put(ctx, op.Path, store.Record{Fid: fid, PickCode: op.PickCode, Kind: store.KindStrm})
	stats.StrmGenerated++
	return nil
}

// normalize 修正本地 .strm 的直链格式（pickcode 未变，只是链接格式过时）。
// 纯本地文件读写、无取消语义，故不需要 ctx。
func (a *Applier) normalize(op Op) error {
	pc := ParseStrmFile(op.Path)
	if pc == "" {
		return nil
	}
	return WriteStrmFile(a.paths.StrmURL, pc, op.Path)
}

// ──── 云端 → 本地 ────

// download 把云端文件落地到本地：视频按开关写 .strm，其余下载实体文件。
func (a *Applier) download(ctx context.Context, op Op, stats *store.Stats) error {
	if err := os.MkdirAll(filepath.Dir(op.Path), 0o755); err != nil {
		return err
	}
	if op.IsVideo {
		if err := WriteStrmFile(a.paths.StrmURL, op.PickCode, op.Path); err != nil {
			return err
		}
		stats.StrmGenerated++
	} else {
		if err := DownloadCloudFile(ctx, a.api, op.PickCode, op.Path); err != nil {
			return err
		}
		stats.Downloaded++
	}

	var size int64
	if st, err := os.Stat(op.Path); err == nil {
		size = st.Size()
	}
	rec := store.Record{Fid: op.Fid, Kind: store.KindFile, Size: size}
	if op.IsVideo {
		rec = store.Record{Fid: op.Fid, PickCode: op.PickCode, Kind: store.KindStrm}
	}
	a.store.Put(ctx, op.Path, rec)
	return nil
}

// ──── 云端清理 ────

// retire 把 .strm 对应的云端视频移入回收目录（可找回）并清索引。
func (a *Applier) retire(ctx context.Context, op Op, stats *store.Stats) error {
	fids := []string{op.Fid}
	if op.Fid == "" {
		var move []string
		move, _ = CollectStrmFids(ctx, a.store, op.Path)
		fids = move
	}
	if len(fids) == 0 {
		return nil
	}
	if err := Retire(ctx, a.api, a.paths, fids, op.Path); err != nil {
		return err
	}
	stats.Deleted += int64(len(fids))
	a.store.ClearTree(ctx, op.Path)
	return nil
}

// drop 删除云端文件或目录：目录会先把子 .strm 对应的视频移入回收目录，再删空目录。
func (a *Applier) drop(ctx context.Context, op Op, stats *store.Stats) error {
	if op.Fid == "" {
		move, del := CollectStrmFids(ctx, a.store, op.Path)
		if len(move) == 0 && len(del) == 0 {
			return nil
		}
		if err := Retire(ctx, a.api, a.paths, move, op.Path); err != nil {
			return err
		}
		if err := Drop(ctx, a.api, del, op.Path); err != nil {
			return err
		}
		stats.Deleted += int64(len(move) + len(del))
		a.store.ClearTree(ctx, op.Path)
		return nil
	}
	if err := Drop(ctx, a.api, []string{op.Fid}, op.Path); err != nil {
		return err
	}
	stats.Deleted++
	a.store.ClearTree(ctx, op.Path)
	return nil
}

// archiveAll 把全部顶层项一次性批量移入回收目录（下载作用域的收尾动作）。
func (a *Applier) archiveAll(ctx context.Context, fids []string, stats *store.Stats) error {
	if len(fids) == 0 {
		return nil
	}
	start := time.Now()
	if err := Retire(ctx, a.api, a.paths, fids, a.paths.CloudDir); err != nil {
		return err
	}
	stats.Deleted += int64(len(fids))
	slog.InfoContext(ctx, "归档到回收目录", "数量", len(fids), "耗时", time.Since(start))
	return nil
}

// ──── 辅助 ────

// countWork 统计需要做实际传输的动作数（供进度条 total 在计划阶段一次性确定，消除边扫边涨的抖动）。
func countWork(ops []Op) int64 {
	var n int64
	for _, op := range ops {
		if op.Kind == OpUpload || op.Kind == OpAdopt || op.Kind == OpDownload {
			n++
		}
	}
	return n
}

// DownloadCloudFile 用 pickcode 换取直链并把文件完整下载到 localPath。
func DownloadCloudFile(ctx context.Context, api *drive.Client, pickCode, localPath string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	info, err := api.GetDownloadURL(ctx, pickCode, "115tools")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "115tools")

	resp, err := drive.HTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // 只读响应体，关闭失败无补救动作
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }() //nolint:errcheck // 关闭失败无补救动作
	if _, err := io.Copy(out, resp.Body); err != nil {
		if rerr := os.Remove(localPath); rerr != nil && !os.IsNotExist(rerr) {
			slog.DebugContext(ctx, "清理下载失败残留失败", "路径", localPath, "错误", rerr)
		}
		return err
	}
	return nil
}
