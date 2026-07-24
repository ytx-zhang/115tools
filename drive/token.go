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
//
// overrideRT 可选：传入非空 refresh_token 时以传入值为准（用于 web 改 token 的校验场景），
// 并跳过 5 分钟节流判断，强制立即用该 rt 试刷新一次。
func (d *Open115) refreshToken(ctx context.Context, overrideRT ...string) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	// rt 默认读当前配置；传入 overrideRT（web 改 refresh_token 的校验场景）时以传入值为准
	rt := d.cfg.GetRefreshToken()
	if len(overrideRT) > 0 && overrideRT[0] != "" {
		rt = overrideRT[0]
	}

	// 无覆盖值时走正常节流：token 距过期仍 >5 分钟直接返回，避免每次请求都刷新
	if len(overrideRT) == 0 && time.Until(d.cfg.GetExpireAt()) > 5*time.Minute {
		return nil // token 还很新鲜，无需刷新
	}

	// 为 token 刷新请求设置独立的超时，防止无响应时永久阻塞 refreshMu 锁
	reqCtx, reqCancel := context.WithTimeout(ctx, 30*time.Second)
	defer reqCancel()

	form := url.Values{
		"refresh_token": {rt},
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

// VerifyAndApplyRefreshToken 用给定的 refresh_token 试刷新一次：成功则把 115 返回的
// 新 access_token（及可能轮换的新 refresh_token）持久化到配置；失败返回错误且不改动配置。
// 用于 web 设置页修改 refresh_token：保存前先校验，避免把无效 token 写盘导致后续刷新全挂。
func (d *Open115) VerifyAndApplyRefreshToken(ctx context.Context, rt string) error {
	if rt == "" {
		return fmt.Errorf("refresh_token 不能为空")
	}
	return d.refreshToken(ctx, rt)
}
