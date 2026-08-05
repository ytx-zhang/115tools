// Package sync 是 115 网盘双向同步核心：本地新增文件 → 上传云端并原地转 .strm，
// 云端目录结构 → 反向同步到本地；另含定时全量扫描兜底与 STRM 索引重建。
//
// 数据流（单次运行态 instance）：
//
//	watchPump ──(文件事件)──▶ 单全局防抖计时器 ──▶ scanDir ──▶ uploadJobs ──▶ worker
//	                                                                          │
//	                                                                          ▼
//	                          Task(cloudTask) ──▶ runCloudSync ──▶ WalkCloud ──▶ 下载/清理
//	                          Task(strmTask)  ──▶ runStrmGen  ──▶ WalkCloud ──▶ 生成.strm
//	                          cronSync 周期触发 FullScan + StartCloudSync 兜底
//
// 对外只暴露 Syncer（生命周期 + 热重载 + 对 web 的全部方法），同包内直接操作
// instance 的 cloudTask/strmTask/env，无额外门面转发层。
package sync

import (
	"context"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/logs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// instance 是单次运行的同步实例：持有运行环境、上传队列、并发去重与两个一次性任务。
type instance struct {
	env        *Env
	uploadJobs chan uploadJob // 上传任务队列，常驻 worker 消费
	inFlight   sync.Map       // 并发上传去重（key=本地路径），防同名重复上传
	cloudTask  *Task          // 云端→本地全量同步任务
	strmTask   *Task          // STRM 索引生成任务
}

func newInstance(env *Env, onChange func()) *instance {
	return &instance{
		env:        env,
		uploadJobs: make(chan uploadJob, 64),
		cloudTask:  NewTask("云端同步", onChange),
		strmTask:   NewTask("STRM生成", onChange),
	}
}

// Start 启动后台协程（都挂 wg 上，随 ctx 取消退出）。
// ⚠️ worker 必须先于 FullScan 启动（v0.8.2 死锁坑）；
// ⚠️ FullScan 必须异步（v0.8.7：同步会阻塞 New() 返回、前端卡「重载中」）。
func (l *instance) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Go(func() { l.startUploadWorkers(ctx, uploadWorkerCount) })
	wg.Go(func() { l.watchPump(ctx) })
	wg.Go(func() { l.FullScan(ctx) })
}

// Status 返回本实例两个任务的进度快照，供 Syncer 聚合进 StatusView。
func (l *instance) Status() (cloud, strm *TaskProgress) {
	return l.cloudTask.Status(), l.strmTask.Status()
}

// RescanRoot 异步触发一次非递归本地扫描（仅直属子项），用于上传排除名单变更后联动。
func (l *instance) RescanRoot(ctx context.Context) {
	go l.syncDir(ctx, l.env.Paths.SyncPath, l.env.Paths.SyncFid, false)
}

// RegenerateStrmFiles 重写两棵本地同步树（SyncPath+StrmPath）下的 .strm 索引，
// 纯本地 IO，ExtractPickcode 反向解析旧的 pickcode/fid。
func (l *instance) RegenerateStrmFiles(ctx context.Context) {
	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		regenerateStrmTree(ctx, l.env, l.env.Paths.SyncPath)
	}()
	go func() {
		defer wg.Done()
		regenerateStrmTree(ctx, l.env, l.env.Paths.StrmPath)
	}()
	wg.Wait()
}

