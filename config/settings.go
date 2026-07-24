package config

import (
	"fmt"
	"log/slog"

	"golang.org/x/crypto/bcrypt"
)

// Editable 是前端可以查看/修改的配置字段集合（JSON 传输用）。
// AuthPassword 在 Snapshot 输出时恒为空（不回传密码）；
// Update 时留空表示保持原密码不变。
type Editable struct {
	SyncPath      string `json:"sync_path"`
	StrmPath      string `json:"strm_path"`
	TempPath      string `json:"temp_path"`
	StrmUrl       string `json:"strm_url"`
	TorrentPath   string `json:"torrent_path"`
	SettleSeconds int    `json:"settle_seconds"`
	AuthUsername  string `json:"auth_username"`
	AuthPassword  string `json:"auth_password,omitempty"`
	HasPassword   bool   `json:"has_password,omitempty"` // 仅 Snapshot 输出
	// RefreshToken：快照只回显 has_refresh_token（绝不回显明文）；
	// 保存时若非空则用新值校验并替换，空表示保持不变。
	RefreshToken    string `json:"refresh_token,omitempty"`
	HasRefreshToken bool   `json:"has_refresh_token,omitempty"` // 仅 Snapshot 输出
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
		TorrentPath:     c.TorrentPath,
		SettleSeconds:   c.SettleSeconds,
		AuthUsername:    c.Auth.Username,
		HasPassword:     c.Auth.PasswordHash != "",
		HasRefreshToken: c.token.RefreshToken != "",
	}
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
	if e.SyncPath == "" || e.StrmPath == "" || e.TempPath == "" || e.StrmUrl == "" {
		return false, fmt.Errorf("sync_path / strm_path / temp_path / strm_url 均不能为空")
	}
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
		e.SettleSeconds != c.SettleSeconds

	c.SyncPath = e.SyncPath
	c.StrmPath = e.StrmPath
	c.TempPath = e.TempPath
	c.StrmUrl = e.StrmUrl
	c.TorrentPath = e.TorrentPath
	c.SettleSeconds = e.SettleSeconds

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
