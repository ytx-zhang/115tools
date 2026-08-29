// Package engine 是同步引擎：管理多个任务的运行时单元（TaskUnit）的生命周期、调度与热重建。
//
// 职责划分：
//   - Engine：任务单元集合（map[taskID]*TaskUnit）的编排——Init/Start/ReloadTask/启停/状态汇总；
//   - TaskUnit：单个任务的双方向运行时（push/pull），见 taskunit.go；
//   - push/pull：两个方向的具体实现子包；
//   - kit：双方向共享的底层能力。
//
// 依赖方向：engine → pan / vault / conf / journal / kit；engine 不 import webui。
package engine

import (
	"context"
	"fmt"
	"sync"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/engine/kit"
	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
	"github.com/ytx-zhang/115tools/internal/vault"
)

// TaskRuntime 单任务运行时状态（供 webui 的 SSE 推送）。
type TaskRuntime struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Running   bool   `json:"running"`
	Completed int64  `json:"completed"`
	Total     int64  `json:"total"`
}

// Engine 任务引擎（组合根：管理全部任务单元）。
type Engine struct {
	pan      *pan.Client
	vault    *vault.Index
	conf     *conf.Config
	journal  *journal.Store
	cache    kit.CacheMover
	rules    kit.Rules
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
func New(api *pan.Client, v *vault.Index, cfg *conf.Config, j *journal.Store, cache kit.CacheMover, onChange func(), appCtx context.Context, appWg *sync.WaitGroup) *Engine {
	return &Engine{
		pan:      api,
		vault:    v,
		conf:     cfg,
		journal:  j,
		cache:    cache,
		rules:    kit.NewRules(cfg),
		onChange: onChange,
		appCtx:   appCtx,
		appWg:    appWg,
		units:    make(map[string]*TaskUnit),
	}
}

// Init 完成运行时初始化：解析全局回收目录 FID，并为每个启用任务构建并初始化单元。
func (e *Engine) Init(ctx context.Context) error {
	info, err := e.pan.GetDirInfo(ctx, e.conf.Settings.TempDir)
	if err != nil {
		fid, ferr := e.ensureTemp(ctx)
		if ferr != nil {
			return ferr
		}
		e.tempFid = fid
	} else {
		e.tempFid = info.Fid
	}

	for _, t := range e.conf.ListTasks() {
		if !t.Enabled {
			continue
		}
		u := e.newUnit(t)
		if err := u.init(ctx); err != nil {
			return fmt.Errorf("初始化任务 %s 失败: %w", t.Name, err)
		}
		e.mu.Lock()
		e.units[t.ID] = u
		e.mu.Unlock()
	}
	return nil
}

// ensureTemp 逐级创建全局回收目录并返回 FID。
func (e *Engine) ensureTemp(ctx context.Context) (string, error) {
	return kit.EnsureCloudDir(ctx, e.pan, e.conf.Settings.TempDir)
}

// EnsureRunning 幂等启动引擎：首次调用时 Init + Start（配置完备且 token 有效后），后续调用直接返回。
func (e *Engine) EnsureRunning() error {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if started {
		return nil
	}
	e.bootstrapMu.Lock()
	defer e.bootstrapMu.Unlock()

	e.mu.Lock()
	started = e.started
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
	e.rules = kit.NewRules(e.conf)

	info, err := e.pan.GetDirInfo(e.appCtx, e.conf.Settings.TempDir)
	if err != nil {
		fid, ferr := e.ensureTemp(e.appCtx)
		if ferr != nil {
			return ferr
		}
		e.tempFid = fid
	} else {
		e.tempFid = info.Fid
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
	units := make([]*TaskUnit, 0, len(e.units))
	for _, u := range e.units {
		units = append(units, u)
	}
	e.mu.Unlock()

	for _, u := range units {
		u.start(ctx, wg)
	}
}

// ReloadTask 热重建单个任务：停旧单元，按新配置重建并启动（不影响其他任务）。
func (e *Engine) ReloadTask(task conf.Task) error {
	e.mu.Lock()
	old := e.units[task.ID]
	delete(e.units, task.ID)
	started := e.started
	e.mu.Unlock()

	if old != nil {
		old.stop()
	}
	if !task.Enabled {
		return nil
	}

	u := e.newUnit(task)
	if err := u.init(e.appCtx); err != nil {
		return fmt.Errorf("初始化任务 %s 失败: %w", task.Name, err)
	}
	e.mu.Lock()
	e.units[task.ID] = u
	e.mu.Unlock()

	if started {
		var wg sync.WaitGroup
		u.start(e.appCtx, &wg)
		e.appWg.Go(wg.Wait)
	}
	if e.onChange != nil {
		e.onChange()
	}
	return nil
}

// RemoveTask 停止并移除单个任务单元（配合 conf.RemoveTask 使用）。
func (e *Engine) RemoveTask(id string) {
	e.mu.Lock()
	old := e.units[id]
	delete(e.units, id)
	e.mu.Unlock()
	if old != nil {
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
	e.mu.Lock()
	units := make([]*TaskUnit, 0, len(e.units))
	for _, u := range e.units {
		units = append(units, u)
	}
	e.mu.Unlock()
	for _, u := range units {
		u.stop()
	}
}
