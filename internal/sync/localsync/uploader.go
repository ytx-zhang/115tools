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

// NewUploader 构造 uploader 小模块（依赖注入）。
func NewUploader(deps *common.Core, task *common.Task) *Uploader {
	return &Uploader{
		api:   deps.API,
		db:    deps.DB,
		paths: deps.Paths,
		rules: deps.Rules,
		task:  task,
	}
}

// Uploader 上传执行模块（只做"执行上传"，不含"该不该上传"判定——判定归 scanner.HandleFile / watch 复用）。
// 每次 AddUpFile 起一个 goroutine 跑 DoUpload，进度计数实时上报前端；uploadMu 保证全串行。
// 批次等待（覆盖「扫描+本批上传完」）由调用方显式传 *sync.WaitGroup：目录消费者新建 wg 透传，
// 视频直传传独立 wg 或 nil（不入批、不阻塞扫描消费者）。
type Uploader struct {
	api   *drive.Client
	db    *store.Store
	paths *common.Paths
	rules common.Rules
	task  *common.Task // 本地同步进度（AddUpFile +total，任务完成 +completed）

	uploadMu sync.Mutex // 全串行：任何来源的上传绝对不并发
	inFlight sync.Map   // path → struct{}：同文件已在传/排队则跳过（防双传）
}

// upJob 一个待上传任务。
type upJob struct {
	ctx       context.Context
	parentFid string
	path      string
}

// AddUpFile 投递一个已判定「需上传」的任务并计数。
// batch 非 nil 时本任务计入批次 wg（+1/-1）并上报本地任务进度（total/completed）；
// nil 用于视频直传等静默增量：不入批、不等待、不进 task 进度（web 进度卡不跳）。
// 进度条 Reset 统一由消费循环每批开头负责，本函数不 Reset——避免视频直传误清零进度条。
func (u *Uploader) AddUpFile(ctx context.Context, batch *sync.WaitGroup, parentFid, fPath string) {
	// 去重：同文件已在传/排队则直接跳过（不重复投递、不重复计数）。
	if _, loaded := u.inFlight.LoadOrStore(fPath, struct{}{}); loaded {
		return
	}
	quiet := batch == nil // 视频直传等静默增量：不进本地任务进度（不触发 SSE 状态帧）
	if !quiet {
		u.task.AddTotal(1)
	}
	if batch != nil {
		batch.Add(1)
	}
	go func() {
		defer u.inFlight.Delete(fPath)
		if batch != nil {
			defer batch.Done()
		}
		u.DoUpload(upJob{ctx: ctx, parentFid: parentFid, path: fPath})
		if !quiet {
			u.task.AddCompleted(1)
		}
	}()
}

// DoUpload 真正执行一次上传（由 AddUpFile 起 goroutine 调用）。
// 开头持锁（有人在上传则阻塞排队），结尾释放。上传前大小一致性兜底由 drive 层负责，本层不重复检查。
func (u *Uploader) DoUpload(job upJob) {
	u.uploadMu.Lock()
	defer u.uploadMu.Unlock()

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

// upFileTask 上传普通文件。视频（CheckVideo 命中）上传完成后本地原件删除、
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
			if err := u.api.MoveFile(ctx, dbFid, u.paths.TempFid, savePath); err != nil {
				return fmt.Errorf("[%s]: 清理旧视频失败: %w", savePath, err)
			}
		}
		if err := common.WriteStrmFile(u.paths.StrmUrl, info.PickCode, savePath); err != nil {
			return fmt.Errorf("[%s]: 写入strm文件失败: %w", savePath, err)
		}
		// DB 记录 size 用写盘后文件实际 mtime（与 HandleFile 比对口径一致），避免跨秒误判重传。
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

// upStrmTask 处理本地新增的 .strm：按 pickcode 找到云端视频 → 移动进目标目录 →
// 改回原名 → 重写本地 .strm。
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

	if err := common.WriteStrmFile(u.paths.StrmUrl, pickcode, fPath); err != nil {
		return fmt.Errorf("[%s]: 文件写入失败: %w", fPath, err)
	}
	// 用写盘后文件实际 mtime 作为 size（与 HandleFile 比对口径一致）。
	size := time.Now().Unix()
	if st, err := os.Stat(fPath); err == nil {
		size = st.ModTime().Unix()
	}
	u.db.SaveRecord(fPath, fid, size)
	return nil
}
