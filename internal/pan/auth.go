package pan

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/journal"
)

// refreshMu 全局串行化所有 token 刷新：请求路径与常驻守护可能并发，共享锁避免重复刷新竞态。
var refreshMu sync.Mutex

// refreshDaemonOnce 保证刷新守护只拉起一次：bootstrap 与 UI 保存配置（首次完备）都可能触发，
// 重复调用会导致两份守护各自按到期调度、重复刷新，故用 Once 收敛。
var refreshDaemonOnce sync.Once

// refreshAhead 提前刷新窗口：距过期不足该值才刷新，刷新后按同一窗口预约下一次。
const refreshAhead = 10 * time.Minute

// refreshBackoff 刷新失败后退避间隔。
const refreshBackoff = time.Minute

// refreshAccessToken 刷新令牌：overrideRT 非空强制用该 rt（改 token 校验场景），空走正常节流。
func refreshAccessToken(ctx context.Context, cfg *conf.Config, overrideRT string) error {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}

	rt := cfg.Token().RefreshToken
	if overrideRT != "" {
		rt = overrideRT
	}
	if overrideRT == "" && time.Until(cfg.Token().ExpireAt) > refreshAhead {
		return nil // token 仍新鲜，无需刷新
	}

	reqCtx, reqCancel := context.WithTimeout(ctx, 30*time.Second)
	defer reqCancel()

	form := url.Values{"refresh_token": {rt}}
	req, err := http.NewRequestWithContext(reqCtx, "POST", "https://passportapi.115.com/open/refreshToken", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("创建刷新请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("网络请求失败: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			journal.Debug(ctx, "关闭刷新响应体失败", "错误", cerr)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取刷新响应失败: %w", err)
	}
	var res struct {
		State   int64  `json:"state"` // refreshToken 接口 state 为 int（1=成功），区别于普通 API 的 bool
		Code    int64  `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			ExpiresIn    int64  `json:"expires_in"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("解析刷新响应失败: %w", err)
	}
	if res.State != 1 {
		return fmt.Errorf("刷新失败: message: %s code: %d", res.Message, res.Code)
	}
	rt = res.Data.RefreshToken
	if rt == "" {
		rt = cfg.Token().RefreshToken // 轮换机制：有时不下发新 rt，保留旧值
	}
	if err := cfg.SaveToken(res.Data.AccessToken, rt, res.Data.ExpiresIn); err != nil {
		return fmt.Errorf("保存令牌失败: %w", err)
	}
	return nil
}

// nextRefreshDelay 返回距「到期 - refreshAhead」的时长；已到期返回 0（立即触发）。
func nextRefreshDelay(cfg *conf.Config) time.Duration {
	if d := time.Until(cfg.Token().ExpireAt.Add(-refreshAhead)); d > 0 {
		return d
	}
	return 0
}

// Verify 验证凭证并返回账户概况：overrideRT 为空验证当前 token；非空则用该 rt 刷新一次。
func (c *Client) Verify(ctx context.Context, overrideRT string) (*UserInfo, error) {
	if overrideRT != "" {
		if err := refreshAccessToken(ctx, c.cfg, overrideRT); err != nil {
			return nil, err
		}
	}
	return c.GetUserInfo(ctx)
}

// StartRefreshDaemon 启动常驻刷新守护：按到期时间链式调度，ctx 取消时自然终止。
// 幂等：重复调用仅首次实际拉起（见 refreshDaemonOnce），供 bootstrap 与 UI 保存配置共用。
func StartRefreshDaemon(ctx context.Context, cfg *conf.Config) {
	refreshDaemonOnce.Do(func() {
		scheduleRefresh(ctx, cfg, nextRefreshDelay(cfg))
	})
}

// scheduleRefresh 在 delay 后触发一次刷新，按结果链式排下一次。
func scheduleRefresh(ctx context.Context, cfg *conf.Config, delay time.Duration) {
	time.AfterFunc(delay, func() {
		if context.Cause(ctx) != nil {
			return
		}
		if cfg.Token().RefreshToken == "" {
			scheduleRefresh(ctx, cfg, refreshBackoff)
			return
		}
		if err := refreshAccessToken(ctx, cfg, ""); err != nil {
			journal.Warn(ctx, "令牌刷新失败，将退避重试", "错误", err, "退避", refreshBackoff.String())
			scheduleRefresh(ctx, cfg, refreshBackoff)
			return
		}
		journal.Info(ctx, "令牌已更新", "到期时间", cfg.Token().ExpireAt.Format("2006-01-02 15:04:05"))
		scheduleRefresh(ctx, cfg, nextRefreshDelay(cfg))
	})
}
