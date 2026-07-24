// Package core 是 syncFile 下三个功能模块的共享层：
//   - local（本地同步：本地 → 云端上传方向）
//   - cloud（云端同步：云端 → 本地下载方向）
//   - strm（STRM 生成）
//
// 【职责】只放三个模块都要用的基础设施：
//   - Env：运行环境（115 API 客户端、数据库、路径配置、API 并发配额）
//   - WalkCloud：统一的云端目录递归遍历器
//   - TaskStats：任务进度统计与变更通知
//   - 文件存取与工具：下载文件、写 .strm、视频判定、pickcode 解析、失败记录
//
// 【边界】本包不依赖 local/cloud/strm 中的任何一个（依赖方向只能是
// 各模块 → core）。因此 core 里不放任何具体业务流程，只放「谁都用得到」的零件；
// 具体「拿到一个文件/目录后做什么」永远由使用方模块自己决定。
package core

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
	"context"
)

// Env 是三个功能模块共享的运行环境，可以理解为「公共工具箱」：
// 模块不各自保管 API 客户端、数据库连接，而是统一从这里取用。
//
// 生命周期：由 syncFile 根包的 New() 调用 NewEnv 构造一次，
// 之后以指针形式注入 local/cloud/strm 三个模块，全程只读。
// （唯一例外是 Paths 里的 SyncFid/TempFid 会在 bootstrap 阶段回填，见 Paths 注释。）
type Env struct {
	API   *drive.Open115 // 115 网盘 API 客户端（token 刷新、限流由 drive 包内部自动处理）
	DB    *db.DB         // 本地文件索引数据库（bbolt），记录 路径 → 云端FID + 大小
	Paths *Paths         // 路径与 FID 配置，三个模块共用同一份
	Sem   chan struct{}  // 云端 API 并发配额（容量 5），仅在 GetFileList 调用期间持有
}

// NewEnv 根据配置创建共享运行环境。
// 调用方：syncFile 根包的 New()；每次启动/热重载时调用一次。
func NewEnv(cfg *config.Config, api *drive.Open115, boltDB *db.DB) *Env {
	return &Env{
		API: api,
		DB:  boltDB,
		Paths: &Paths{
			SyncPath: cfg.SyncPath,
			TempPath: cfg.TempPath,
			StrmPath: cfg.StrmPath,
			StrmUrl:  cfg.StrmUrl,
			Settle:   SettleDuration(cfg.SettleSeconds),
		},
		Sem: make(chan struct{}, 5),
	}
}

// AcquireSlot 获取一个云端 API 并发配额，成功返回 true；
// ctx 被取消时返回 false，调用方应放弃本次操作。
//
// 使用约定（防止死锁，务必遵守）：配额必须在 API 调用结束后立即释放，
// 不得持有到子目录递归结束——否则父目录持锁等子目录完成、
// 子目录又等空闲配额，双方互相等待形成死锁。正确用法见 WalkCloud。
func (e *Env) AcquireSlot(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case e.Sem <- struct{}{}:
		return true
	}
}
