package conf

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"
)

// now 返回当前时间，便于测试替换。
var now = time.Now

// DefaultCacheRetentionDays 透传缓存默认保留天数（CacheRetentionDays <= 0 时回退）。
const DefaultCacheRetentionDays = 1

// defaultQuietMinutes 监听静默时间默认值（分钟）。
const defaultQuietMinutes = 10

// defaultCronHours 定时任务默认间隔（小时）。
const defaultCronHours = 12

// DefaultVideoExts 视频扩展名默认白名单（video_exts 未设置时回退）。
var DefaultVideoExts = []string{".mp4", ".mkv"}

// Auth 前端登录凭据；密码仅存 bcrypt 哈希。
type Auth struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// Settings 全局设置（对所有任务生效）。
type Settings struct {
	StrmURL    string `json:"strm_url"`    // .strm 直链前缀（/download 地址）
	TempDir    string `json:"temp_dir"`    // 云端回收目录（清理视频时移入，可找回）
	CacheDir   string `json:"cache_dir"`   // 本地透传缓存目录（上传后视频暂存，命中直读）
	OfflineDir string `json:"offline_dir"` // 离线下载默认保存目录（空=云端根 /）

	CacheRetentionDays int `json:"cache_retention_days"` // 缓存保留天数；<=0 回退默认 1

	VideoExts     []string `json:"video_exts"`     // 视频扩展名白名单（空则用内置默认）
	UploadExclude []string `json:"upload_exclude"` // 上传排除名单（后缀或整名）

	Auth Auth `json:"auth"`
}

// TokenData 115 访问/刷新令牌（敏感，独立持久化，绝不回显前端明文）。
type TokenData struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpireAt     time.Time `json:"expire_at"`
}

// Config 是配置的内存态：全局设置 + 任务集合 + 令牌。并发安全（RWMutex）。
type Config struct {
	Settings Settings
	Tasks    []Task

	path  string
	mu    sync.RWMutex
	token TokenData
}

// configFile 是配置文件的 JSON 序列化模型（与内存态分离，避免序列化互斥锁）。
type configFile struct {
	Settings Settings  `json:"settings"`
	Tasks    []Task    `json:"tasks"`
	Token    TokenData `json:"token"`
}

// New 加载配置文件；文件不存在时创建空白骨架（字段全空，供前端填写）。
func New(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := &Config{path: path}
			cfg.mu.Lock()
			if serr := cfg.persistLocked(); serr != nil {
				cfg.mu.Unlock()
				return nil, fmt.Errorf("创建配置文件失败: %w", serr)
			}
			cfg.mu.Unlock()
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var f configFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg := &Config{
		Settings: f.Settings,
		Tasks:    f.Tasks,
		path:     path,
		token:    f.Token,
	}
	cfg.normalizeLocked()
	return cfg, nil
}

// normalizeLocked 归一化默认值（调用方需持有写锁）：视频扩展名、缓存保留期、任务内的去抖/定时间隔。
func (c *Config) normalizeLocked() {
	if len(c.Settings.VideoExts) == 0 {
		c.Settings.VideoExts = slices.Clone(DefaultVideoExts)
	}
	if c.Settings.CacheRetentionDays <= 0 {
		c.Settings.CacheRetentionDays = DefaultCacheRetentionDays
	}
	for i := range c.Tasks {
		t := &c.Tasks[i]
		if t.Watch.QuietMinutes <= 0 {
			t.Watch.QuietMinutes = defaultQuietMinutes
		}
		if t.Rescan.IntervalHours <= 0 {
			t.Rescan.IntervalHours = defaultCronHours
		}
		if t.PullCron.IntervalHours <= 0 {
			t.PullCron.IntervalHours = defaultCronHours
		}
	}
}

// persistLocked 序列化并写盘（调用方需持有写锁）。JSON 用缩进便于人工排查。
func (c *Config) persistLocked() error {
	raw, err := json.Marshal(configFile{Settings: c.Settings, Tasks: c.Tasks, Token: c.token})
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	var v jsontext.Value
	if err := v.UnmarshalJSON(raw); err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	if err := v.Indent(jsontext.WithIndent("  ")); err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	return os.WriteFile(c.path, []byte(v), 0o644)
}

// Status 返回配置完备状态（缺项清单），供初始化与前端横幅使用。
type Status struct {
	Ready   bool     `json:"ready"`
	Missing []string `json:"missing"`
}

// Status 计算配置缺项：refresh_token / strm_url / temp_dir / cache_dir 必填。
func (c *Config) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var miss []string
	if c.token.RefreshToken == "" {
		miss = append(miss, "refresh_token")
	}
	if c.Settings.StrmURL == "" {
		miss = append(miss, "strm_url")
	}
	if c.Settings.TempDir == "" {
		miss = append(miss, "temp_dir")
	}
	if c.Settings.CacheDir == "" {
		miss = append(miss, "cache_dir")
	}
	return Status{Ready: len(miss) == 0, Missing: miss}
}

// ──── 令牌 ────

// Token 返回当前令牌快照。
func (c *Config) Token() TokenData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// SaveToken 持久化令牌（refresh 轮换：refresh 非空才覆盖，否则保留旧值）。
func (c *Config) SaveToken(access, refresh string, expiresIn int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token.AccessToken = access
	if refresh != "" {
		c.token.RefreshToken = refresh
	}
	c.token.ExpireAt = now().Add(time.Duration(expiresIn) * time.Second)
	return c.persistLocked()
}

// ──── 任务 CRUD（均加锁并持久化） ────

// AddTask 追加任务并落盘。返回校验错误（不写盘）。
func (c *Config) AddTask(t Task) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.ID == "" {
		t.ID = NewID()
	}
	if err := c.validateTasksLocked(append(slices.Clone(c.Tasks), t)); err != nil {
		return err
	}
	c.Tasks = append(c.Tasks, t)
	c.normalizeLocked()
	return c.persistLocked()
}

// UpdateTask 按 ID 覆盖任务并落盘。任务不存在返回错误。
func (c *Config) UpdateTask(t Task) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.indexOfLocked(t.ID)
	if idx < 0 {
		return fmt.Errorf("任务不存在: %s", t.ID)
	}
	next := slices.Clone(c.Tasks)
	next[idx] = t
	if err := c.validateTasksLocked(next); err != nil {
		return err
	}
	c.Tasks = next
	c.normalizeLocked()
	return c.persistLocked()
}

// RemoveTask 按 ID 删除任务并落盘。
func (c *Config) RemoveTask(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.indexOfLocked(id)
	if idx < 0 {
		return fmt.Errorf("任务不存在: %s", id)
	}
	c.Tasks = slices.Delete(c.Tasks, idx, idx+1)
	return c.persistLocked()
}

// GetTask 按 ID 取任务（副本）。
func (c *Config) GetTask(id string) (Task, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if i := c.indexOfLocked(id); i >= 0 {
		return c.Tasks[i], true
	}
	return Task{}, false
}

// ListTasks 返回任务集合副本（按当前顺序）。
func (c *Config) ListTasks() []Task {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.Tasks)
}

// validateTasksLocked 校验追加/覆盖后的任务集合（调用方需持有写锁）。
func (c *Config) validateTasksLocked(tasks []Task) error { return validateTasks(tasks) }

func (c *Config) indexOfLocked(id string) int {
	for i := range c.Tasks {
		if c.Tasks[i].ID == id {
			return i
		}
	}
	return -1
}
