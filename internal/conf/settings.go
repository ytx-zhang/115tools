package conf

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Editable 是前端可读写的全局设置 DTO。密码与 refresh_token 遵循「空 = 保持不变」语义，
// 快照输出时绝不回显明文（AuthPassword 恒空、RefreshToken 恒空、用 HasRefreshToken 标志是否已配置）。
type Editable struct {
	StrmURL            string   `json:"strm_url"`
	TempDir            string   `json:"temp_dir"`
	CacheDir           string   `json:"cache_dir"`
	OfflineDir         string   `json:"offline_dir"`
	CacheRetentionDays int      `json:"cache_retention_days"`
	VideoExts          []string `json:"video_exts"`
	UploadExclude      []string `json:"upload_exclude"`
	AuthUsername       string   `json:"auth_username"`
	AuthPassword       string   `json:"auth_password,omitempty"`
	RefreshToken       string   `json:"refresh_token,omitempty"`
	HasRefreshToken    bool     `json:"has_refresh_token,omitempty"`
}

// CacheDir 返回缓存目录（轻量读，供保存设置时对比变更，避免构建整份快照）。
func (c *Config) CacheDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Settings.CacheDir
}

// Snapshot 返回全局设置快照（不回显密码明文与 refresh_token 明文）。
func (c *Config) Snapshot() Editable {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Editable{
		StrmURL:            c.Settings.StrmURL,
		TempDir:            c.Settings.TempDir,
		CacheDir:           c.Settings.CacheDir,
		OfflineDir:         c.Settings.OfflineDir,
		CacheRetentionDays: c.Settings.CacheRetentionDays,
		VideoExts:          slices.Clone(c.Settings.VideoExts),
		UploadExclude:      slices.Clone(c.Settings.UploadExclude),
		AuthUsername:       c.Settings.Auth.Username,
		HasRefreshToken:    c.token.RefreshToken != "",
	}
}

// Update 应用全局设置并落盘。密码/refresh_token 留空表示保持不变。
// 返回需要触发引擎重建的信息由调用方（app 层）根据变更自行判断。
func (c *Config) Update(e Editable) error {
	if e.AuthUsername != "" && e.AuthPassword == "" {
		if u, _ := c.GetAuth(); u == "" {
			return fmt.Errorf("启用登录验证时必须设置密码")
		}
	}

	newHash, err := hashPasswordIfChanged(e.AuthUsername, e.AuthPassword)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.Settings.StrmURL = e.StrmURL
	c.Settings.TempDir = e.TempDir
	c.Settings.CacheDir = e.CacheDir
	c.Settings.OfflineDir = e.OfflineDir
	c.Settings.CacheRetentionDays = e.CacheRetentionDays
	c.Settings.VideoExts = normalizeExts(e.VideoExts, true)
	c.Settings.UploadExclude = normalizeExts(e.UploadExclude, false)

	switch {
	case e.AuthUsername == "":
		c.Settings.Auth = Auth{}
	case newHash != "":
		c.Settings.Auth = Auth{Username: e.AuthUsername, PasswordHash: newHash}
	default:
		c.Settings.Auth.Username = e.AuthUsername
	}
	if e.RefreshToken != "" {
		c.token.RefreshToken = e.RefreshToken
	}

	c.normalizeLocked()
	return c.persistLocked()
}

// GetAuth 返回登录凭据（username, bcrypt 哈希）。
func (c *Config) GetAuth() (string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Settings.Auth.Username, c.Settings.Auth.PasswordHash
}

// AuthRequired 是否需要登录验证。
func (c *Config) AuthRequired() bool {
	u, _ := c.GetAuth()
	return u != ""
}

// hashPasswordIfChanged 仅在提供了新密码时生成 bcrypt 哈希；空字符串表示无变更。
func hashPasswordIfChanged(username, password string) (string, error) {
	if username == "" || password == "" {
		return "", nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("密码哈希失败: %w", err)
	}
	return string(hash), nil
}

// normalizeExts 清洗扩展名/排除名单：去空格、统一小写、去重；padDot 为 true 时补前导点。
// 空白名单（fillDefault 情形）的兜底由调用方处理：VideoExts 空回退默认，UploadExclude 空=不排除。
func normalizeExts(in []string, padDot bool) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if padDot && !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}
