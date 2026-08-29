// Package conf 负责配置的加载、校验、持久化与热更新。
//
// 配置分为三层：
//   - Settings：全局设置（strm 直链前缀、云端回收目录、透传缓存目录等），对所有任务生效；
//   - Task：同步任务，分 push（本地→云端）与 pull（云端→本地）两类，各自绑定一对目录并携带独立开关；
//   - Token：115 访问/刷新令牌（敏感字段，运行时轮换，独立成段持久化）。
//
// 本文件定义任务模型与目录重叠校验；config.go 定义全局段与文件读写；dto.go 定义 Web 传输 DTO。
package conf

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// TaskKind 任务方向：push = 本地 → 云端；pull = 云端 → 本地。
type TaskKind string

const (
	// KindPush 本地同步到云端（可附带定时全量 + 全量后连带云端扫描）。
	KindPush TaskKind = "push"
	// KindPull 云端同步到本地。
	KindPull TaskKind = "pull"
)

// Valid 判断任务方向是否合法。
func (k TaskKind) Valid() bool { return k == KindPush || k == KindPull }

// WatchOpts 文件事件监听配置（仅 push 任务）。
type WatchOpts struct {
	Enabled      bool `json:"enabled"`       // 是否启用 inotify 文件事件监听
	QuietMinutes int  `json:"quiet_minutes"` // 监听静默时间（分钟）；<=0 回退默认 10
	StrmNow      bool `json:"strm_now"`      // .strm 文件事件立即同步（绕过静默窗口）
	VideoNow     bool `json:"video_now"`     // 视频文件事件立即同步（绕过静默窗口）
}

// CronOpts 定时任务配置。push 任务用其驱动定时全量扫描；pull 任务用其驱动定时同步。
type CronOpts struct {
	Enabled       bool `json:"enabled"`        // 是否启用定时
	IntervalHours int  `json:"interval_hours"` // 间隔（小时）；<=0 回退默认 12
}

// Task 单个同步任务。Kind 决定哪些字段生效：
//   - push：Watch / Rescan / RescanThenPull / ToStrm / ToCache；
//   - pull：PullCron / PullToStrm / DropRedundant / FetchMissing / ArchiveToTemp。
//
// 两类任务共用 LocalDir/CloudDir 这一对目录映射与 Enabled 开关。
type Task struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     TaskKind `json:"kind"`
	Enabled  bool     `json:"enabled"`
	LocalDir string   `json:"local_dir"` // 本地绝对路径
	CloudDir string   `json:"cloud_dir"` // 115 云端绝对路径（以 / 开头）

	// ── push 方向 ──
	Watch      WatchOpts `json:"watch"`
	Rescan     CronOpts  `json:"rescan"`           // 定时全量扫描
	RescanPull bool      `json:"rescan_then_pull"` // 全量扫描后是否连带云端扫描
	ToStrm     bool      `json:"to_strm"`          // 视频上传后本地替换为 .strm（关 = 保留原视频，纯云端备份）
	ToCache    bool      `json:"to_cache"`         // 上传后原视频移入透传缓存（关 = 删除原视频）

	// ── pull 方向 ──
	PullCron      CronOpts `json:"pull_cron"`       // 定时同步
	PullToStrm    bool     `json:"pull_to_strm"`    // 视频落地为 .strm（关 = 下载原视频）
	DropRedundant bool     `json:"drop_redundant"`  // 删除云端同名冗余文件
	FetchMissing  bool     `json:"fetch_missing"`   // 下载云端存在、本地不存在的文件
	ArchiveToTemp bool     `json:"archive_to_temp"` // 全部成功后把顶层项移入云端回收目录
}

// NewID 生成稳定唯一的任务 ID（t_ + 8 字节随机 hex）。
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败属不可恢复的系统级异常；任务 ID 冲突概率可忽略，此处退化为时间戳式兜底。
		return "t_" + fmt.Sprintf("%x", nowNanos())
	}
	return "t_" + hex.EncodeToString(b[:])
}

// nowNanos 返回当前纳秒时间戳，仅在 crypto/rand 失败时作为 ID 兜底来源。
func nowNanos() int64 {
	return now().UnixNano()
}

// ──── 校验 ────

// Validate 校验单任务字段合法性（不涉及任务间关系）。返回首个错误。
func (t *Task) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("任务名不能为空")
	}
	if !t.Kind.Valid() {
		return fmt.Errorf("未知任务类型: %s", t.Kind)
	}
	if !filepath.IsAbs(t.LocalDir) {
		return fmt.Errorf("本地目录必须是绝对路径: %s", t.LocalDir)
	}
	if !isCloudAbs(t.CloudDir) {
		return fmt.Errorf("云端目录必须以 / 开头: %s", t.CloudDir)
	}
	return nil
}

// isCloudAbs 判断 115 云端路径是否为绝对路径（以 / 开头）。
func isCloudAbs(p string) bool { return strings.HasPrefix(p, "/") }

// overlap 判断两个本地绝对路径是否重叠（相等或互为祖先/后代，即嵌套）。
// 用于禁止两个任务指向同一本地目录或其子目录，否则路径索引会互相污染。
func overlap(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	// 前缀判定需补分隔符，避免 /a/bb 与 /a/b 误判为嵌套。
	return strings.HasPrefix(ca, cb+string(filepath.Separator)) ||
		strings.HasPrefix(cb, ca+string(filepath.Separator))
}

// cloudOverlap 判断两个云端绝对路径是否重叠（相等或嵌套）。115 路径统一以 / 分隔。
func cloudOverlap(a, b string) bool {
	ca, cb := CleanCloudPath(a), CleanCloudPath(b)
	if ca == cb {
		return true
	}
	return strings.HasPrefix(ca, cb+"/") || strings.HasPrefix(cb, ca+"/")
}

// CleanCloudPath 规范化云端路径：去除尾斜杠（保留根 "/"）。
// 导出供 engine/kit 等路径映射复用（云端路径统一以 / 分隔，与本地文件系统无关）。
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
			if overlap(locals[i], locals[j]) {
				return fmt.Errorf("本地目录冲突：%s 与 %s 重叠或嵌套", locals[i], locals[j])
			}
		}
	}
	for i := range clouds {
		for j := i + 1; j < len(clouds); j++ {
			if cloudOverlap(clouds[i], clouds[j]) {
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
