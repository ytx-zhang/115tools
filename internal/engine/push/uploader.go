package push

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/engine/shared"
	"github.com/ytx-zhang/115tools/internal/index"
	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
)

// uploadMu 与 inFlight 为包级：多任务后每个任务一个 Uploader，锁留实例里上传会变成任务间并行、
// 撞 115 风控。包级保证全局串行上传，与 pan 包级限流器同款做法。
var (
	uploadMu sync.Mutex
	inFlight sync.Map // path → struct{}：同文件已在传/排队则跳过
)

// Uploader 上传执行模块（只执行上传，判定归 Scanner.HandleFile / Watcher）。
type Uploader struct {
	api   *pan.Client
	idx   *index.Index
	paths *shared.TaskPaths
	rules shared.Rules
	cache shared.CacheMover
	prog  *shared.Progress
	opts  Opts
}

// Opts 任务级上传选项。
type Opts struct {
	GenStrm  bool // 视频上传后替换为 .strm（关 = 保留原视频，纯云端备份）
	ToCache  bool // 上传后移入透传缓存（关 = 删除原件）
	StrmNow  bool // .strm 事件立即同步
	VideoNow bool // 视频事件立即同步
}

// NewUploader 构造上传模块。
func NewUploader(deps *shared.Deps, prog *shared.Progress, opts Opts) *Uploader {
	return &Uploader{api: deps.Pan, idx: deps.Index, paths: deps.Paths, rules: deps.Rules, cache: deps.Cache, prog: prog, opts: opts}
}

type upJob struct {
	ctx       context.Context
	parentFid string
	path      string
}

// AddUpFile 投递一个已判定「需上传」的任务并计数。batch 非 nil 时入批并上报进度。
func (u *Uploader) AddUpFile(ctx context.Context, batch *sync.WaitGroup, parentFid, fPath string) {
	if _, loaded := inFlight.LoadOrStore(fPath, struct{}{}); loaded {
		return
	}
	quiet := batch == nil
	if !quiet {
		u.prog.AddTotal(1)
	}
	if batch != nil {
		batch.Add(1)
	}
	go func() {
		defer inFlight.Delete(fPath)
		if batch != nil {
			defer batch.Done()
		}
		u.DoUpload(upJob{ctx: ctx, parentFid: parentFid, path: fPath})
		if !quiet {
			u.prog.AddCompleted(1)
		}
	}()
}

// DoUpload 真正执行一次上传（全串行）。
func (u *Uploader) DoUpload(job upJob) {
	uploadMu.Lock()
	defer uploadMu.Unlock()

	fileInfo, err := os.Stat(job.path)
	if err != nil {
		journal.Warn(job.ctx, "同步的文件不存在", "路径", job.path)
		return
	}
	upStart := time.Now()
	if shared.IsStrmPath(job.path) {
		err = u.upStrmTask(job.ctx, job.parentFid, job.path)
	} else {
		err = u.upFileTask(job.ctx, job.parentFid, job.path, fileInfo)
	}
	if err != nil {
		journal.Error(job.ctx, "同步失败", "路径", job.path, "错误", err, "耗时", time.Since(upStart))
	} else {
		journal.Info(job.ctx, "上传文件完成", "路径", job.path, "耗时", time.Since(upStart))
	}
}

// upFileTask 上传普通文件。视频上传完成后按选项替换为 .strm 并移缓存/删原件。
func (u *Uploader) upFileTask(ctx context.Context, parentFid, fPath string, fileInfo os.FileInfo) error {
	info, err := pan.UploadHelper(ctx, u.api, fPath, parentFid, "", "")
	if err != nil {
		return err
	}
	cloudFid := info.Fid
	size := fileInfo.Size()
	savePath := fPath
	ext := filepath.Ext(fPath)

	if u.rules.CheckVideo(ext, size) {
		if !u.opts.GenStrm {
			// 不生成 strm：保留原视频（纯云端备份），按原路径 + 真实 size 记索引。
			u.idx.Put(ctx, savePath, cloudFid, size)
			return nil
		}
		savePath = shared.VideoToStrmPath(fPath)
		// 旧同名 strm 在库 → 旧云端视频移入回收目录（同名视频覆盖 v3）
		if dbFid := u.idx.GetFid(ctx, savePath); dbFid != "" {
			if err := u.api.MoveFile(ctx, dbFid, u.paths.TempFid, savePath); err != nil {
				return fmt.Errorf("[%s]: 清理旧视频失败: %w", savePath, err)
			}
		}
		if err := shared.WriteStrmFile(u.paths.StrmURL, info.PickCode, savePath); err != nil {
			return fmt.Errorf("[%s]: 写入 strm 失败: %w", savePath, err)
		}
		// 索引 size 用写盘后实际 mtime（与 HandleFile 比对口径一致）。
		if st, err := os.Stat(savePath); err == nil {
			size = st.ModTime().Unix()
		}
		// 原视频：ToCache 开启且缓存可用 → 移入缓存；否则删除原件（本地不残留与云端 strm 并存的双份）。
		if u.opts.ToCache && u.cache != nil {
			if _, err := u.cache.Move(fPath, info.PickCode); err != nil {
				if rerr := os.Remove(fPath); rerr != nil && !os.IsNotExist(rerr) {
					return fmt.Errorf("[%s]: 移入缓存失败且删除原件失败: %w", fPath, err)
				}
				journal.Warn(ctx, "视频移入缓存失败，已删除原件", "路径", fPath, "错误", err)
			}
		} else if err := os.Remove(fPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("[%s]: 删除视频失败: %w", fPath, err)
		}
	}

	u.idx.Put(ctx, savePath, cloudFid, size)
	return nil
}

// upStrmTask 处理本地新增的 .strm：按 pickcode 定位云端视频 → 移入目标目录 → 改回原名 → 重写本地 strm。
func (u *Uploader) upStrmTask(ctx context.Context, parentFid, fPath string) error {
	pickcode, fid := shared.ParseStrmFile(fPath)
	if pickcode == "" {
		return fmt.Errorf("[%s]: 无 pickcode", fPath)
	}
	if fid == "" {
		var err error
		if fid, err = pan.PickcodeToID(pickcode); err != nil {
			return fmt.Errorf("[%s]: 获取 fid 失败: %w", fPath, err)
		}
	}

	if err := u.api.MoveFile(ctx, fid, parentFid, fPath); err != nil {
		return fmt.Errorf("[%s]: 移动云端视频失败: %w", fPath, err)
	}
	origName := strings.TrimSuffix(filepath.Base(fPath), ".strm")
	newName, err := u.api.RenameFile(ctx, fid, origName, fPath)
	if err != nil {
		return fmt.Errorf("[%s]: 云端改名失败: %w", fPath, err)
	}
	if newName != origName+filepath.Ext(newName) {
		if _, err := u.api.RenameFile(ctx, fid, origName+filepath.Ext(newName), fPath); err != nil {
			return fmt.Errorf("[%s]: 云端扩展名修复失败: %w", fPath, err)
		}
	}
	if err := shared.WriteStrmFile(u.paths.StrmURL, pickcode, fPath); err != nil {
		return fmt.Errorf("[%s]: 文件写入失败: %w", fPath, err)
	}
	size := time.Now().Unix()
	if st, err := os.Stat(fPath); err == nil {
		size = st.ModTime().Unix()
	}
	u.idx.Put(ctx, fPath, fid, size)
	return nil
}
