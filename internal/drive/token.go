package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// 本文件负责 115 访问令牌（AccessToken）的自动刷新与登录凭证验证。
//
// 机制：AccessToken 有有效期（通常几小时）。守护基于 time.AfterFunc 链式调度——
// StartRefreshDaemon 初始化按配置里的到期时间提前 refreshAhead 排定首次刷新，
// scheduleRefresh 每次刷新成功后按新到期时间续排、失败则退避 refreshBackoff，
// 形成永不断裂的链；ctx 取消时回调开头检查 ctx.Err() 直接终止，不再续排。
// 业务请求侧（Client 的 before 钩子）发现过期时也走同一刷新函数，由包级 refreshMu
// 串行化，避免并发重复刷新。⚠️ 检查节流阈值与预约阈值必须同为 refreshAhead。

// refreshMu 全局串行化所有 token 刷新：请求路径与常驻守护可能并发，
// 共享同一把锁避免重复刷新竞态（SaveToken 落盘由 config 内部锁保证安全）。
var refreshMu sync.Mutex

// refreshAhead 是 token 提前刷新的时间窗口：距过期不足该值时才刷新，
// 刷新成功后也按同一窗口预约下一次，保证预约点总在检查点之前。
const refreshAhead = 10 * time.Minute

// refreshBackoff 是刷新失败后的退避间隔：token 失效/被限频时按此节奏温和重试，
// 既不让错误无限堆积，也不高频打 115 端「刷新过于频繁」限频。
const refreshBackoff = time.Minute

// refreshAccessToken 包级刷新实现：cfg 指定读写哪个配置（守护与 client 实例共用）。
// overrideRT 非空时强制用该 rt 刷新（web 改 token 校验场景），空串走正常节流。
// 并发安全：包级 refreshMu 保证多个并发请求/守护同时发现过期时只真正刷新一次。
func refreshAccessToken(ctx context.Context, cfg *config.Config, overrideRT string) error {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	// rt 默认读当前配置；传入 overrideRT（web 改 refresh_token 的校验场景）时以传入值为准
	rt := cfg.Token().RefreshToken
	if overrideRT != "" {
		rt = overrideRT
	}

	// 无覆盖值时走正常节流：token 距过期仍 >refreshAhead 直接返回，避免每次请求都刷新
	if overrideRT == "" && time.Until(cfg.Token().ExpireAt) > refreshAhead {
		return nil // token 还很新鲜，无需刷新
	}

	// 为 token 刷新请求设置独立的超时，防止无响应时永久阻塞 refreshMu 锁
	reqCtx, reqCancel := context.WithTimeout(ctx, 30*time.Second)
	defer reqCancel()

	form := url.Values{
		"refresh_token": {rt},
	}

	req, err := http.NewRequestWithContext(reqCtx, "POST", "https://passportapi.115.com/open/refreshToken", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("网络请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var res struct {
		State   IntString `json:"state"` // ⚠️ 115 整数字段偶发为字符串，双兼容（见 IntString）
		Code    IntString `json:"code"`
		Message string    `json:"message"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			ExpiresIn    int64  `json:"expires_in"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	if res.State != 1 {
		return fmt.Errorf("刷新失败: message: %s code: %d", res.Message, res.Code)
	}
	rt = res.Data.RefreshToken
	if rt == "" {
		rt = cfg.Token().RefreshToken
	}
	cfg.SaveToken(res.Data.AccessToken, rt, res.Data.ExpiresIn)
	return nil
}

// nextRefreshDelay 返回距「到期 - refreshAhead」的时长；已过期时返回 0（立即触发）。
func nextRefreshDelay(cfg *config.Config) time.Duration {
	if delay := time.Until(cfg.Token().ExpireAt.Add(-refreshAhead)); delay > 0 {
		return delay
	}
	return 0
}

// Verify 统一验证入口（返回账户概况 UserInfo 供调用方打印）：
//   - overrideRT 为空：验证当前已配置 token——调 GetUserInfo（/open/user/info 需有效
//     Bearer，天然验证 token 有效性），token 新鲜时不会触发刷新；
//   - overrideRT 非空：用该 rt 试刷新一次（web 改 refresh_token 的校验场景），成功则
//     把 115 返回的新 access_token（及可能轮换的新 refresh_token）持久化到配置，再拉账户概况。
//
// SyncPath 云端可达性不再在此提前校验，交由后续首次扫描（scanDir）自然兜底。
func (c *Client) Verify(ctx context.Context, overrideRT string) (*UserInfo, error) {
	if overrideRT != "" {
		if err := refreshAccessToken(ctx, c.cfg, overrideRT); err != nil {
			return nil, err
		}
	}
	return c.GetUserInfo(ctx)
}

// StartRefreshDaemon 启动常驻刷新守护：按配置里的到期时间排定首次刷新。
// 只要配置中存在 refresh_token 就持续在后台刷新，防止 refresh_token 长期闲置被 115
// 判过期；未配置时按 refreshBackoff 空转等待（不发请求、不打日志），配置更新后自动恢复。
// ctx 取消（应用退出）时链条在下次触发时自然终止。
func StartRefreshDaemon(ctx context.Context, cfg *config.Config) {
	scheduleRefresh(ctx, cfg, nextRefreshDelay(cfg))
}

// scheduleRefresh 在 delay 后触发一次刷新，触发后按结果链式排下一次。
func scheduleRefresh(ctx context.Context, cfg *config.Config, delay time.Duration) {
	time.AfterFunc(delay, func() {
		if ctx.Err() != nil {
			return // 应用退出，终止刷新链
		}
		// 未配置 refresh_token：按 refreshBackoff 空转等待（不发请求、不打日志），
		// 配置更新后自动恢复刷新。
		if cfg.Token().RefreshToken == "" {
			scheduleRefresh(ctx, cfg, refreshBackoff)
			return
		}
		if err := refreshAccessToken(ctx, cfg, ""); err != nil {
			logs.Warn(logs.ModuleCloud, "刷新失败，将退避重试", "错误", err, "退避", refreshBackoff.String())
			scheduleRefresh(ctx, cfg, refreshBackoff) // 失败：1 分钟退避
			return
		}
		// 成功（或 token 仍新鲜被节流跳过）：按新到期时间重排下一次。
		// 云端分类日志：仅显示 token 已更新与真实到期时间（系统分类不打此日志）。
		logs.Info(logs.ModuleCloud, fmt.Sprintf("Token 已更新  到期时间=%s",
			cfg.Token().ExpireAt.Format("2006-01-02 15:04:05")))
		scheduleRefresh(ctx, cfg, nextRefreshDelay(cfg))
	})
}
