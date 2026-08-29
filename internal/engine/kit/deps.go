// Package kit 是同步引擎双方向共用的底层能力：路径参数、判定规则、进度封装、
// .strm 工具、云端树遍历与落地编排，以及本地/云端路径互映射。
//
// 被 engine/push 与 engine/pull 共同 import，本包不 import 任何引擎子包（避免循环依赖）。
package kit

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/pan"
	"github.com/ytx-zhang/115tools/internal/vault"
)

// CacheMover 透传缓存写入接口（消费方定义）：上传完成的视频移入缓存供 /download 直读。
// stash.Cache 实现之；nil 表示缓存未启用（上传后退化为删除原件）。
type CacheMover interface {
	Move(src, pickCode string) (string, error)
}

// Deps 是引擎子模块共享的依赖载体（避免每个构造函数一长串重复形参）。
type Deps struct {
	Pan   *pan.Client
	Vault *vault.Index
	Paths *TaskPaths
	Rules Rules
	Cache CacheMover
}

// TaskPaths 单个任务的路径与文件系统交互参数。
// 任务级：LocalDir/CloudDir/CloudFid/Debounce；全局：TempDir/TempFid/StrmURL/CacheDir（从 Settings 复制）。
type TaskPaths struct {
	LocalDir string // 本地同步根（任务级）
	CloudDir string // 云端同步根，115 绝对路径（任务级，可与 LocalDir 不同）
	CloudFid string // 云端同步根 FID（运行时解析，任务级）

	TempDir  string // 云端回收目录（全局）
	TempFid  string // 回收目录 FID（全局，运行时解析一次）
	StrmURL  string // .strm 直链前缀（全局）
	CacheDir string // 本地透传缓存根目录（全局）

	Debounce time.Duration // 监听静默窗口（任务级）
}

// NewTaskPaths 从全局设置与任务配置装配路径对象。
func NewTaskPaths(cfg *conf.Config, task conf.Task) *TaskPaths {
	return &TaskPaths{
		LocalDir: task.LocalDir,
		CloudDir: conf.CleanCloudPath(task.CloudDir),
		TempDir:  cfg.Settings.TempDir,
		StrmURL:  cfg.Settings.StrmURL,
		CacheDir: cfg.Settings.CacheDir,
		Debounce: time.Duration(task.Watch.QuietMinutes) * time.Minute,
	}
}

// MapLocalToCloud 把本地绝对路径映射为云端路径（相对 localRoot 的部分拼到 cloudRoot 下）。
func MapLocalToCloud(localRoot, cloudRoot, localPath string) string {
	rel := strings.TrimPrefix(filepath.Clean(localPath), filepath.Clean(localRoot))
	rel = strings.TrimPrefix(rel, string(filepath.Separator))
	if rel == "" {
		return conf.CleanCloudPath(cloudRoot)
	}
	return conf.CleanCloudPath(cloudRoot) + "/" + rel
}

// MapCloudToLocal 把云端路径映射为本地路径（相对 cloudRoot 的部分拼到 localRoot 下）。
func MapCloudToLocal(localRoot, cloudRoot, cloudPath string) string {
	rel := strings.TrimPrefix(filepath.Clean(cloudPath), filepath.Clean(conf.CleanCloudPath(cloudRoot)))
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return localRoot
	}
	return filepath.Join(localRoot, rel)
}
