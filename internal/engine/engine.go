// Package engine 是同步引擎：管理多个任务的运行时单元（TaskUnit）的生命周期、调度与热重建。
//
// 职责划分：
//   - Engine：任务单元集合（map[taskID]*TaskUnit）的编排——Init/Start/ReloadTask/启停/状态汇总；
//   - TaskUnit：单个任务的双方向运行时（push/pull），见 taskunit.go；
//   - push/pull：两个方向的具体实现子包；
//   - shared：双方向共享的底层能力。
//
// 依赖方向：engine → pan / index / conf / journal / shared；engine 不 import webui。
package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/engine/shared"
	"github.com/ytx-zhang/115tools/internal/index"
	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
)

// TaskRuntime 单任务运行时状态（供 webui 的 SSE 推送）。
type TaskRuntime struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Running      bool   `json:"running"`
	Initializing bool   `json:"initializing"` // 初始化中：已登记但未就绪，此时不可执行
	Completed    int64  `json:"completed"`
	Total        int64  `json:"total"`
}

// Engine 任务引擎（组合根：管理全部任务单元）。
type Engine struct {
	pan      *pan.Client
	idx      *index.Index
	conf     *conf.Config
	journal  *journal.Store
	cache    shared.CacheMover
	rules    shared.Rules
	onChange func()
	appCtx   context.Context
	appWg    *sync.WaitGroup

	mu      sync.Mutex
	units   map[string]*TaskUnit
	tempFid string // 全局回收目录 FID（Init 解析一次）
	started bool

	bootstrapMu sync.Mutex // 序列化 EnsureRunning（首次 Init+Start 只执行一次）
}

// New 构造引擎（不启动，调用方再调 Init + Start）。
func New(api *pan.Client, v *index.Index, cfg *conf.Config, j *journal.Store, cache shared.CacheMover, onChange func(), appCtx context.Context, appWg *sync.WaitGroup) *Engine {
	return &Engine{
		pan:      api,
		idx:      v,
		conf:     cfg,
		journal:  j,
		cache:    cache,
		rules:    shared.NewRules(cfg),
		onChange: onChange,
		appCtx:   appCtx,
		appWg:    appWg,
		units:    make(map[string]*TaskUnit),
	}
}

