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
	SyncPath string `yaml:"sync_path"`
	StrmPath string `yaml:"strm_path"`
	TempPath string `yaml:"temp_path"`
	StrmUrl  string `yaml:"strm_url"`

	// 内部私有属性
	path  string
	mu    sync.RWMutex
	token tokenData
}

type tokenData struct {
	AccessToken  string    `yaml:"access_token"`
	RefreshToken string    `yaml:"refresh_token"`
	ExpireAt     time.Time `yaml:"expire_at"`
}

// New 初始化并执行必须字段检查
func New(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var tmp struct {
		Config `yaml:",inline"`
		Token  tokenData `yaml:"token"`
	}

	if err := yaml.Unmarshal(data, &tmp); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	cfg := &tmp.Config
	cfg.path = path
	cfg.token = tmp.Token

	// 执行必须字段检查
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validate 检查核心参数是否为空
func (c *Config) validate() error {
	if c.token.RefreshToken == "" {
		return fmt.Errorf("配置错误: token.refresh_token 不能为空")
	}
	if c.SyncPath == "" {
		return fmt.Errorf("配置错误: sync_path 不能为空")
	}
	if c.StrmPath == "" {
		return fmt.Errorf("配置错误: strm_path 不能为空")
	}
	if c.TempPath == "" {
		return fmt.Errorf("配置错误: temp_path 不能为空")
	}
	if c.StrmUrl == "" {
		return fmt.Errorf("配置错误: strm_url 不能为空")
	}
	return nil
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

	// 序列化并存盘
	out, _ := yaml.Marshal(struct {
		*Config `yaml:",inline"`
		Token   tokenData `yaml:"token"`
	}{c, c.token})

	_ = os.WriteFile(c.path, out, 0644)

	// 显示直观的到期时间日志
	slog.Info("[CONFIG] Token 已更新", "到期时间", expireAt.Format("2006-01-02 15:04:05"))
}
