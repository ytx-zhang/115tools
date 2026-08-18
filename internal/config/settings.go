package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ytx-zhang/115tools/internal/logs"
	"golang.org/x/crypto/bcrypt"
)

// ConfigStatus 返回配置就绪状态，供初始化步骤与 SSE 推送前端。
type ConfigStatus struct {
	Ready   bool     `json:"ready"`
	Missing []string `json:"missing"`
}

// Editable 是前端可以查看/修改的配置字段集合（JSON 传输用）。
// AuthPassword 在 Snapshot 输出时恒为空（不回传密码）；
// Update 时留空表示保持原密码不变。
type Editable struct {
	SyncPath        string   `json:"sync_path"`
	StrmPath        string   `json:"strm_path"`
	TempPath        string   `json:"temp_path"`
	StrmUrl         string   `json:"strm_url"`
	DebounceMinutes int      `json:"debounce_minutes"`
	Cron            CronJSON `json:"cron"`
	AuthUsername    string   `json:"auth_username"`
	AuthPassword    string   `json:"auth_password,omitempty"`
	// RefreshToken：快照只回显 has_refresh_token（绝不回显明文）；
	// 保存时若非空则用新值校验并替换，空表示保持不变。
	RefreshToken    string `json:"refresh_token,omitempty"`
	HasRefreshToken bool   `json:"has_refresh_token,omitempty"` // 仅 Snapshot 输出

	// VideoExts 视频文件扩展名白名单（命中且体积达阈值按视频处理）。
	// 快照回显当前生效值（未设置则为内置默认）；保存时按用户输入覆盖。
	VideoExts []string `json:"video_exts"`

	// UploadExclude 上传排除名单（下载器/系统临时文件后缀；整名如 .DS_Store 也支持）。
	// 快照回显当前生效值（未设置则为空=不排除任何文件）；保存时按用户输入覆盖。
	UploadExclude []string `json:"upload_exclude"`
}

// CronJSON 是定时全量同步配置的前端传输结构（用普通 bool，避免 JSON 出现 null）。
// 后端的 *bool 语义在 Snapshot/Update 处归一：nil → true（默认开启）。
type CronJSON struct {
	Enabled       bool `json:"enabled"`
	IntervalHours int  `json:"interval_hours"`
}

// missingLocked 返回缺失的必填项（调用方必须持有锁）。
func (c *Config) missingLocked() []string {
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

// Snapshot 返回当前可编辑配置的副本（不含密码明文，也不回显 refresh_token 明文）。
func (c *Config) Snapshot() Editable {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Editable{
		SyncPath:        c.SyncPath,
		StrmPath:        c.StrmPath,
		TempPath:        c.TempPath,
		StrmUrl:         c.StrmUrl,
		DebounceMinutes: c.DebounceMinutes,
		Cron: CronJSON{
			Enabled:       c.CronEnabled(), // 归一：nil 或 true → true，前端始终收到布尔
			IntervalHours: c.Cron.IntervalHours,
		},
		AuthUsername:    c.Auth.Username,
		HasRefreshToken: c.token.RefreshToken != "",
		VideoExts:       c.VideoExts,
		UploadExclude:   c.UploadExclude,
	}
}

// normalizeVideoExts 清洗扩展名白名单：去空格、统一小写、补前导点、
// 去空、去重；全空时回退内置默认（保证运行期不会因空白名单把一切判为非视频）。
func normalizeVideoExts(in []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	if len(out) == 0 {
		return slices.Clone(DefaultVideoExts)
	}
	return out
}

// normalizeUploadExclude 清洗上传排除名单：去空格、小写、去空、去重；
// 空输入即返回空名单（运行期空名单 = 不排除任何文件）。
// 不强制补前导点（如 .DS_Store 已是带点的整名）；匹配时文件名先 ToLower 再比。
func normalizeUploadExclude(in []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// GetAuth 返回登录凭据；username 为空表示未启用登录验证。
// 返回的 password 字段为 bcrypt 哈希，而非明文。
func (c *Config) GetAuth() (username, passwordHash string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Auth.Username, c.Auth.PasswordHash
}

// Update 应用并持久化配置（无校验，仅处理字段合并/token保留/密码哈希等）。
// 完整验证由 Initialize 流程中的 Check 负责。空字段（密码/token）表示保持原值不变。
func (c *Config) Update(e Editable) error {
	if e.AuthUsername != "" && e.AuthPassword == "" {
		if _, old := c.GetAuth(); old == "" {
			return fmt.Errorf("启用登录验证时必须设置密码")
		}
	}

	newHash, err := hashPasswordIfChanged(e)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.SyncPath = e.SyncPath
	c.StrmPath = e.StrmPath
	c.TempPath = e.TempPath
	c.StrmUrl = e.StrmUrl
	c.VideoExts = normalizeVideoExts(e.VideoExts)
	c.UploadExclude = normalizeUploadExclude(e.UploadExclude)
	c.DebounceMinutes = e.DebounceMinutes

	// cron：Enabled 取地址存 *bool（前端恒传布尔，显式 false 才是真正关闭）；
	// 间隔走 normalizeCronInterval 统一兜底（与加载/CronInterval 共用同一默认值）。
	enabled := e.Cron.Enabled
	c.Cron = CronConfig{Enabled: &enabled, IntervalHours: normalizeCronInterval(e.Cron.IntervalHours)}

	switch {
	case e.AuthUsername == "":
		c.Auth = AuthConfig{}
	case newHash != "":
		c.Auth = AuthConfig{Username: e.AuthUsername, PasswordHash: newHash}
	default:
		c.Auth.Username = e.AuthUsername
	}
	if e.RefreshToken != "" {
		c.token.RefreshToken = e.RefreshToken
	}

	if err := c.persistLocked(); err != nil {
		return fmt.Errorf("配置写盘失败: %w", err)
	}
	logs.Info(logs.ModuleSystem, "配置已更新")
	return nil
}

// hashPasswordIfChanged 在用户提供了新密码时才生成 bcrypt 哈希；
// 返回空字符串表示无密码变更。
func hashPasswordIfChanged(e Editable) (string, error) {
	if e.AuthUsername == "" || e.AuthPassword == "" {
		return "", nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(e.AuthPassword), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("密码哈希失败: %w", err)
	}
	return string(hash), nil
}