// Init 完成运行时初始化：解析全局回收目录 FID，并为每个启用任务构建并初始化单元。
func (e *Engine) Init(ctx context.Context) error {
	if err := e.resolveTempFid(ctx); err != nil {
		return err
	}

	for _, t := range e.conf.ListTasks() {
		if !t.Enabled {
			continue
		}
		if _, err := e.initUnit(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// initUnit 登记并初始化一个任务单元：期间单元在状态里带「初始化中」标记（前端卡片据此显示），
// 前后各广播一次；失败则摘除单元并带上任务名。
func (e *Engine) initUnit(ctx context.Context, t conf.Task) (*TaskUnit, error) {
	u := e.newUnit(t)
	u.initializing.Store(true)
	e.mu.Lock()
	e.units[t.ID] = u
	e.mu.Unlock()
	e.notify()

	start := time.Now()
	journal.Info(ctx, "任务初始化开始", "任务", t.Name, "本地", t.LocalDir, "云端", t.CloudDir)
	err := u.init(ctx)
	u.initializing.Store(false)
	if err != nil {
		e.popUnit(t.ID)
		journal.Error(ctx, "任务初始化失败", "任务", t.Name, "耗时", time.Since(start), "错误", err)
	}
	e.notify()
	if err != nil {
		return nil, fmt.Errorf("初始化任务 %s 失败: %w", t.Name, err)
	}
	journal.Info(ctx, "任务初始化完成", "任务", t.Name, "耗时", time.Since(start))
	return u, nil
}

// notify 广播状态变更（SSE 推给前端）。
func (e *Engine) notify() {
	if e.onChange != nil {
		e.onChange()
	}
}

// resolveTempFid 解析全局回收目录 FID：已存在直接取，否则逐级创建。
func (e *Engine) resolveTempFid(ctx context.Context) error {
	info, err := e.pan.GetDirInfo(ctx, e.conf.Settings.TempDir)
	if err != nil {
		fid, ferr := shared.EnsureCloudDir(ctx, e.pan, e.conf.Settings.TempDir)
		if ferr != nil {
			return ferr
		}
		e.tempFid = fid
		return nil
	}
	e.tempFid = info.Fid
	return nil
}

// EnsureRunning 幂等启动引擎：首次调用时 Init + Start（配置完备且 token 有效后），后续调用直接返回。
func (e *Engine) EnsureRunning() error {
	e.bootstrapMu.Lock()
	defer e.bootstrapMu.Unlock()

	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if started {
		return nil
	}
	if err := e.Init(e.appCtx); err != nil {
		return err
	}
	var wg sync.WaitGroup
	e.Start(e.appCtx, &wg)
	e.appWg.Go(wg.Wait)
	return nil
}

// ReloadAll 全局设置变更后重建全部任务单元：重新读取规则、重解析回收目录 FID。
func (e *Engine) ReloadAll() error {
	e.rules = shared.NewRules(e.conf)

	if err := e.resolveTempFid(e.appCtx); err != nil {
		return err
	}

	for _, t := range e.conf.ListTasks() {
		if err := e.ReloadTask(t); err != nil {
			return err
		}
	}
	return nil
}

// Start 启动所有任务单元的常驻协程。
func (e *Engine) Start(ctx context.Context, wg *sync.WaitGroup) {
	e.mu.Lock()
	e.started = true
	e.mu.Unlock()

	for _, u := range e.snapshotUnits() {
		u.start(ctx, wg)
	}
}

// snapshotUnits 返回任务单元快照（加锁拷贝，遍历时不持锁）。
func (e *Engine) snapshotUnits() []*TaskUnit {
	e.mu.Lock()
	defer e.mu.Unlock()
	units := make([]*TaskUnit, 0, len(e.units))
	for _, u := range e.units {
		units = append(units, u)
	}
	return units
}

// popUnit 摘除并返回任务单元（不存在返回 nil）。
func (e *Engine) popUnit(id string) *TaskUnit {
	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.units[id]
	delete(e.units, id)
	return old
}

// ReloadTask 热重建单个任务：停旧单元，按新配置重建并启动（不影响其他任务）。
func (e *Engine) ReloadTask(task conf.Task) error {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()

	old := e.popUnit(task.ID)
	if old != nil {
		old.stop()
	}
	if !task.Enabled {
		return nil
	}

	u, err := e.initUnit(e.appCtx, task)
	if err != nil {
		return err
	}

	if started {
		var wg sync.WaitGroup
		u.start(e.appCtx, &wg)
		e.appWg.Go(wg.Wait)
	}
	e.notify()
	return nil
}

// RemoveTask 停止并移除单个任务单元（配合 conf.RemoveTask 使用）。
func (e *Engine) RemoveTask(id string) {
	if old := e.popUnit(id); old != nil {
		old.stop()
	}
	if e.onChange != nil {
		e.onChange()
	}
}

// StartTask 手动执行任务：push 任务投全量扫描，pull 任务启动云端同步。
func (e *Engine) StartTask(id string) error {
	e.mu.Lock()
	u := e.units[id]
	e.mu.Unlock()
	if u == nil {
		return fmt.Errorf("任务未就绪: %s", id)
	}
	u.trigger(journal.TriggerManual)
	return nil
}

// StopTask 停止任务：push 停止扫描，pull 停止同步。
func (e *Engine) StopTask(id string) {
	e.mu.Lock()
	u := e.units[id]
	e.mu.Unlock()
	if u == nil {
		return
	}
	if u.task.Kind == conf.KindPush {
		u.stopPush()
	} else {
		u.stopPull()
	}
}

// Status 返回所有任务单元的运行时状态快照。
func (e *Engine) Status() []TaskRuntime {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]TaskRuntime, 0, len(e.units))
	for _, u := range e.units {
		out = append(out, u.runtime())
	}
	return out
}

// Shutdown 停止所有任务单元。
func (e *Engine) Shutdown() {
	for _, u := range e.snapshotUnits() {
		u.stop()
	}
}
