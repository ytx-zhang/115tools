// Package config 负责配置的加载、校验与热更新。
//
// 结构说明：
//   - Config：JSON 存储模型（配置文件字段，含私有 token/path/mu）。
//   - Editable：JSON 传输 DTO（web 面板快照与更新），见 settings.go。
//   - Snapshot/Update：配置快照与合并更新（空字段=保持原值）。
//   - 死字段已清理：Editable 不再含 ConfigReady/MissingFields/HasPassword（前端不消费）。
package config

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// Config 包含所有业务路径和 Token 操作方法。
type Config struct {
	// 静态配置字段：外部直接通过 cfg.SyncPath 访问
	SyncPath string `json:"sync_path"`
	StrmPath string `json:"strm_path"`
	TempPath string `json:"temp_path"`
	StrmUrl  string `json:"strm_url"`

	// 视频文件扩展名白名单：命中且体积达阈值的文件按「视频」处理
	// （上传后替换为 .strm 索引而非落地原文件）。为空时使用内置默认（见 DefaultVideoExts）。
	// 可通过配置文件或 Web 设置页修改，二者保持一致。
	VideoExts []string `json:"video_exts"`

	// 上传排除名单：这些后缀（或整名，如 .DS_Store / Thumbs.db）的文件不上传，
	// 且云端已存在的同名项会被联动清理。用于跳过下载器/系统的临时半成品文件。
	// 为空时不排除任何文件（运行期名单为空即不过滤）；可通过配置文件或 Web 设置页修改。
	UploadExclude []string `json:"upload_exclude"`

	// 透传本地缓存保留期（天）：上传完成的视频移入本地缓存后保留 N 天，到期由清理协程回收；
	// 0 表示使用默认 1 天。仅透传模式生效（命中本地可跳过 115 上游回源，详见 internal/cache）。
	CacheRetentionDays int `json:"cache_retention_days"`

	// 本地同步去抖窗口（分钟）：非视频事件（.strm/目录）监听后等待该时长内无新事件才批量同步，
	// 避免扫描/上传过程中其他程序仍在修改文件造成竞态。0 表示使用默认 10 分钟。
	// 视频文件事件实时直传，不走此窗口（秒级生效）。
	DebounceMinutes int `json:"debounce_minutes"`

	// Cron 定时全量同步配置（嵌套段）。
	Cron CronConfig `json:"cron"`

	// Auth 前端管理页登录凭据；Username 为空表示关闭登录验证。
	// 密码仅以 bcrypt 哈希存储（PasswordHash），绝不保存明文。
	// /download 直链接口始终不做验证（供 Emby 使用）。
	Auth AuthConfig `json:"auth"`

	// 内部私有属性
	path  string
	mu    sync.RWMutex
	token tokenData
}

// DefaultVideoExts 视频文件扩展名内置默认白名单（常见视频格式），是 video_exts 的唯一默认来源；
// 配置未显式设置 video_exts 时由 loadConfig 回退到它。
var DefaultVideoExts = []string{
	".mp4", ".mkv",
}

// AuthConfig 前端登录的账号密码；密码仅以 bcrypt 哈希存储。
type AuthConfig struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// CronConfig 定时全量同步配置（嵌套段，JSON 键 cron）。
// Enabled 用 *bool：nil 表示未显式设置，按「默认开启」处理；
// 只有显式写 enabled: false（面板取消勾选或手动 YAML）才是真正关闭，
// 确保「默认开启」与「用户能关掉」两个语义同时成立。
type CronConfig struct {
	Enabled       *bool `json:"enabled"`        // 是否启用；nil = 未设置，默认开启
	IntervalHours int   `json:"interval_hours"` // 间隔（小时），0 表示默认 12
}

// defaultCronInterval 定时全量同步默认间隔（IntervalHours <= 0 时回退），
// 是「12 小时」这一默认值的唯一来源（加载、Update 写盘、CronInterval 读取三处共用）。
const defaultCronInterval = 12 * time.Hour

// normalizeCronInterval 归一 cron 间隔小时数：<=0 回退 defaultCronInterval。
// 唯一的兜底实现——避免加载/更新/读取三处各写一遍 12 而彼此漂移。
func normalizeCronInterval(hours int) int {
	if hours <= 0 {
		return int(defaultCronInterval / time.Hour)
	}
	return hours
}

