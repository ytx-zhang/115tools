package config

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 包含所有业务路径和 Token 操作方法
type Config struct {
	// 静态配置字段：外部直接通过 cfg.SyncPath 访问
	SyncPath    string `yaml:"sync_path"`
	StrmPath    string `yaml:"strm_path"`
	TempPath    string `yaml:"temp_path"`
	StrmUrl     string `yaml:"strm_url"`
	TorrentPath string `yaml:"torrent_path"` // 离线下载上传种子的云端临时目录；为空则用临时目录（temp_path），再空用根目录

	// 视频文件扩展名白名单：命中且体积达阈值的文件按「视频」处理
	// （上传后替换为 .strm 索引而非落地原文件）。为空时使用内置默认（见 DefaultVideoExts）。
	// 可通过配置文件或 Web 设置页修改，二者保持一致。
	VideoExts []string `yaml:"video_exts"`

	// 本地同步去抖窗口（秒）：监听事件后等待该时长内无新事件才执行同步，
	// 避免扫描/上传过程中其他程序仍在修改文件造成竞态。0 表示使用默认 5 秒。
	// 上限 10 秒，防止窗口过长导致本地变更迟迟不生效。
	DebounceSeconds int `yaml:"debounce_seconds"`

	// Cron 定时全量同步配置（嵌套段，YAML 键 cron）。
	// 旧版用顶层 cron_enabled / cron_interval_hours，现已统一到本段，不再兼容旧字段。
	Cron CronConfig `yaml:"cron"`

	// Auth 前端管理页登录凭据；Username 为空表示关闭登录验证。
	// 密码仅以 bcrypt 哈希存储（PasswordHash），绝不保存明文。
	// /download 直链接口始终不做验证（供 Emby 使用）。
	Auth AuthConfig `yaml:"auth"`

	// 内部私有属性
	path  string
	mu    sync.RWMutex
	token tokenData
}

// DefaultVideoExts 视频文件扩展名内置默认白名单（常见视频格式）。
// 与 syncFile/core.DefaultVideoExts 保持一致——配置未显式设置 video_exts 时使用它，
// 运行期也可由配置覆盖（见 settings.Update / core.NewEnv）。
var DefaultVideoExts = []string{
	".mp4", ".mkv", ".avi", ".mov", ".ts", ".flv", ".wmv",
	".m4v", ".mpg", ".mpeg", ".webm", ".rmvb", ".3gp", ".vob",
}

// AuthConfig 前端登录的账号密码；密码仅以 bcrypt 哈希存储。
type AuthConfig struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

// CronConfig 定时全量同步配置（嵌套段，YAML 键 cron）。
// 旧版用顶层 cron_enabled / cron_interval_hours，现已统一到本段，不再兼容旧字段。
// Enabled 用 *bool：nil 表示未显式设置，按「默认开启」处理；
// 只有显式写 enabled: false（面板取消勾选或手动 YAML）才是真正关闭，
// 确保「默认开启」与「用户能关掉」两个语义同时成立。
type CronConfig struct {
	Enabled       *bool `yaml:"enabled"`        // 是否启用；nil = 未设置，默认开启
	IntervalHours int   `yaml:"interval_hours"` // 间隔（小时），0 表示默认 12
}

type tokenData struct {
	AccessToken  string    `yaml:"access_token"`
	RefreshToken string    `yaml:"refresh_token"`
	ExpireAt     time.Time `yaml:"expire_at"`
}

// New 读取并解析配置文件。
//
// 与旧版不同：本函数不再因「缺少必填项」而报错——缺失由 IsSyncReady 判定，
// 缺失时只阻止同步启动、不影响程序与面板运行（前端会提示用户补齐）。
//
// 文件不存在时自动生成一份模板（字段全空、含注释），随后重新读入返回，不致命退出；
// 仅当文件存在但读取/解析失败时返回 error（属于真实损坏，需用户介入）。
func New(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 配置文件缺失：生成模板供用户在面板填写，降级继续运行而非退出。
			if genErr := writeTemplate(path); genErr != nil {
				return nil, fmt.Errorf("生成配置文件模板失败: %w", genErr)
			}
			slog.Warn("[CONFIG] 配置文件不存在，已生成模板，请在管理面板填写后保存以启动同步", "路径", path)
			data, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("读取生成的配置文件失败: %w", err)
			}
		} else {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	var tmp struct {
		Config `yaml:",inline"`
		Token  tokenData `yaml:"token"`
	}
	if err := yaml.Unmarshal(data, &tmp); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 定时全量同步配置已由嵌套 cron 段统一承载，不再兼容旧顶层字段。
	// 间隔兜底：IntervalHours <= 0 时使用默认 12 小时（避免 0 触发即时死循环）。
	// Enabled 不在此兜底——缺省（nil）由 CronEnabled() 方法按「默认开启」处理，
	// 只有显式 enabled: false 才真正关闭（见 CronConfig 注释）。
	if tmp.Config.Cron.IntervalHours <= 0 {
		tmp.Config.Cron.IntervalHours = 12
	}

	// 兼容旧配置：旧版用 settle_seconds，新版用 debounce_seconds。
	// 当配置文件里没显式写 debounce_seconds 但写了 settle_seconds 时，回退使用旧值。
	var rawSettle struct {
		SettleSeconds int `yaml:"settle_seconds"`
	}
	_ = yaml.Unmarshal(data, &rawSettle)
	if tmp.Config.DebounceSeconds == 0 && rawSettle.SettleSeconds != 0 {
		tmp.Config.DebounceSeconds = rawSettle.SettleSeconds
	}

	// 视频扩展名白名单：配置文件未显式写 video_exts 时回退内置默认。
	if len(tmp.Config.VideoExts) == 0 {
		tmp.Config.VideoExts = append([]string(nil), DefaultVideoExts...)
	}

	cfg := &tmp.Config
	cfg.path = path
	cfg.token = tmp.Token
	return cfg, nil
}

