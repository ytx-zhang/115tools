package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 本文件负责 115 访问令牌（AccessToken）的自动刷新。
//
// 机制：AccessToken 有有效期（通常几小时），到期前 5 分钟内发起的请求
// 会先触发刷新；刷新成功后按 expires_in 预约下一次刷新，失败则 1 分钟后重试，
// 全程对业务方透明。

// refreshToken 确保 AccessToken 有效：距过期超过 5 分钟直接返回；
// 否则用 RefreshToken 换新 token 并持久化到配置文件。
// 并发安全：refreshMu 保证多个并发请求同时发现过期时只真正刷新一次。
func (d *Open115) refreshToken(ctx context.Context) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if time.Until(d.cfg.GetExpireAt()) > 5*time.Minute {
		return nil // token 还很新鲜，无需刷新
	}

	// 为 token 刷新请求设置独立的超时，防止无响应时永久阻塞 refreshMu 锁
	reqCtx, reqCancel := context.WithTimeout(ctx, 30*time.Second)
	defer reqCancel()

	form := url.Values{
		"refresh_token": {d.cfg.GetRefreshToken()},
	}

	// fail 记录刷新失败并安排短间隔重试，避免 token 过期前再无机会刷新。
	fail := func(format string, a ...any) error {
		err := fmt.Errorf(format, a...)
		slog.Warn("[TOKEN] 刷新失败，将短间隔重试", "错误信息", err)
		time.AfterFunc(time.Minute, func() {
			_ = d.refreshToken(context.Background())
		})
		return err
	}

	req, err := http.NewRequestWithContext(reqCtx, "POST", "https://passportapi.115.com/open/refreshToken", strings.NewReader(form.Encode()))
	if err != nil {
		return fail("[TOKEN] 创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return fail("[TOKEN] 网络请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var res struct {
		State   int    `json:"state"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			ExpiresIn    int64  `json:"expires_in"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fail("[TOKEN] 解析响应失败: %w", err)
	}
	if res.State == 1 {
		rt := res.Data.RefreshToken
		if rt == "" {
			rt = d.cfg.GetRefreshToken()
		}
		d.cfg.SaveToken(res.Data.AccessToken, rt, res.Data.ExpiresIn)
		// 预约下一次刷新：到期前 10 分钟
		nextDelay := max(time.Duration(res.Data.ExpiresIn)*time.Second-10*time.Minute, time.Second)
		time.AfterFunc(nextDelay, func() {
			_ = d.refreshToken(context.Background())
		})
		return nil
	}
	return fail("[TOKEN] 刷新失败: message: %s code: %d", res.Message, res.Code)
}
