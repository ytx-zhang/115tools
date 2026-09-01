// Package conf 负责配置的加载、校验、持久化与热更新。
//
// 配置分三层：
//   - Settings：全局设置（strm 直链前缀、云端回收目录、透传缓存目录等），对所有任务生效；
//   - Task：同步任务，绑定一对「本地目录 ↔ 云端目录」，用一组平铺开关描述要做什么。
//     不再区分 push / pull 两种互斥类型——上传和下载只是两个可以任意组合的开关；
//   - Token：115 访问/刷新令牌（敏感字段，运行时轮换，独立成段持久化）。
//
// 本文件定义任务模型与目录重叠校验；config.go 定义全局段与文件读写。
package conf

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// Task 一个同步任务：一对目录映射 + 一组平铺开关。
//
// 开关分三组，互不排斥（上传与下载可以同时开，就是双向镜像）：
//   - 本地 → 云端：Upload 总开关，Watch / InstantNow 是它的细化；ToStrm 上传后替换为
//     .strm、ToCache 原件移入本地缓存；
//   - 云端 → 本地：Download 下载缺失、Archive 归档顶层项、ToStrmDl 下载落地为 .strm；
//   - 定时：Cron 同时驱动两个方向（各按上面的开关决定跑不跑）。
//
// 冗余副本清理无配置项：双向（上传+下载同时开）任务由引擎自动开启。
type Task struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	LocalDir string `json:"local_dir"` // 本地绝对路径
	CloudDir string `json:"cloud_dir"` // 115 云端绝对路径（以 / 开头）

	// 本地 → 云端
	Upload bool `json:"upload"` // 上传本地新增/变更到云端
	Watch  bool `json:"watch"`  // 文件事件监听（依赖 Upload）
	// QuietMinutes 监听静默时间（分钟）；<=0 回退默认 10
	QuietMinutes int `json:"quiet_minutes"`
	// InstantNow 视频或 .strm 文件事件立即同步（绕过静默窗口）；其余文件仍按静默防抖合批
	InstantNow bool `json:"instant_now"`
	ToStrm     bool `json:"to_strm"`  // 上传后本地替换为 .strm（关 = 保留实体视频）
	ToCache    bool `json:"to_cache"` // 上传后原件移入本地透传缓存（关 = 删除原件）

	// 云端 → 本地
	Download bool `json:"download"`
	// Archive 完成后把云端顶层项移入回收目录——纯下载（未开上传）任务专用，开启上传时 normalize 强制关闭
	Archive  bool `json:"archive"`
	ToStrmDl bool `json:"to_strm_dl"` // 下载落地为 .strm（关 = 下载实体视频）

	Cron CronOpts `json:"cron"`
}

// CronOpts 定时任务配置。
type CronOpts struct {
	Enabled       bool `json:"enabled"`        // 是否启用定时
	IntervalHours int  `json:"interval_hours"` // 间隔（小时）；<=0 回退默认 12
}

// UploadEnabled 是否启用「本地 → 云端」（含监听细化开关）。
func (t Task) UploadEnabled() bool { return t.Upload }

// DownloadEnabled 是否启用「云端 → 本地」（下载缺失、归档任一开启即算）。
func (t Task) DownloadEnabled() bool { return t.Download || t.Archive }

// WatchEnabled 是否启用文件事件监听（未开上传时监听没有意义）。
func (t Task) WatchEnabled() bool { return t.Upload && t.Watch }

// QuietWindow 返回监听静默窗口（已填默认值）。
func (t Task) QuietWindow() int {
	if t.QuietMinutes <= 0 {
		return defaultQuietMinutes
	}
	return t.QuietMinutes
}

// CronInterval 返回定时间隔小时数（已填默认值）。
func (t Task) CronInterval() int {
	if t.Cron.IntervalHours <= 0 {
		return defaultCronHours
	}
	return t.Cron.IntervalHours
}