// templateConfig 是自动生成的配置文件模板内容（字段全空、含注释）。
// 字段名/缩进与 persistLocked 的 marshal 产物（*Config inline + token）对齐，
// 避免用户首次保存后产生无谓的字段顺序 diff。
const templateConfig = `# 115tools 配置文件（自动生成模板）
# 可直接在此编辑后重启，或通过 Web 管理面板填写。
# 注意：token 中仅 refresh_token 需手动填写；access_token / expire_at 由程序自动刷新写入。

sync_path: ""
strm_path: ""
temp_path: ""
strm_url: ""
torrent_path: ""

# 视频文件扩展名白名单（命中且体积达阈值按视频处理，生成 .strm 而非下载原文件）。
# 留空则使用内置默认；亦可在 Web 设置页修改。删除此段即恢复内置默认。
video_exts:
  - .mp4
  - .mkv
  - .avi
  - .mov
  - .ts
  - .flv
  - .wmv
  - .m4v
  - .mpg
  - .mpeg
  - .webm
  - .rmvb
  - .3gp
  - .vob

# 本地同步去抖窗口（秒）：监听事件后等待该时长无新事件再同步；0 表示默认 5 秒（上限 10）
debounce_seconds: 0

# 定时全量同步：开启后每 interval_hours 小时做一次全量扫描
# （兜底文件监听可能漏掉的本地变化 + 云端全量同步）。关闭则仅依赖本地文件监听
cron:
  enabled: true
  interval_hours: 12

# 管理面板登录：username 留空表示关闭登录验证（仅内网安全时使用）
auth:
  username: ""
  password_hash: ""

token:
  access_token: ""
  refresh_token: ""
  expire_at: "0001-01-01T00:00:00Z"
`

// writeTemplate 将模板写入指定路径（目录需已存在，由部署挂载保证）。
func writeTemplate(path string) error {
	return os.WriteFile(path, []byte(templateConfig), 0644)
}

// RequiredMissing 返回缺失的必填项（用于前端提示与启动决策）。
// 仅 refresh_token 是用户必须提供的 token；access_token / expire_at 由程序
// 首次刷新时自动写入，不计入必填。路径四项是同步器启动的必要条件。
func (c *Config) RequiredMissing() []string {
	var miss []string
	if c.token.RefreshToken == "" {
		miss = append(miss, "refresh_token")
	}
	if c.SyncPath == "" {
		miss = append(miss, "sync_path")
	}
	if c.StrmPath == "" {
		miss = append(miss, "strm_path")
	}
	if c.TempPath == "" {
		miss = append(miss, "temp_path")
	}
	if c.StrmUrl == "" {
		miss = append(miss, "strm_url")
	}
	return miss
}

// CronEnabled 返回定时全量同步是否启用：未显式设置（Enabled 为 nil）按「默认开启」处理，
// 仅显式 enabled: false 才返回 false（用户可在面板取消勾选真正关闭）。
func (c *Config) CronEnabled() bool {
	return c.Cron.Enabled == nil || *c.Cron.Enabled
}

// CronInterval 返回定时全量同步间隔；Cron.IntervalHours <= 0 时回退默认 12 小时。
func (c *Config) CronInterval() time.Duration {
	if c.Cron.IntervalHours <= 0 {
		return 12 * time.Hour
	}
	return time.Duration(c.Cron.IntervalHours) * time.Hour
}

// IsSyncReady 配置是否已足以启动同步器。
func (c *Config) IsSyncReady() bool {
	return len(c.RequiredMissing()) == 0
}

func (c *Config) GetAccessToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token.AccessToken
}

func (c *Config) GetRefreshToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token.RefreshToken
}

func (c *Config) GetExpireAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token.ExpireAt
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
		slog.Error("[CONFIG] Token 写盘失败，内存已更新但未落盘，重启后可能读到旧 Token", "错误信息", err)
		return
	}

	// 显示直观的到期时间日志
	slog.Info("[CONFIG] Token 已更新", "到期时间", expireAt.Format("2006-01-02 15:04:05"))
}

// persistLocked 序列化并写盘，调用方必须已持有 c.mu 写锁。
func (c *Config) persistLocked() error {
	out, err := yaml.Marshal(struct {
		*Config `yaml:",inline"`
		Token   tokenData `yaml:"token"`
	}{c, c.token})
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	return os.WriteFile(c.path, out, 0644)
}
