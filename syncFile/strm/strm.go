// Package strm 是「STRM 生成」模块：扫描云端媒体库目录（StrmPath），
// 为其中的视频在本地生成 .strm 索引文件（非视频文件则真实下载）。
//
// 【与 strmServer 包的区别】
//   - 本包（syncFile/strm）：批量「生成」.strm 索引文件的任务；
//   - strmServer 包：/download 接口，播放时把 .strm 里的 URL 实时 302 到 115 直链。
//
// 一个是「造索引」，一个是「用索引」，职责完全不同。
//
// 【执行流程】
//  1. 从 StrmPath 出发用 core.WalkCloud 遍历整棵目录树；
//  2. 本地已存在的 .strm 跳过，缺失的逐个生成；
//  3. 遍历期间把「顶层目录下的文件/目录 FID」收集到 moveFids；
//  4. 任务结束且零失败时，把收集到的原始文件统一移入回收目录（TempPath）——
//     因为索引已生成，云端原件留在原位会被 Emby 等媒体服务器重复扫到。
//
// 【触发方式】web 面板手动触发（POST /api/task/strm），Stop 手动停止；
// Start 自带防重入。
package strm

import (
	"115tools/syncFile/core"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Strm 是 STRM 生成模块的实例。
// 与 cloud 模块一样无后台常驻协程——每次 Start 就是一轮完整任务。
type Strm struct {
	env    *core.Env               // 共享运行环境（API/DB/路径配置）
	stats  core.TaskStats          // 任务进度统计，驱动前端进度条
	cancel context.CancelCauseFunc // 取消本轮任务；Stop 时以「用户请求」为原因调用

	moveFidsMu sync.Mutex // 保护 moveFids 的并发追加（遍历是多协程的）
	moveFids   []string   // 顶层目录下各项的云端 FID，任务成功后统一移入回收目录
}

// New 创建 STRM 生成模块实例。
// notify 是状态变更通知通道，由 Runner 创建并跨热重载共享（见 core.TaskStats）。
// 调用方：syncFile 根包的 New()。
func New(env *core.Env, notify chan struct{}) *Strm {
	return &Strm{
		env:   env,
		stats: core.NewTaskStats(notify),
	}
}

// StatusJSON 返回任务进度的 JSON 快照，供根包拼装面板状态。
func (s *Strm) StatusJSON() string {
	return s.stats.GetStatus()
}

// Start 启动一轮 STRM 生成任务（在调用方的协程中运行，通常由 web 层异步触发）。
// 同一时刻只允许一轮任务：重复触发直接返回。
func (s *Strm) Start(parentCtx context.Context) {
	if !s.stats.TryStart() {
		return // 已有一轮在跑，忽略本次触发
	}
	start := time.Now()
	ctx, cancel := context.WithCancelCause(parentCtx)
	s.cancel = cancel

	defer func() {
		s.stats.SetRunning(false)
		s.cancel(nil)
		slog.Info("生成strm任务结束", "总数", s.stats.Total(), "耗时", time.Since(start))
	}()

	s.stats.Reset()
	// 清空上一轮收集的 FID 列表（[:0] 复用底层数组、长度归零；
	// 注意不能用 clear()——它只把元素置零、长度不变，append 会接在旧元素后面）。
	s.moveFids = s.moveFids[:0]
	slog.Debug("开始生成strm文件...")

	// appendMoveFid 登记「顶层目录下的项」（遍历是多协程的，append 需加锁）。
	// 只有直接挂在 StrmPath 下的项才登记——它们生成索引后要从云端原位挪走；
	// 更深层的内容保持原目录结构不动。
	appendMoveFid := func(path, fid string) {
		if filepath.Dir(path) == s.env.Paths.StrmPath {
			s.moveFidsMu.Lock()
			s.moveFids = append(s.moveFids, fid)
			s.moveFidsMu.Unlock()
		}
	}

	info, err := s.env.API.GetDirInfo(ctx, s.env.Paths.StrmPath)
	if err != nil {
		slog.Error("无法获取起始目录id", "错误信息", err)
		return
	}

	// 遍历云端媒体库。致命错误已由回调内的 FailLog 逐条记录，返回值显式忽略。
	_ = s.env.WalkCloud(ctx, s.env.Paths.StrmPath, info.Fid, core.Visitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			appendMoveFid(path, fid)
			if err := os.MkdirAll(path, 0755); err != nil {
				core.FailLog(&s.stats, path, "创建目录失败", err)
				return false, nil
			}
			return true, nil
		},
		VisitFile: func(ctx context.Context, path, fid, pickCode string, e core.Entry) error {
			appendMoveFid(path, fid)
			savePath, _ := core.ProcessCloudFile(path, e)
			if _, err := os.Stat(savePath); err == nil {
				return nil // 本地已有 .strm/文件，跳过
			}
			s.stats.AddTotal(1)
			if err := s.env.FetchAndSave(ctx, pickCode, fid, savePath, e.IsVideo, &s.stats); err != nil {
				return nil
			}
			s.stats.AddCompleted(1)
			return nil
		},
	}, nil)

	if err := context.Cause(ctx); err != nil {
		slog.Error("生成strm任务被取消", "取消信息", err)
		return
	}
	// 零失败才把原始文件移入回收目录：任一文件生成失败时保持云端原状，
	// 避免「索引没生成好、原件又被挪走」的双重损失。
	if s.stats.Failed() == 0 && len(s.moveFids) > 0 {
		s.moveStrmPathFiles(ctx, s.moveFids)
	}
}

// Stop 停止正在运行的本轮任务（面板「停止」按钮）。
// 无任务在跑时安全地什么都不做。
func (s *Strm) Stop() {
	if s.stats.Running() && s.cancel != nil {
		s.cancel(fmt.Errorf("用户请求停止任务"))
	}
}

// moveStrmPathFiles 把收集到的顶层 FID 批量移入云端回收目录（TempPath）。
func (s *Strm) moveStrmPathFiles(ctx context.Context, paths []string) {
	fidsStr := strings.Join(paths, ",")
	count := len(paths)
	if err := s.env.API.MoveFile(ctx, fidsStr, s.env.Paths.TempFid); err != nil {
		core.FailLog(&s.stats, "TempPath", "移动文件至 TempPath 失败", err)
	} else {
		slog.Info("移动文件至 TempPath", "文件数量", count, "路径", fidsStr)
	}
}