// normalize 清理无效组合并夹住非法取值：未开上传时，监听及其细化开关一并失效。
// 默认值不在这里落库（由 QuietWindow / CronInterval 在读取时填），避免配置里出现
// 「显式写着 10 但其实用户没填」的假值。
func (t *Task) normalize() {
	if t.Upload {
		// 归档是纯下载专用：开启上传（双向/纯上传）时强制关闭，避免把云端顶层项归档走
		t.Archive = false
	} else {
		t.Watch, t.InstantNow = false, false
	}
	if t.QuietMinutes < 0 {
		t.QuietMinutes = 0
	}
	if t.Cron.IntervalHours < 0 {
		t.Cron.IntervalHours = 0
	}
}

// NewID 生成稳定唯一的任务 ID（t_ + 8 字节随机 hex）。
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败属不可恢复的系统级异常；任务 ID 冲突概率可忽略，此处退化为时间戳式兜底。
		return "t_" + fmt.Sprintf("%x", now().UnixNano())
	}
	return "t_" + hex.EncodeToString(b[:])
}

// ──── 校验 ────

// Validate 校验单任务字段合法性（不涉及任务间关系）。返回首个错误。
func (t *Task) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("任务名不能为空")
	}
	if !filepath.IsAbs(t.LocalDir) {
		return fmt.Errorf("本地目录必须是绝对路径: %s", t.LocalDir)
	}
	if !strings.HasPrefix(t.CloudDir, "/") {
		return fmt.Errorf("云端目录必须以 / 开头: %s", t.CloudDir)
	}
	if !t.Enabled {
		return nil
	}
	// 启用的任务至少要有一个方向，否则挂在那里什么都不做
	if !t.UploadEnabled() && !t.DownloadEnabled() {
		return fmt.Errorf("任务至少要开启「上传」或「下载」其中一个方向")
	}
	return nil
}

// overlapClean 判断两个**已规范化**的路径是否重叠（相等或互为祖先/后代，即嵌套）。
// 用于禁止两个任务指向同一目录或其子目录，否则路径索引会互相污染。
func overlapClean(a, b string, sep byte) bool {
	if a == b {
		return true
	}
	// 前缀判定需补分隔符，避免 /a/bb 与 /a/b 误判为嵌套。
	return strings.HasPrefix(a, b+string(sep)) || strings.HasPrefix(b, a+string(sep))
}

// CleanCloudPath 规范化云端路径：去除尾斜杠（保留根 "/"）。
// 导出供 sync 等路径映射复用（云端路径统一以 / 分隔，与本地文件系统无关）。
func CleanCloudPath(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	return p
}

// validateOverlaps 校验一组任务内部无目录重叠：本地目录之间、云端目录之间分别不得重叠/嵌套。
// 本地目录与云端目录属不同命名空间（文件系统 vs 115 路径），不互相排斥。
func validateOverlaps(tasks []Task) error {
	locals := make([]string, 0, len(tasks))
	clouds := make([]string, 0, len(tasks))
	for _, t := range tasks {
		locals = append(locals, filepath.Clean(t.LocalDir))
		clouds = append(clouds, CleanCloudPath(t.CloudDir))
	}
	for i := range locals {
		for j := i + 1; j < len(locals); j++ {
			if overlapClean(locals[i], locals[j], filepath.Separator) {
				return fmt.Errorf("本地目录冲突：%s 与 %s 重叠或嵌套", locals[i], locals[j])
			}
		}
	}
	for i := range clouds {
		for j := i + 1; j < len(clouds); j++ {
			if overlapClean(clouds[i], clouds[j], '/') {
				return fmt.Errorf("云端目录冲突：%s 与 %s 重叠或嵌套", clouds[i], clouds[j])
			}
		}
	}
	return nil
}

// validateTasks 全量校验任务集合：单任务合法性 + 名称唯一 + 目录不重叠。
func validateTasks(tasks []Task) error {
	seen := make(map[string]struct{}, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		if err := t.Validate(); err != nil {
			return fmt.Errorf("任务[%s]: %w", t.Name, err)
		}
		if _, dup := seen[t.Name]; dup {
			return fmt.Errorf("任务名重复: %s", t.Name)
		}
		seen[t.Name] = struct{}{}
	}
	return validateOverlaps(tasks)
}
