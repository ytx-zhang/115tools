package localsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// UploadWorkerCount 上传并发数（仅上传模块使用，不放进 common）。
// 全串行模型：sem=1，任何来源（扫描目录/watcher 视频直传）的上传绝对不并发。
const UploadWorkerCount = 1

// NewUploader 构造 uploader 小模块（依赖注入）。
func NewUploader(deps *common.SyncDeps, task *common.Task) *Uploader {
	return &Uploader{
		api:   deps.API,
		db:    deps.DB,
		paths: deps.Paths,
		rules: deps.Rules,
		task:  task,
		sem:   make(chan struct{}, UploadWorkerCount),
	}
}

// Uploader 上传执行小模块（只做"执行上传"，不含"该不该上传"的判定）。
// 依赖：api（云端上传/移动/改名）、db（记录写入/旧视频清理）、paths（回收 FID、strm URL）、
// rules（视频判定，用于 upFileTask 是否转 .strm）。
// .strm 索引文件经 common.WriteStrmFile 写入，本模块不再依赖 cloudsync.StrmIO。
// 「文件该不该上传」的判定归扫描模块（scanner.HandleFile / watch 复用），本模块仅执行。
// 每次 AddUpFile 直接起一个 goroutine 跑 DoUpload，进度计数（task）实时上报前端 local 卡片。
// 并发上限由 sem（容量 = uploadWorkerCount）令牌控制：DoUpload 开头取令牌、结尾归还。
//
// 批次等待（让「运行中」覆盖「扫描 + 本批上传完」）由调用方显式传入 *sync.WaitGroup 实现：
// 目录消费者（scanner.ConsumeLoop）处理一个目录时新建 wg，透传给 AddUpFile，处理完 wg.Wait()。
// 视频直传也调同一函数、传独立 wg（或 nil 不入批），不阻塞扫描消费者。
// 不再用 ctx 携带 wg、也不再于投递处判断 ctx.Err —— 取消由整体 ctx 传给 drive 层中断。
type Uploader struct {
	api   *drive.Client
	db    *store.Store
	paths *common.Paths
	rules common.Rules
	task  *common.Task // 本地同步进度目标（AddUpFile +total，任务完成 +completed）

	sem      chan struct{} // 并发令牌，容量 = uploadWorkerCount
	inFlight sync.Map      // path → struct{}，同一文件已在传/排队则跳过（防双传）
}

// upJob 一个待上传任务。
type upJob struct {
	ctx       context.Context
	parentFid string
	path      string
}

// AddUpFile 投递一个已判定「需上传」的任务并计数；直接起 goroutine 异步执行（并发上限由 sem 令牌控制）。
// ⚠️ 调用方（scanner/watch）负责「该不该上传」的判定与存在性复查；
// 此函数只做投递与进度计数，保持执行器精简。所有上传路径（扫描目录/watcher 视频直传）统一走此入口。
// batch 为本次目录任务的批次 WaitGroup（nil 表示不入批、不等待，用于视频直传等静默增量）；
// 非 nil 时本任务 +1、完成 -1，消费者在目录处理末尾 wg.Wait() 即覆盖「扫描 + 本批上传完」。
// 进度条的 Reset 统一由消费者（dirpool.ConsumeLoop 每批开头）负责，本函数不再按需 Reset——
// 避免视频直传（batch=nil）静默增量时误把进度条清零。
func (u *Uploader) AddUpFile(ctx context.Context, batch *sync.WaitGroup, parentFid, fPath string) {
	// 去重：同一文件已在传/排队则直接跳过（不重复投递、不重复计数）。
	if _, loaded := u.inFlight.LoadOrStore(fPath, struct{}{}); loaded {
		return
	}
	u.task.AddTotal(1)
	if batch != nil {
		batch.Add(1)
	}
	go func() {
		defer u.inFlight.Delete(fPath)
		if batch != nil {
			defer batch.Done()
		}
		u.DoUpload(upJob{ctx: ctx, parentFid: parentFid, path: fPath})
		u.task.AddCompleted(1)
	}()
}

