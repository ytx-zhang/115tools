package config

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

func Get() *config {
	return conf.Load()
}

var (
	conf       atomic.Pointer[config]
	configPath string
)

type config struct {
	Token        tokenConfig `yaml:"token" json:"token"`
	SyncPath     string      `yaml:"sync_path" json:"sync_path"`
	StrmPath     string      `yaml:"strm_path" json:"strm_path"`
	TempPath     string      `yaml:"temp_path" json:"temp_path"`
	StrmUrl      string      `yaml:"strm_url" json:"strm_url"`
	EmbyUrl      string      `yaml:"emby_url" json:"emby_url"`
	FontInAssUrl string      `yaml:"fontinass_url" json:"fontinass_url"`
}
type tokenConfig struct {
	AccessToken  string    `yaml:"access_token" json:"access_token"`
	RefreshToken string    `yaml:"refresh_token" json:"refresh_token"`
	ExpireAt     time.Time `yaml:"expire_at" json:"expire_at"`
}

func LoadConfig(ctx context.Context, p string) error {
	configPath = p
	file, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("配置文件不存在或无法读取: %v", err)
	}

	var initialConf config
	if err := yaml.Unmarshal(file, &initialConf); err != nil {
		return fmt.Errorf("配置文件格式解析失败: %v", err)
	}

	if initialConf.Token.RefreshToken == "" {
		return fmt.Errorf("配置不完整，缺少 RefreshToken")
	}
	conf.Store(&initialConf)
	if initialConf.Token.ExpireAt.Unix() > 0 {
		slog.Info("[CONFIG] 配置已加载", "token有效期", initialConf.Token.ExpireAt.Format("2006-01-02 15:04:05"))
	}

	return nil
}
func StartRefresh(ctx context.Context) {
	for {
		success := refreshToken(ctx)
		duration := 1 * time.Minute
		if success {
			duration = 5 * time.Minute
		}
		select {
		case <-time.After(duration):
			continue
		case <-ctx.Done():
			slog.Info("Token 刷新任务已安全停止")
			return
		}
	}
}
func refreshToken(ctx context.Context) bool {
	if err := context.Cause(ctx); err != nil {
		return false
	}
	current := conf.Load()
	if current == nil || current.Token.RefreshToken == "" {
		slog.Error("[TOKEN] 刷新失败: 缺少 RefreshToken")
		os.Exit(1)
	}
	if time.Until(current.Token.ExpireAt) > 10*time.Minute {
		return true
	}
	form := url.Values{"refresh_token": {strings.TrimSpace(current.Token.RefreshToken)}}
	reqBody := strings.NewReader(form.Encode())
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://passportapi.115.com/open/refreshToken", reqBody)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("[TOKEN] 网络请求失败", "错误信息", err)
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	result := gjson.ParseBytes(body)
	data := result.Get("data")
	if result.Get("state").Int() == 1 {
		newConf := *current

		newConf.Token.AccessToken = data.Get("access_token").String()
		newConf.Token.ExpireAt = time.Now().Add(time.Duration(data.Get("expires_in").Int()) * time.Second)
		newRefreshToken := data.Get("refresh_token").String()
		if newRefreshToken != "" {
			newConf.Token.RefreshToken = newRefreshToken
		}

		conf.Store(&newConf)
		d, _ := yaml.Marshal(&newConf)
		_ = os.WriteFile(configPath, d, 0644)
		slog.Info("[TOKEN] 刷新成功!", "有效期至", newConf.Token.ExpireAt.Format("15:04:05"))
		return true
	}
	slog.Error("[TOKEN] 刷新失败", "接口消息", result.Get("message"))
	return false
}
