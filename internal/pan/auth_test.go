package pan

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestIsRefreshTokenDead(t *testing.T) {
	cases := []struct {
		name string
		code int64
		msg  string
		want bool
	}{
		{"典型失效码", 40140137, "refresh_token 已失效，请停止重试并重新授权", true},
		{"提示重新授权", 0, "请重新授权后重试", true},
		{"消息含已失效", 0, "refresh_token 已失效", true},
		{"普通业务失败", 20004, "该目录名称已存在", false},
		{"限流", 0, "请求过于频繁，请稍后再试", false},
		{"成功", 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRefreshTokenDead(c.code, c.msg); got != c.want {
				t.Errorf("isRefreshTokenDead=%v (期望 %v) code=%d msg=%q", got, c.want, c.code, c.msg)
			}
		})
	}
}

func TestErrRefreshTokenExpiredIs(t *testing.T) {
	err := fmt.Errorf("%w: %w", ErrRefreshTokenExpired, errors.New("刷新失败: code: 40140137"))
	if !errors.Is(err, ErrRefreshTokenExpired) {
		t.Error("应可用 errors.Is 识别 refresh_token 失效")
	}
}

func TestIsTokenExpired(t *testing.T) {
	cases := []struct {
		name   string
		status int
		probe  probeResp
		want   bool
	}{
		{"HTTP 401", http.StatusUnauthorized, probeResp{}, true},
		{"HTTP 403", http.StatusForbidden, probeResp{}, true},
		{"state=false code=401", http.StatusOK, probeResp{State: false, Code: 401, Message: "登录失效"}, true},
		{"message 含 token", http.StatusOK, probeResp{State: false, Code: 0, Message: "access_token 已过期"}, true},
		{"message 含 登录", http.StatusOK, probeResp{State: false, Code: 0, Message: "请先登录"}, true},
		{"message 含 授权", http.StatusOK, probeResp{State: false, Code: 0, Message: "授权已失效"}, true},
		{"限流 稍后再试", http.StatusOK, probeResp{State: false, Code: 0, Message: "请求过于频繁，请稍后再试"}, false},
		{"目录已存在", http.StatusOK, probeResp{State: false, Code: 20004, Message: "该目录名称已存在"}, false},
		{"链接过期不应误判", http.StatusOK, probeResp{State: false, Code: 0, Message: "分享链接已过期"}, false},
		{"成功响应", http.StatusOK, probeResp{State: true, Code: 0, Message: ""}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTokenExpired(c.status, c.probe); got != c.want {
				t.Errorf("isTokenExpired=%v (期望 %v) status=%d probe=%+v", got, c.want, c.status, c.probe)
			}
		})
	}
}
