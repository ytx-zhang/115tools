// Package pan 是 115 开放平台（refresh_token）客户端：纯 API 封装，不校验用户输入、不掺业务。
//
// 划分：
//   - client.go：Client 装配、泛型请求入口（Get/Post）、全局限流、鉴权、重试策略；
//   - auth.go：访问令牌自动刷新（RefreshDaemon）与凭证验证（Verify）；
//   - types.go：领域类型、StructOrArray 容错、SHA1 工具；
//   - files.go：文件/目录操作（列表/下载直链/增删移改）；
//   - upload.go：上传（秒传 init → OSS 单传/分片）；
//   - offline.go：离线下载（链接/种子/任务管理）；
//   - pickcode.go：pickcode 本地解码；
//   - torrent.go：种子 bencode 解析。
package pan

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/journal"
	"golang.org/x/time/rate"
)

// Resp 是 115 开放平台统一响应外壳。
type Resp[T any] struct {
	State   bool   `json:"state"`
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// probeResp 仅用于探测 115 业务限流响应（state=false + message 含「稍后再试」）。
type probeResp struct {
	State   bool   `json:"state"`
	Message string `json:"message"`
}

// Form 表单/查询参数（GET 走 query、POST 走 form body）。
type Form map[string]string

// ReqOption 单次请求的函数式装配选项。
type ReqOption func(*http.Request)

// ReqWithUA 设置 User-Agent（下载直链取链用，直链绑定 UA）。
func ReqWithUA(ua string) ReqOption {
	return func(r *http.Request) { r.Header.Set("User-Agent", ua) }
}

// apiBaseURL 115 开放平台固定地址。
const apiBaseURL = "https://proapi.115.com"

// 重试参数：仅对 115 业务限流（HTTP 200 + state=false + message 含「稍后再试」）重试，
// 最多 3 次，等待递增 1s → 2s → 3s。
const (
	maxRetries    = 3
	retryWaitTime = time.Second
)

// apiLimiter 全局 API 限流（2/s + burst 3）。必须包级：客户端实例可能重建，限流器须跨实例存活。
var apiLimiter = rate.NewLimiter(rate.Limit(2), 3)

// sharedHTTPClient 全项目共享 HTTP 客户端（连接池 + 30s 超时）。
var sharedHTTPClient = &http.Client{Timeout: 30 * time.Second}

// HTTPClient 返回共享 HTTP 客户端（供 relay 透传下载等复用连接池）。
func HTTPClient() *http.Client { return sharedHTTPClient }

// Client 是 115 开放平台 API 客户端（refresh_token 登录）。
// token 经 conf.Config 读写；自动刷新由独立守护 RefreshDaemon 负责，请求前同步保活。
type Client struct {
	cfg *conf.Config
}

// NewClient 创建客户端（纯装配，无网络请求）。
func NewClient(cfg *conf.Config) *Client { return &Client{cfg: cfg} }

// Get 发送 GET 请求，解析 data 段为 T。返回 data 与真实网络耗时。
func Get[T any](ctx context.Context, c *Client, path string, query Form, opts ...ReqOption) (T, time.Duration, error) {
	resp, dur, err := exec[T](ctx, c, http.MethodGet, path, query, opts...)
	return resp.Data, dur, err
}

// Post 发送 POST 请求，解析 data 段为 T。返回 data 与真实网络耗时。
func Post[T any](ctx context.Context, c *Client, path string, form Form, opts ...ReqOption) (T, time.Duration, error) {
	resp, dur, err := exec[T](ctx, c, http.MethodPost, path, form, opts...)
	return resp.Data, dur, err
}

// exec 统一执行请求：限流 → token 保活 → 发请求 → 解析外壳 → state=false 报错 → 解析 data。
func exec[T any](ctx context.Context, c *Client, method, path string, params Form, opts ...ReqOption) (Resp[T], time.Duration, error) {
	// 外壳用 Resp[jsontext.Value] 宽松解析：错误场景 data 段可能是 []，直接解析到 Resp[T] 会失败，
	// 导致 state/code 读不到。data 段延后到 state=true 时再解析到 T。
	var shell Resp[jsontext.Value]
	status := http.StatusOK
	var lastBody []byte
	netDur := time.Duration(0)
	for attempt := 0; ; attempt++ {
		if err := apiLimiter.Wait(ctx); err != nil {
			return Resp[T]{}, 0, err
		}
		if err := refreshAccessToken(ctx, c.cfg, ""); err != nil {
			return Resp[T]{}, 0, err
		}
		attemptStart := time.Now()
		st, body, err := c.doOnce(ctx, method, path, params, opts...)
		netDur = time.Since(attemptStart)
		lastBody = body
		if err != nil {
			if len(body) > 0 {
				err = fmt.Errorf("%w (原始响应: %s)", err, prettyJSON(body))
			}
			return Resp[T]{}, netDur, err
		}
		status = st

		// 115 业务限流：HTTP 200 + state=false + message 含「稍后再试」→ 递增等待重试。
		var probe probeResp
		if err := json.Unmarshal(body, &probe); err != nil {
			probe = probeResp{} // json.Unmarshal 失败可能残留部分字段，解析前清零防误判
		}
		if status == http.StatusOK && !probe.State && strings.Contains(probe.Message, "稍后再试") && attempt < maxRetries {
			wait := retryWaitTime * time.Duration(attempt+1)
			select {
			case <-ctx.Done():
				return Resp[T]{}, netDur, context.Cause(ctx)
			case <-time.After(wait):
			}
			continue
		}

		// 重置外壳再解析（json.Unmarshal 不清零，重试场景防字段残留）。
		shell = Resp[jsontext.Value]{}
		if err := json.Unmarshal(body, &shell); err != nil {
			return Resp[T]{}, netDur, fmt.Errorf("解析响应外壳失败: %w (原始响应: %s)", err, prettyJSON(body))
		}
		break
	}

	if !shell.State {
		apierr := fmt.Errorf("[115报错] %s (code: %d, HTTP %d, 原始响应: %s)",
			shell.Message, shell.Code, status, prettyJSON(lastBody))
		return Resp[T]{}, netDur, apierr
	}
	var resp Resp[T]
	if len(shell.Data) == 0 || string(shell.Data) == "null" {
		return resp, netDur, nil // data 缺失（null）按空处理
	}
	if err := json.Unmarshal(shell.Data, &resp.Data); err != nil {
		return Resp[T]{}, netDur, fmt.Errorf("解析 data 段失败: %w (原始响应: %s)", err, prettyJSON(shell.Data))
	}
	return resp, netDur, nil
}

// doOnce 单次 HTTP 请求：GET 走 query、其余走 form body，注入 Bearer，返回状态码与完整响应体。
func (c *Client) doOnce(ctx context.Context, method, path string, params Form, opts ...ReqOption) (int, []byte, error) {
	u, err := url.Parse(apiBaseURL + path)
	if err != nil {
		return 0, nil, err
	}
	clean := removeEmpty(params)
	vals := make(url.Values, len(clean))
	for k, v := range clean {
		vals[k] = []string{v}
	}
	var reader io.Reader
	if method == http.MethodGet {
		u.RawQuery = vals.Encode()
	} else {
		reader = strings.NewReader(vals.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token().AccessToken)
	for _, o := range opts {
		o(req)
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			journal.Debug(ctx, "关闭响应体失败", "错误", cerr)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// removeEmpty 过滤空串表单字段。
func removeEmpty(m Form) Form {
	out := make(Form, len(m))
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// log 统一云端操作日志：成功 Debug（接口调用级，量大，避免刷屏）、失败 Error，附真实网络耗时。
func log(ctx context.Context, action string, err error, dur time.Duration, info ...any) {
	if err != nil {
		journal.Error(ctx, action+"失败", slices.Concat(info, []any{"错误", err, "耗时", dur})...)
		return
	}
	journal.Debug(ctx, action+"完成", slices.Concat(info, []any{"耗时", dur})...)
}

// prettyJSON 把响应体转可读缩进文本（非 JSON 原样返回）。
func prettyJSON(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, err := json.Marshal(v, jsontext.WithIndent("  "))
	if err != nil {
		return string(b)
	}
	return string(out)
}
