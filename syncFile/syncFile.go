// Package syncFile 是文件同步功能的「根包/门面」：本身不含具体业务逻辑，
// 负责把 core（共享设施）与 local（本地同步）、cloud（云端同步）、strm（STRM 生成）
// 三个功能模块装配成一个整体，并对外提供统一的调用入口。
//
// 【本包包含什么】
//   - syncFile.go：SyncFile 组合器与 New() 装配流程、对 web 层的五个门面方法；
//   - bootstrap.go：启动时的初始化编排（建库扫描 initRoot、回收目录 initTemp）；
//   - cron.go：每 12 小时的定时全量同步；
//   - runner.go：Runner 热重载生命周期管理（配置变更后重建 SyncFile 实例）。
//
// 【各模块干什么】（要看具体业务，请直接进入对应子包）
//   - local/  本地 → 云端：监听本地文件变化，上传新文件、清理云端多余项；
//   - cloud/  云端 → 本地：全量遍历云端，下载新文件、为视频生成 .strm；
//   - strm/   为云端媒体库批量生成 .strm 索引文件；
//   - core/   三个模块共用的零件（运行环境/云端遍历器/进度统计/文件存取）。
//
// 【整体生命周期】
//  1. main 建 Runner → Runner.Start → New()：
//     构造 core.Env → initRoot 扫描云端建数据库索引 → initTemp 获取回收目录 FID
//     → 构造 local/cloud/strm 三模块 → 启动 local 后台协程与定时任务；
//  2. 本地同步（常驻）：文件事件 → 静默窗口 → 判定 → 上传/清理；
//  3. 云端同步 / STRM 生成（面板手动或定时触发）：经下方门面方法启动；
//  4. 配置变更：Runner.Reload 取消旧实例全部协程，用新配置重建。
package syncFile

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
	"115tools/syncFile/cloud"
	"115tools/syncFile/core"
	"115tools/syncFile/local"
	"115tools/syncFile/strm"
	"context"
	"sync"
)

// SyncFile 是三个功能模块的组合器，也是 web 层唯一的调用入口（门面）。
// 它自身不实现业务，只把调用委托给对应模块。
type SyncFile struct {
	env   *core.Env    // 共享运行环境（三个模块共同依赖的「工具箱」）
	local *local.Local // 本地同步模块（常驻：监听器 + 队列 + 上传池）
	cloud *cloud.Cloud // 云端同步模块（按需触发的一轮轮全量任务）
	strm  *strm.Strm   // STRM 生成模块（按需触发的一轮轮全量任务）
}

// New 装配并初始化整个同步系统。
//
// statsChanged 是状态变更通知通道，由 Runner 创建并跨热重载实例共享：
// 注入给 cloud/strm 两个任务的进度统计器，使 web 层 SSE 在实例替换后仍能持续接收。
// wg 是全局等待组：本函数启动的后台协程都挂在它上面，保证进程退出前收尾完成。
func New(ctx context.Context, cfg *config.Config, api *drive.Open115, boltDB *db.DB, wg *sync.WaitGroup, statsChanged chan struct{}) (*SyncFile, error) {
	env := core.NewEnv(cfg, api, boltDB)
	s := &SyncFile{env: env}

	// 第一步：初始化编排（必须先于模块启动）。
	// initRoot 需要扫描云端建立数据库索引，local 的扫描对比依赖这些记录。
	if err := s.initRoot(ctx); err != nil {
		return nil, err
	}
	if err := s.initTemp(ctx); err != nil {
		return nil, err
	}

	// 第二步：构造三个功能模块（共享同一个 env 与通知通道）。
	s.local = local.New(env)
	s.cloud = cloud.New(env, statsChanged)
	s.strm = strm.New(env, statsChanged)

	// 第三步：启动常驻后台协程（本地同步全套 + 定时全量同步）。
	s.local.Start(ctx, wg)
	wg.Go(func() { s.cronSync(ctx) })
	return s, nil
}

// ──── 以下是 web 层调用的五个门面方法（签名与历史版本保持一致）────

// StartCloudSync 启动一轮云端全量同步（委托 cloud 模块；重复触发自动忽略）。
func (s *SyncFile) StartCloudSync(ctx context.Context) { s.cloud.Start(ctx) }

// StopCloudSync 停止正在运行的云端同步。
func (s *SyncFile) StopCloudSync() { s.cloud.Stop() }

// StartAddStrm 启动一轮 STRM 生成任务（委托 strm 模块；重复触发自动忽略）。
func (s *SyncFile) StartAddStrm(ctx context.Context) { s.strm.Start(ctx) }

// StopAddStrm 停止正在运行的 STRM 生成任务。
func (s *SyncFile) StopAddStrm() { s.strm.Stop() }

// StatusSnapshot 返回云端同步/STRM 生成两个任务的进度 JSON，供 web 层 SSE 推送。
// 收口内部状态读取，web 层无需（也无法）触碰模块内部字段。
func (s *SyncFile) StatusSnapshot() (syncJSON, strmJSON string) {
	return s.cloud.StatusJSON(), s.strm.StatusJSON()
}
