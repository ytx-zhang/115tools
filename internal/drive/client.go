// Package drive 是 115 开放平台（refresh_token）客户端：纯 API 封装，不校验用户输入、不掺业务。
//
// 划分：
//   - client.go：Client 装配、统一请求入口（request/Get/Post）、全局限流、鉴权、重试策略；
//   - auth.go：访问令牌自动刷新（RefreshDaemon）与凭证验证（Verify）；
//   - types.go：领域类型、StructOrArray 容错、SHA1 工具；
//   - files.go：文件/目录操作（列表/下载直链/增删移改）；
//   - upload.go：上传（秒传 init → OSS 单传/分片）；
//   - offline.go：离线下载（链接/种子/任务管理）；
//   - pickcode.go：pickcode 本地解码；
//   - torrent.go：种子 bencode 解析。
package drive

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/conf"
	"golang.org/x/time/rate"
)

// Resp 是 115 开放平台统一响应外壳。
type Resp[T any] struct {
	State   bool   `json:"state"`
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// probeResp 仅用于探测 115 业务响应（state=false 的限流 / 鉴权错误）。
type probeResp struct {
	State   bool   `json:"state"`
	Code    int64  `json:"code"`
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

// APIError 是 115 业务错误（HTTP 200 + state=false，或鉴权失败）。携带 code/message/HTTP 状态，
// Error() 内含 message 文本，故上层 strings.Contains(err.Error(), "该目录名称已存在") 仍可用；
// 需要精确判定时可用 errors.As 取出 APIError 读取 Code/HTTPStatus。
type APIError struct {
	Code       int64
	Message    string
	HTTPStatus int
	Body       string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("[115报错] %s (code: %d, HTTP %d, 原始响应: %s)", e.Message, e.Code, e.HTTPStatus, e.Body)
	}
	return fmt.Sprintf("[115报错] %s (code: %d, HTTP %d)", e.Message, e.Code, e.HTTPStatus)
}

// Get 发送 GET 请求，解析 data 段为 T，返回数据、真实网络耗时与错误。
func Get[T any](ctx context.Context, c *Client, path string, query Form, opts ...ReqOption) (T, time.Duration, error) {
	return doRequest[T](ctx, c, http.MethodGet, path, query, opts...)
}

// Post 发送 POST 请求，解析 data 段为 T，返回数据、真实网络耗时与错误。
func Post[T any](ctx context.Context, c *Client, path string, form Form, opts ...ReqOption) (T, time.Duration, error) {
	return doRequest[T](ctx, c, http.MethodPost, path, form, opts...)
}

// doRequest 是 request 的薄封装：解析 data 段为具体类型 T。
func doRequest[T any](ctx context.Context, c *Client, method, path string, params Form, opts ...ReqOption) (T, time.Duration, error) {
	var zero T
	data, dur, err := c.request(ctx, method, path, params, opts...)
	if err != nil {
		return zero, dur, err
	}
	var out T
	if len(data) > 0 && string(data) != "null" {
		if uerr := json.Unmarshal(data, &out); uerr != nil {
			return zero, dur, fmt.Errorf("解析 data 段失败: %w (原始响应: %s)", uerr, prettyJSON(data))
		}
	}
	return out, dur, nil
}

// request 发送一次 HTTP 请求并解析 115 响应外壳，返回 data 段原始 JSON、HTTP 状态码、网络耗时与错误。
// 内部承担所有重试与令牌保活语义：
//   - 业务限流（200 + state=false + 「稍后再试」）：递增退避重试，最多 maxRetries 次；
//   - 访问令牌失效（401/403 或 115 鉴权错误）：强制刷新一次后重试，覆盖本地 ExpireAt 不准 / 115 提前吊销；
//   - refresh_token 本身已失效：不再空刷，直接以本次响应判失败（横幅已提示用户重新授权）。
func (c *Client) request(ctx context.Context, method, path string, params Form, opts ...ReqOption) ([]byte, time.Duration, error) {
	refreshed := false // 已因令牌失效强制刷新并重试一次，避免 auth 错误死循环
	for attempt := 0; ; attempt++ {
		if err := apiLimiter.Wait(ctx); err != nil {
			return nil, 0, err
		}
		// 请求前保活：token 临近过期则刷新（refresh_token 缺失/已死则直接失败）。
		if err := c.refreshToken(ctx, false, ""); err != nil {
			return nil, 0, err
		}
		attemptStart := time.Now()
		status, body, rerr := c.do(ctx, method, path, params, opts...)
		dur := time.Since(attemptStart)
		if rerr != nil {
			return nil, dur, fmt.Errorf("%w (原始响应: %s)", rerr, prettyJSON(body))
		}

		// 外壳用 Resp[jsontext.Value] 宽松解析：data 段可能是 []，延后到 state=true 再解析到具体类型。
		var shell Resp[jsontext.Value]
		if err := json.Unmarshal(body, &shell); err != nil {
			return nil, dur, fmt.Errorf("解析响应外壳失败: %w (原始响应: %s)", err, prettyJSON(body))
		}
		probe := probeResp{State: shell.State, Code: shell.Code, Message: shell.Message}

		// 业务限流：HTTP 200 + state=false + 「稍后再试」→ 递增等待重试。
		if status == http.StatusOK && !probe.State && strings.Contains(probe.Message, "稍后再试") && attempt < maxRetries {
			wait := retryWaitTime * time.Duration(attempt+1)
			select {
			case <-ctx.Done():
				return nil, dur, context.Cause(ctx)
			case <-time.After(wait):
			}
			continue
		}

		// 访问令牌失效：强制刷新并重试一次。已知 refresh_token 本身已失效则跳过重刷，直接判失败。
		if !refreshed && attempt < maxRetries && isTokenExpired(status, probe) {
			if deadReported.Load() {
				return failResp(&shell, status, dur)
			}
			slog.WarnContext(ctx, "访问令牌可能已失效，强制刷新后重试", "HTTP", status, "消息", probe.Message)
			if ferr := c.refreshToken(ctx, true, ""); ferr != nil {
				return nil, dur, ferr
			}
			refreshed = true
			continue
		}

		if !shell.State {
			return failResp(&shell, status, dur)
		}
		return shell.Data, dur, nil
	}
}

// failResp 将 115 业务失败外壳转为 APIError（携带 code/message/HTTP 状态，供上层精确判定）。
func failResp(shell *Resp[jsontext.Value], status int, dur time.Duration) ([]byte, time.Duration, error) {
	return nil, dur, &APIError{Code: shell.Code, Message: shell.Message, HTTPStatus: status, Body: string(shell.Data)}
}

// do 单次 HTTP 请求：GET 走 query、其余走 form body，注入 Bearer，返回状态码与完整响应体。
func (c *Client) do(ctx context.Context, method, path string, params Form, opts ...ReqOption) (int, []byte, error) {
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
			slog.DebugContext(ctx, "关闭响应体失败", "错误", cerr)
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

// log 统一云端操作日志：成功 Info（每个 115 请求一条，便于在 docker 日志观察云端动作）、失败 Error，附真实网络耗时。
func logCall(ctx context.Context, action string, err error, dur time.Duration, info ...any) {
	if err != nil {
		slog.ErrorContext(ctx, action+"失败", slices.Concat(info, []any{"错误", err, "耗时", dur})...)
		return
	}
	slog.InfoContext(ctx, action+"完成", slices.Concat(info, []any{"耗时", dur})...)
}

// isTokenExpired 判断 115 响应是否因访问令牌失效而失败，用于触发强制刷新重试。
// 精确匹配，避免把「链接过期」「文件失效」等无关错误误判为鉴权问题而白白燃烧 refresh_token：
//   - HTTP 401/403 视为鉴权失败；
//   - 或 115 业务响应 state=false 且 code=401，或 message 含 token/登录/授权 关键字。
func isTokenExpired(status int, probe probeResp) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	if !probe.State {
		if probe.Code == 401 {
			return true
		}
		msg := strings.ToLower(probe.Message)
		if strings.Contains(msg, "token") || strings.Contains(msg, "登录") || strings.Contains(msg, "授权") {
			return true
		}
	}
	return false
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