// DefaultCacheRetentionDays 透传本地缓存保留期默认天数（CacheRetentionDays <= 0 时回退），
// 是「1 天」这一默认值的唯一来源（加载、Update 写盘、CacheRetention 读取三处共用）。
const DefaultCacheRetentionDays = 1

// normalizeCacheRetentionDays 归一缓存保留天数：<=0 回退默认 1 天，
// 避免 0/负导致缓存被瞬间清空（retention 为 0 时 cleanup 会删掉所有刚写入的缓存）。
func normalizeCacheRetentionDays(days int) int {
	if days <= 0 {
		return DefaultCacheRetentionDays
	}
	return days
}

type tokenData struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpireAt     time.Time `json:"expire_at"`
}

// configFile 是配置文件的 JSON 序列化模型。
// 嵌入 *Config 以避免复制其内部的 sync.RWMutex；Token 单独成段持久化。
type configFile struct {
	*Config
	Token tokenData `json:"token"`
}

// New 读取配置文件（JSON 格式）。文件不存在时创建空白骨架（字段全空，供 web 面板填写）。
func New(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := &Config{path: path}
			cfg.mu.Lock()
			if genErr := cfg.persistLocked(); genErr != nil {
				cfg.mu.Unlock()
				return nil, fmt.Errorf("创建配置文件失败: %w", genErr)
			}
			cfg.mu.Unlock()
			logs.Warn(logs.ModuleSystem, "配置文件已创建，请通过管理面板填写后保存以启动同步", "路径", path)
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var f configFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// cron 间隔兜底
	f.Cron.IntervalHours = normalizeCronInterval(f.Cron.IntervalHours)

	// 缓存保留天数兜底
	f.CacheRetentionDays = normalizeCacheRetentionDays(f.CacheRetentionDays)

	// 视频扩展名白名单：未设置时回退内置默认（克隆避免后续修改污染全局默认值）
	if len(f.VideoExts) == 0 {
		f.VideoExts = slices.Clone(DefaultVideoExts)
	}

	cfg := f.Config
	cfg.path = path
	cfg.token = f.Token
	return cfg, nil
}

// Status 返回配置完备状态——供初始化步骤与前端 SSE 使用。
func (c *Config) Status() ConfigStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	miss := c.missingLocked()
	return ConfigStatus{Ready: len(miss) == 0, Missing: miss}
}

// CronEnabled 返回定时全量同步是否启用：未显式设置（Enabled 为 nil）按「默认开启」处理。
func (c *Config) CronEnabled() bool {
	return c.Cron.Enabled == nil || *c.Cron.Enabled
}

// CronInterval 返回定时全量同步间隔；IntervalHours <= 0 时回退默认 12 小时（见 normalizeCronInterval）。
func (c *Config) CronInterval() time.Duration {
	return time.Duration(normalizeCronInterval(c.Cron.IntervalHours)) * time.Hour
}

// CacheRetention 返回透传本地缓存保留期；CacheRetentionDays <= 0 时回退默认 1 天（见 normalizeCacheRetentionDays）。
func (c *Config) CacheRetention() time.Duration {
	return time.Duration(normalizeCacheRetentionDays(c.CacheRetentionDays)) * 24 * time.Hour
}

// Token 返回当前 token 快照（访问令牌 / 刷新令牌 / 到期时间）。
func (c *Config) Token() tokenData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

func (c *Config) SaveToken(access, refresh string, expiresIn int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.token.AccessToken = access
	if refresh != "" {
		c.token.RefreshToken = refresh
	}

	// 计算下次到期时间
	expireAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
	c.token.ExpireAt = expireAt

	if err := c.persistLocked(); err != nil {
		logs.Error(logs.ModuleSystem, "Token 写盘失败，内存已更新但未落盘，重启后可能读到旧 Token", "错误", err)
		return
	}
}

// persistLocked 序列化并写盘，调用方必须已持有 c.mu 写锁。
func (c *Config) persistLocked() error {
	var v jsontext.Value
	raw, err := json.Marshal(configFile{Config: c, Token: c.token})
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	if err := v.UnmarshalJSON(raw); err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	if err := v.Indent(jsontext.WithIndent("  ")); err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	out := []byte(v)
	return os.WriteFile(c.path, out, 0644)
}
