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
// 机制：AccessToken 有有效期（通常几小时）。进程启动时由常驻守护 scheduleRefresh
// 按当前到期时间恒定提前 refreshAhead 排定下一次刷新，且每次触发后无条件重排，
// 保证守护链永不断裂、不依赖业务请求被动触发。请求侧 refreshToken 仍保留
// 距过期 ≤refreshAhead 的兜底刷新（防守护定时器极端延迟晚点），二者阈值一致不冲突。
// ⚠️ 检查节流阈值与预约阈值必须同为 refreshAhead，否则预约点若晚于检查点，
// 会在到期前 5~10 分钟窗口反复触发刷新拖慢请求。

// refreshAhead 是 token 提前刷新的时间窗口：距过期不足该值时才刷新，
// 刷新成功后也按同一窗口预约下一次，保证预约点总在检查点之前。
const refreshAhead = 10 * time.Minute

// refreshToken 确保 AccessToken 有效：距过期超过 refreshAhead 直接返回；
// 否则用 RefreshToken 换新 token 并持久化到配置文件。
// 并发安全：refreshMu 保证多个并发请求同时发现过期时只真正刷新一次。
//
// overrideRT 可选：传入非空 refresh_token 时以传入值为准（用于 web 改 token 的校验场景），
// 并跳过节流判断，强制立即用该 rt 试刷新一次。
func (d *Open115) refreshToken(ctx context.Context, overrideRT ...string) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	// rt 默认读当前配置；传入 overrideRT（web 改 refresh_token 的校验场景）时以传入值为准
	rt := d.cfg.Token().RefreshToken
	if len(overrideRT) > 0 && overrideRT[0] != "" {
		rt = overrideRT[0]
	}

	// 无覆盖值时走正常节流：token 距过期仍 >refreshAhead 直接返回，避免每次请求都刷新
	if len(overrideRT) == 0 && time.Until(d.cfg.Token().ExpireAt) > refreshAhead {
		return nil // token 还很新鲜，无需刷新
	}

	// 为 token 刷新请求设置独立的超时，防止无响应时永久阻塞 refreshMu 锁
	reqCtx, reqCancel := context.WithTimeout(ctx, 30*time.Second)
	defer reqCancel()

	form := url.Values{
		"refresh_token": {rt},
	}

	// fail 记录刷新失败并安排短间隔重试，同时重排守护，避免失败链断裂后无机会再刷。
	fail := func(format string, a ...any) error {
		err := fmt.Errorf(format, a...)
		slog.Warn("[TOKEN] 刷新失败，将短间隔重试", "错误信息", err)
		time.AfterFunc(time.Minute, func() {
			_ = d.refreshToken(context.Background())
			d.scheduleRefresh() // 失败也重排守护，保持链不断
		})
		return err
	}

	req, err := http.NewRequestWithContext(reqCtx, "POST", "https://passportapi.115.com/open/refreshToken", strings.NewReader(form.Encode()))
	if err != nil {
		return fail("[TOKEN] 创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := HTTPClient().Do(req)
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
			rt = d.cfg.Token().RefreshToken
		}
		d.cfg.SaveToken(res.Data.AccessToken, rt, res.Data.ExpiresIn)
		return nil
	}
	return fail("[TOKEN] 刷新失败: message: %s code: %d", res.Message, res.Code)
}

// scheduleRefresh 常驻守护：按当前 token 到期时间恒定提前 refreshAhead 排下一次刷新，
// 触发后无条件重排形成永不断裂的链。无论进程是否重启、是否曾有请求触发，守护始终存在，
// 不依赖业务请求被动兜底。
func (d *Open115) scheduleRefresh() {
	delay := max(time.Until(d.cfg.Token().ExpireAt.Add(-refreshAhead)), time.Second)
	time.AfterFunc(delay, func() {
		_ = d.refreshToken(context.Background())
		d.scheduleRefresh() // 刷新后（成功或失败）都重排下一次
	})
}

// startRefreshDaemon 在进程启动时调用一次，立即排定守护，不再依赖业务请求触发首次预约。
func (d *Open115) startRefreshDaemon() {
	d.scheduleRefresh()
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