// DoUpload 真正执行一次上传（由 AddUpFile 起 goroutine 调用）。
// 入队前调用方已判定「需上传」，此处只复查文件是否仍在（排队等待期间可能被删/移动）。
// 开头取并发令牌（满则阻塞排队），结尾归还，自然限制同时只有 uploadWorkerCount 个在跑。
// 上传前的大小一致性兜底由 drive 层（uploadByOSS 的 f.Stat 比对）负责，本层不重复检查。
// ctx 仅透传 drive 层用于停止时中断上传，不在本层做取消早退判断。
func (u *Uploader) DoUpload(job upJob) {
	// 取并发令牌：满了就在这排队，不占用额外资源。
	u.sem <- struct{}{}
	defer func() { <-u.sem }()

	fileInfo, err := os.Stat(job.path)
	if err != nil {
		logs.Warn(logs.ModuleSync, "同步的文件不存在", "路径", job.path)
		return
	}

	upStart := time.Now()
	if common.IsStrmPath(job.path) {
		err = u.upStrmTask(job.ctx, job.parentFid, job.path)
	} else {
		err = u.upFileTask(job.ctx, job.parentFid, job.path, fileInfo)
	}
	if err != nil {
		logs.Error(logs.ModuleSync, "同步失败", "路径", job.path, "错误", err, "耗时", time.Since(upStart))
	} else {
		logs.Info(logs.ModuleSync, "上传文件完成", "路径", job.path, "耗时", time.Since(upStart))
	}
}

// ──── 两种上传任务 ────

// upFileTask 上传普通文件。视频（CheckVideo 命中）上传完成后本地原件被删除，
// 原地替换为 .strm 索引文件——「本地不存视频、Emby 照常播放」的核心机制。
func (u *Uploader) upFileTask(ctx context.Context, parentFid, fPath string, fileInfo os.FileInfo) error {
	info, err := drive.UploadHelper(ctx, u.api, fPath, parentFid, "", "")
	if err != nil {
		return err
	}
	cloudFid := info.Fid
	size := fileInfo.Size()
	savePath := fPath
	ext := filepath.Ext(fPath)
	if u.rules.CheckVideo(ext, size) {
		savePath = common.VideoToStrmPath(fPath)
		if dbFid := u.db.GetFid(savePath); dbFid != "" {
			if err := u.api.MoveFile(ctx, dbFid, u.paths.TempFid); err != nil {
				return fmt.Errorf("[%s]: 清理旧视频失败: %w", savePath, err)
			}
		}
		if err := common.WriteStrmFile(u.paths.StrmUrl, info.PickCode, savePath); err != nil {
			return fmt.Errorf("[%s]: 写入strm文件失败: %w", savePath, err)
		}
		// DB 记录 .strm 的 size 用写盘后文件实际 mtime（与 HandleFile 的 mtime 比对口径一致），
		// 避免 time.Now() 与落盘跨秒导致下次扫描误判修改 → 删云端重传震荡。
		if st, err := os.Stat(savePath); err == nil {
			size = st.ModTime().Unix()
		}
		if err := os.Remove(fPath); err != nil {
			return fmt.Errorf("[%s]: 删除视频文件失败: %w", fPath, err)
		}
	}
	u.db.SaveRecord(savePath, cloudFid, size)
	return nil
}

// upStrmTask 处理本地新增的 .strm 文件：按 pickcode 找到云端已有视频 →
// 移动到目标目录 → 改回原名 → 重写本地 .strm。
func (u *Uploader) upStrmTask(ctx context.Context, parentFid, fPath string) error {
	pickcode, fid := common.ParseStrmFile(fPath)
	if pickcode == "" {
		return fmt.Errorf("[%s]: 无pickcode", fPath)
	}
	if fid == "" {
		var err error
		if fid, err = drive.PickcodeToID(pickcode); err != nil {
			return fmt.Errorf("[%s]: 获取fid失败: %w", fPath, err)
		}
	}

	if err := u.api.MoveFile(ctx, fid, parentFid); err != nil {
		return fmt.Errorf("[%s]: 移动云端视频失败: %w", fPath, err)
	}

	origName := strings.TrimSuffix(filepath.Base(fPath), ".strm")
	newName, err := u.api.RenameFile(ctx, fid, origName)
	if err != nil {
		return fmt.Errorf("[%s]: 云端改名失败: %w", fPath, err)
	}
	if newName != origName+filepath.Ext(newName) {
		if _, err := u.api.RenameFile(ctx, fid, origName+filepath.Ext(newName)); err != nil {
			return fmt.Errorf("[%s]: 云端扩展名修复失败: %w", fPath, err)
		}
	}

	if err := common.WriteStrmFile(u.paths.StrmUrl, pickcode, fPath); err != nil {
		return fmt.Errorf("[%s]: 文件写入失败: %w", fPath, err)
	}
	// 用写盘后文件实际 mtime 作为 size（与 HandleFile 的 mtime 比对口径一致）。
	size := time.Now().Unix()
	if st, err := os.Stat(fPath); err == nil {
		size = st.ModTime().Unix()
	}
	u.db.SaveRecord(fPath, fid, size)
	return nil
}