// initRoot 建立主同步目录的数据库索引（以云端 FID 为准）。FID 一致复用索引；
// 不一致或首次运行则全量扫描；扫描被中止时清理半成品索引。
func (l *instance) initRoot(parentCtx context.Context, oldSyncPath string) error {
	if err := context.Cause(parentCtx); err != nil {
		logs.Warn(logs.ModuleSync, "初始化同步", "错误信息", err)
		return err
	}
	ctx, cancel := context.WithCancelCause(parentCtx)
	defer cancel(nil)
	stopWithErr := func(err error) { cancel(err) }

	info, err := l.env.API.GetDirInfo(ctx, l.env.Paths.SyncPath)
	if err != nil {
		return err
	}
	cloudFid := info.Fid
	l.env.Paths.SyncFid = cloudFid

	dbFid := l.env.DB.GetFid(l.env.Paths.SyncPath)
	if dbFid != "" && dbFid == cloudFid {
		return nil // 复用索引
	}

	if dbFid == "" {
		logs.Info(logs.ModuleSync, "初次运行，开始初始化云端数据库...")
	} else {
		logs.Info(logs.ModuleSync, "云端目录 FID 已变更，将清空旧索引并重新全量扫描",
			"旧FID", dbFid, "新FID", cloudFid)
		l.env.DB.BatchClearPaths([]string{l.env.Paths.SyncPath})
	}
	l.env.DB.SaveRecord(l.env.Paths.SyncPath, cloudFid, db.SizeDir)

	var scanErr error
	defer func() {
		if scanErr != nil {
			logs.Error(logs.ModuleSync, "云端扫描被中止，正在清理数据库", "错误信息", scanErr)
			l.env.DB.BatchClearPaths([]string{l.env.Paths.SyncPath})
		}
	}()
	// 新索引构建成功后清理旧同步根：失败不删；旧目录是新目录子树时只删不重叠分支。
	defer func() {
		if scanErr == nil {
			l.env.DB.ClearOldRoot(oldSyncPath, l.env.Paths.SyncPath)
		}
	}()

	scanErr = l.env.WalkCloud(ctx, l.env.Paths.SyncPath, l.env.Paths.SyncFid, Visitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			l.env.DB.SaveRecord(path, fid, db.SizeDir)
			return true, nil
		},
		VisitFile: func(_ context.Context, path, fid, _ string, e Entry) error {
			saveSize := e.Size
			if e.IsVideo {
				path = strings.TrimSuffix(path, filepath.Ext(path)) + ".strm"
				saveSize = 0
				if info, err := os.Stat(path); err == nil {
					if _, localFid := ExtractPickcode(path); localFid == fid {
						saveSize = info.ModTime().Unix()
					}
				}
			}
			l.env.DB.SaveRecord(path, fid, saveSize)
			return nil
		},
	}, stopWithErr)
	if scanErr != nil {
		cancel(scanErr)
		return scanErr
	}
	logs.Info(logs.ModuleSync, "云端数据库初始化完成")
	return nil
}

// initTemp 查询云端回收目录 FID，只存内存（不落库，避免 temp_path 变更后用过期 FID）。
func (l *instance) initTemp(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		logs.Warn(logs.ModuleSync, "Temp目录初始化", "错误信息", err)
		return err
	}
	info, err := l.env.API.GetDirInfo(ctx, l.env.Paths.TempPath)
	if err != nil {
		return err
	}
	l.env.Paths.TempFid = info.Fid
	return nil
}

// ensureDirs 建好配置里的目录：双栖路径(sync/strm)建本地+云端，temp 只建云端。
func (l *instance) ensureDirs(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	dirs := []struct {
		path  string
		local bool
	}{
		{l.env.Paths.SyncPath, true},
		{l.env.Paths.StrmPath, true},
		{l.env.Paths.TempPath, false},
	}
	for _, d := range dirs {
		if strings.TrimSpace(d.path) == "" {
			continue
		}
		if d.local {
			if err := os.MkdirAll(d.path, 0755); err != nil {
				return fmt.Errorf("[初始化] 创建本地目录失败 %s: %w", d.path, err)
			}
		}
		if _, err := AddCloudFolder(ctx, l.env, "", d.path); err != nil {
			return fmt.Errorf("[初始化] 创建云端目录失败 %s: %w", d.path, err)
		}
	}
	return nil
}

// cronSync 定时全量同步：每 CronInterval 触发本地全量扫描+云端同步。
// cron.enabled=false 时挂起空转，仅依赖文件监听。
func (l *instance) cronSync(ctx context.Context) {
	if !l.env.CronEnabled {
		logs.Info(logs.ModuleSync, "定时全量同步已关闭（配置 cron.enabled=false），仅依赖本地文件监听")
		<-ctx.Done()
		return
	}
	interval := l.env.CronInterval
	logs.Info(logs.ModuleSync, "定时全量同步已启用", "间隔", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			logs.Info(logs.ModuleSync, "触发定时全量同步任务")
			l.FullScan(ctx)
			l.cloudTask.Start(ctx, func(c context.Context) {
				runCloudSync(c, l.env, l.cloudTask)
			})
		case <-ctx.Done():
			return
		}
	}
}
