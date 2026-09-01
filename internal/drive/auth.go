package drive

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrRefreshTokenExpired 表示 refresh_token 本身已在 115 侧失效（典型 code 40140137，
// 提示「请停止重试并重新授权」）。这是永久性错误，重试无意义，必须用户重新填写 refresh_token。
// 与网络抖动 / access token 过期（refresh token 仍有效）区分开。
var ErrRefreshTokenExpired = errors.New("refresh_token 已失效，请重新授权")

// refreshTokenDeadCode 115 返回的 refresh_token 失效业务码（401 开头，Is401Started 同类）。
const refreshTokenDeadCode = int64(40140137)

// isRefreshTokenDead 判定刷新响应是否表示 refresh_token 本身已失效、需重新授权。
func isRefreshTokenDead(code int64, msg string) bool {
	if code == refreshTokenDeadCode {
		return true
	}
	m := strings.ToLower(msg)
	return strings.Contains(m, "重新授权") || strings.Contains(m, "已失效")
}

// ──── 懒刷新（无常驻守护） ────
//
// 设计说明：access token 只在发请求时才有影响，因此刷新只在请求路径上按需触发
// （Client.request 的请求前保活 + 401 回退，以及 Verify 校验新 token）。不设常驻守护。
// 刷新失败按严重程度退避，退避期内非强制刷新直接跳过（用旧 token 碰运气，收到 401 自然走强制刷新）。
//
// refreshMu     串行化所有刷新：请求路径可能并发触发，共享锁避免重复刷新竞态。
// nextAllowed   刷新失败后的最早允许时刻（零值 = 不限制）；成功刷新复位。
// deadReported   已提示过「refresh_token 失效」去重；成功刷新复位。

var (
	refreshMu    sync.Mutex
	nextAllowed  time.Time
	deadReported atomic.Bool
)

// 刷新节流与退避参数：
//   - refreshAhead：距过期不足该值才刷新（提前刷新，避免请求带着将过期的 token 撞失败）；
//   - retryBackoff：一般失败（网络抖动 / 限流）退避 1 分钟；
//   - deadBackoff：refresh_token 本身失效后放慢为 10 分钟轮询，尊重 115「请停止重试」，
//     用户重新填写 token 后下一次刷新成功即自动恢复。
const (
	refreshAhead = 10 * time.Minute
	retryBackoff = time.Minute
	deadBackoff  = 10 * time.Minute
)

// refreshToken 刷新访问令牌。
//
// force=true 绕过节流立即刷新（401 回退重试、Verify 校验新 token 用）；
// overrideRT 非空强制用该 rt（UI 保存新 token 场景），空走存储值。
func (c *Client) refreshToken(ctx context.Context, force bool, overrideRT string) error {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}

	rt := c.cfg.Token().RefreshToken
	if overrideRT != "" {
		rt = overrideRT
	}
	if rt == "" {
		return nil // 无可刷（未配置 refresh_token），让上层走「凭证缺失」路径
	}

	// 非强制：token 仍新鲜或正在退避 → 跳过，用旧 token 碰运气
	if !force && time.Until(c.cfg.Token().ExpireAt) > refreshAhead {
		return nil
	}
	if !force && !time.Now().After(nextAllowed) {
		return nil
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
		nextAllowed = time.Now().Add(retryBackoff)
		return fmt.Errorf("网络请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // 只读响应体，关闭失败无补救动作
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		nextAllowed = time.Now().Add(retryBackoff)
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
		nextAllowed = time.Now().Add(retryBackoff)
		return fmt.Errorf("解析刷新响应失败: %w", err)
	}
	if res.State != 1 {
		err := fmt.Errorf("刷新失败: message: %s code: %d", res.Message, res.Code)
		if isRefreshTokenDead(res.Code, res.Message) {
			// refresh_token 已死，115 明确「请停止重试并重新授权」。升级为系统横幅
			// （同周期仅一次），让用户去设置重新填写 refresh_token；刷新会放慢轮询而非狂刷。
			nextAllowed = time.Now().Add(deadBackoff)
			if deadReported.CompareAndSwap(false, true) {
				slog.ErrorContext(ctx, "刷新令牌已失效，请在「设置」重新填写 refresh_token 并保存",
					"消息", res.Message, "code", res.Code)
			}
			return fmt.Errorf("%w: %v", ErrRefreshTokenExpired, err)
		}
		nextAllowed = time.Now().Add(retryBackoff)
		return err
	}
	nextAllowed = time.Time{}
	deadReported.Store(false) // 刷新成功（用户已重新授权）→ 复位，便于下次失效再提示
	rt = res.Data.RefreshToken
	if rt == "" {
		// 115 偶不下发新 refresh_token：优先沿用刚粘贴的 overrideRT（覆盖写入场景），
		// 否则会回退到可能已失效的旧存储值，导致下次刷新失败。
		if overrideRT != "" {
			rt = overrideRT
		} else {
			rt = c.cfg.Token().RefreshToken
		}
	}
	if err := c.cfg.SaveToken(res.Data.AccessToken, rt, res.Data.ExpiresIn); err != nil {
		return fmt.Errorf("保存令牌失败: %w", err)
	}
	return nil
}

// Verify 验证凭证并返回账户概况：overrideRT 为空验证当前 token；非空则用该 rt 刷新一次。
func (c *Client) Verify(ctx context.Context, overrideRT string) (*UserInfo, error) {
	if overrideRT != "" {
		if err := c.refreshToken(ctx, true, overrideRT); err != nil {
			return nil, err
		}
	}
	return c.GetUserInfo(ctx)
}
