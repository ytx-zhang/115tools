package config

import (
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Editable 是前端可以查看/修改的配置字段集合（JSON 传输用）。
// AuthPassword 在 Snapshot 输出时恒为空（不回传密码）；
// Update 时留空表示保持原密码不变。
type Editable struct {
	SyncPath        string   `json:"sync_path"`
	StrmPath        string   `json:"strm_path"`
	TempPath        string   `json:"temp_path"`
	StrmUrl         string   `json:"strm_url"`
	TorrentPath     string   `json:"torrent_path"`
	DebounceSeconds int      `json:"debounce_seconds"`
	Cron            CronJSON `json:"cron"`
	AuthUsername    string   `json:"auth_username"`
	AuthPassword    string   `json:"auth_password,omitempty"`
	HasPassword     bool     `json:"has_password,omitempty"` // 仅 Snapshot 输出
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

	// ConfigReady / MissingFields 仅由 GET /api/config 返回（供前端展示配置完备状态），
	// PUT 请求中忽略这两个字段。
	ConfigReady   bool     `json:"config_ready,omitempty"`
	MissingFields []string `json:"missing_fields,omitempty"`
}

// CronJSON 是定时全量同步配置的前端传输结构（用普通 bool，避免 JSON 出现 null）。
// 后端的 *bool 语义在 Snapshot/Update 处归一：nil → true（默认开启）。
type CronJSON struct {
	Enabled       bool `json:"enabled"`
	IntervalHours int  `json:"interval_hours"`
}

// Snapshot 返回当前可编辑配置的副本（不含密码明文，也不回显 refresh_token 明文）。
func (c *Config) Snapshot() Editable {
	c.mu.RLock()
	defer c.mu.RUnlock()
	missing := c.RequiredMissing() // 算一次，ConfigReady 与 MissingFields 都从它派生
	return Editable{
		SyncPath:        c.SyncPath,
		StrmPath:        c.StrmPath,
		TempPath:        c.TempPath,
		StrmUrl:         c.StrmUrl,
		TorrentPath:     c.TorrentPath,
		DebounceSeconds: c.DebounceSeconds,
		Cron: CronJSON{
			Enabled:       c.CronEnabled(), // 归一：nil 或 true → true，前端始终收到布尔
			IntervalHours: c.Cron.IntervalHours,
		},
		AuthUsername:    c.Auth.Username,
		HasPassword:     c.Auth.PasswordHash != "",
		HasRefreshToken: c.token.RefreshToken != "",
		VideoExts:       c.VideoExts,
		UploadExclude:   c.UploadExclude,
		ConfigReady:     len(missing) == 0,
		MissingFields:   missing,
	}
}

// normalizeVideoExts 清洗用户输入的扩展名白名单：去空格、统一小写、补前导点、
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
		return append([]string(nil), DefaultVideoExts...)
	}
	return out
}

// normalizeUploadExclude 清洗用户输入的上传排除名单：去空格、小写、去空、去重；
// 空输入即返回空名单（运行期空名单 = 不排除任何文件）。
// 与 syncFile/core.normalizeUploadExclude 逻辑一致（config 不能 import core，故刻意重复）。
// 注意：不强制补前导点（如 .DS_Store 已是带点的整名）。匹配时文件名会先 ToLower 再比，
// 与已小写化的名单比较，大小写无关，无需原样保留大小写。
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

// Update 校验并应用新配置，落盘持久化。
// 返回 needReload 表示同步相关字段（路径/URL/静默窗口）发生变化，
// 调用方需要热重载同步器使其实时生效。
func (c *Config) Update(e Editable) (needReload bool, err error) {
	// 注意：不再强制 sync_path / strm_path / temp_path / strm_url 非空，
	// 允许先保存不完整配置（前端会提示缺失项、同步器暂不启动）；
	// 待用户在面板补齐后由 web 保存逻辑自动拉起同步器。
	if e.AuthUsername != "" && e.AuthPassword == "" {
		// 允许留空表示沿用旧密码，但旧密码也为空时必须设置
		if _, old := c.GetAuth(); old == "" {
			return false, fmt.Errorf("启用登录验证时必须设置密码")
		}
	}

	// 先完成可能失败的哈希计算，再进入变更区，避免出错时配置被改一半
	var newHash string
	if e.AuthUsername != "" && e.AuthPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(e.AuthPassword), bcrypt.DefaultCost)
		if err != nil {
			return false, fmt.Errorf("密码哈希失败: %w", err)
		}
		newHash = string(hash)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	needReload = e.SyncPath != c.SyncPath ||
		e.StrmPath != c.StrmPath ||
		e.TempPath != c.TempPath ||
		e.StrmUrl != c.StrmUrl ||
		e.DebounceSeconds != c.DebounceSeconds ||
		e.Cron.Enabled != c.CronEnabled() ||
		e.Cron.IntervalHours != c.Cron.IntervalHours

	c.SyncPath = e.SyncPath
	c.StrmPath = e.StrmPath
	c.TempPath = e.TempPath
	c.StrmUrl = e.StrmUrl
	c.TorrentPath = e.TorrentPath
	c.VideoExts = normalizeVideoExts(e.VideoExts)
	c.UploadExclude = normalizeUploadExclude(e.UploadExclude)
	c.DebounceSeconds = e.DebounceSeconds
	// 定时全量同步：用堆分配的 *bool 承载（避免局部变量取地址导致悬空指针）。
	// 前端始终提交明确 true/false，故此处直接按用户意图落盘；
	// 间隔 <=0 视为使用默认 12 小时，避免 0 触发即时死循环。
	p := new(bool)
	*p = e.Cron.Enabled
	c.Cron = CronConfig{Enabled: p, IntervalHours: e.Cron.IntervalHours}
	if c.Cron.IntervalHours <= 0 {
		c.Cron.IntervalHours = 12
	}

	switch {
	case e.AuthUsername == "":
		// 清空用户名 = 关闭登录验证
		c.Auth = AuthConfig{}
	case newHash != "":
		// 密码非空：以 bcrypt 哈希存储，绝不保存明文
		c.Auth = AuthConfig{Username: e.AuthUsername, PasswordHash: newHash}
	default:
		c.Auth.Username = e.AuthUsername // 密码留空沿用旧哈希
	}

	if err := c.persistLocked(); err != nil {
		return needReload, fmt.Errorf("配置写盘失败: %w", err)
	}
	slog.Info("[CONFIG] 配置已更新", "需要热重载", needReload)
	return needReload, nil
}
