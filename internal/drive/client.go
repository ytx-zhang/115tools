package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/ytx-zhang/115tools/internal/config"
	"golang.org/x/time/rate"
)

// Resp 是 115 开放平台统一响应外壳：state 为 false 时 code/message 是错误信息。
type Resp[T any] struct {
	State   bool   `json:"state"`
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// Form 是表单/查询参数的键值集合（Get/Post/Raw 的参数入口，GET 走 query、POST 走 form）。
type Form map[string]string

// RestyOption 是单次请求的函数式装配选项（仅 ReqWithUA 使用；表单/查询参数由 Get/Post 直接传入）。
type RestyOption func(*resty.Request)

// ReqWithUA 设置 User-Agent（下载直链取链用，直链绑定 UA）。
func ReqWithUA(ua string) RestyOption {
	return func(r *resty.Request) { r.SetHeader("User-Agent", ua) }
}

// removeEmpty 过滤空串字段（openlist 的 removeEmptyForm 同款语义）。
func removeEmpty(m Form) Form {
	out := make(Form, len(m))
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// sharedHTTPClient 供全项目各模块复用（连接池），30s 超时防挂死。
// 下载大文件时若需更长超时，调用方应使用 http.Transport 自建。
var sharedHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
}

// HTTPClient 返回全局共享的 HTTP 客户端（连接池 + 超时）。
func HTTPClient() *http.Client {
	return sharedHTTPClient
}

// apiLimiter 全局 API 限流：2/s 恢复 + burst 3（突发 3 个瞬时并发，之后每 0.5s 补 1 个令牌）。
// resty v2 无内置限流器，用 x/time/rate 手写（与 v3 的 NewRateLimitTokenBucket 语义一致）。
// ⚠️ 必须包级而非实例字段：ApplyConfig 热切换会重建 Client，实例字段会让限流器随旧实例失效。
var apiLimiter = rate.NewLimiter(rate.Limit(2), 3)

// Client 是 115 开放平台 API 客户端（refresh_token 登录）。
// 请求经 resty v2 装配统一限流（2/s burst 3）+ 30s 超时 + Bearer 鉴权。
// token 自动刷新由独立守护 RefreshDaemon 负责（见 token.go），请求前同步保活。
type Client struct {
	rc  *resty.Client  // 底层 HTTP 客户端（已装配好限流/鉴权中间件）
	cfg *config.Config // 配置（token 存取）
}

// NewClient 创建 115 开放平台客户端（纯装配，无网络请求）。
// 固定 baseURL 为 proapi.115.com。
// 重试交给 resty 内置机制：识别「稍后再试」（115 业务限流）后指数退避重试最多 3 次。
// 用 resty 而非手写循环的原因：AddRetryCondition 用 body 通用反序列化判断
// state=false + message「稍后再试」（与泛型 Result 类型解耦），退避/次数由
// SetRetryWaitTime/SetRetryCount 统一管理。
func NewClient(cfg *config.Config) *Client {
	c := &Client{cfg: cfg}
	c.rc = resty.NewWithClient(HTTPClient()).
		SetBaseURL("https://proapi.115.com").
		SetTimeout(30 * time.Second).
		SetRetryCount(3).
		SetRetryWaitTime(time.Second).
		SetRetryMaxWaitTime(4 * time.Second).
		AddRetryCondition(func(resp *resty.Response, err error) bool {
			// 仅对 115 业务限流重试：HTTP 200 + state=false + message「稍后再试」。
			// 用通用外壳判断（与泛型 Result 类型解耦）；网络错误（err != nil）不重试。
			if err != nil || resp == nil || resp.StatusCode() != http.StatusOK {
				return false
			}
			var shell struct {
				State   bool   `json:"state"`
				Message string `json:"message"`
			}
			if json.Unmarshal(resp.Body(), &shell) != nil {
				return false
			}
			return !shell.State && strings.Contains(shell.Message, "稍后再试")
		}).
		OnBeforeRequest(func(_ *resty.Client, r *resty.Request) error {
			// 请求发出前：限流（2/s burst 3）+ 确保 token 有效（保活刷新）并注入 Bearer
			if err := apiLimiter.Wait(r.Context()); err != nil {
				return err
			}
			if err := refreshAccessToken(r.Context(), c.cfg, ""); err != nil {
				return err
			}
			r.SetAuthToken(c.cfg.Token().AccessToken)
			return nil
		})
	return c
}

// Get 发送 GET 请求，把响应 data 段反序列化为 T 后返回。
// query 是查询参数（GET 走 query 串）；opts 用于附加特殊选项（如 ReqWithUA）。
// state=false 或网络错误时直接报错（错误含 115 code/message 或底层网络错误）。
func Get[T any](ctx context.Context, c *Client, url string, query Form, opts ...RestyOption) (T, error) {
	resp, err := exec[T](ctx, c, http.MethodGet, url, query, opts...)
	return resp.Data, err
}

// Post 发送 POST 请求，把响应 data 段反序列化为 T 后返回。
// form 是表单参数（POST 走 form body）；opts 用于附加特殊选项（如 ReqWithUA）。
// state=false 或网络错误时直接报错。data 段为 null/空时返回零值 T。
func Post[T any](ctx context.Context, c *Client, url string, form Form, opts ...RestyOption) (T, error) {
	resp, err := exec[T](ctx, c, http.MethodPost, url, form, opts...)
	return resp.Data, err
}

// exec 统一执行请求：按 method 自动决定 query/form 装配 → 解析统一外壳 Resp[T]
// → state=false 报错 → 返回外壳（Get/Post 取 Data 段）。
// token 保活刷新由 OnBeforeRequest 中间件负责，重试由 resty AddRetryCondition 机制负责。
func exec[T any](ctx context.Context, c *Client, method, url string, params Form, opts ...RestyOption) (Resp[T], error) {
	// ⚠️ 解析用 Resp[json.RawMessage] 而非 Resp[T]：115 错误场景（如创建同名目录 code 20004）
	// data 段是 []（与 T 结构不匹配），直接 SetResult(&Resp[T]) 会在 resty 内部解析失败，
	// 导致 state/code 读不到（报错信息丢失 115 的 code）。用 RawMessage 宽松解析外壳，
	// data 段延后到 state=true 时再手动反序列化到 T。
	var shell Resp[json.RawMessage]
	req := c.rc.R().SetContext(ctx)
	if method == http.MethodGet {
		req.SetQueryParams(removeEmpty(params))
	} else {
		req.SetFormData(removeEmpty(params))
	}
	for _, o := range opts {
		o(req)
	}
	req.SetResult(&shell)
	response, err := req.Execute(method, url)
	if err != nil {
		// 网络错误或响应体解析失败（如 115 突然改返回格式）：附上完整原始响应便于调试。
		// open 接口返回恒为 JSON，prettyJSON 解码 \uXXXX 转义，中文可读。
		if response != nil && len(response.Body()) > 0 {
			return Resp[T]{}, fmt.Errorf("%w (原始响应: %s)", err, prettyJSON(response.Body()))
		}
		return Resp[T]{}, err
	}
	if !shell.State {
		return Resp[T]{}, fmt.Errorf("[115报错] %s (code: %d, HTTP %d, 原始响应: %s)",
			shell.Message, shell.Code, response.StatusCode(), prettyJSON(response.Body()))
	}
	var resp Resp[T]
	if len(shell.Data) == 0 || string(shell.Data) == "null" {
		return resp, nil // data 缺失（null）按空处理，由调用方按需校验
	}
	if err := json.Unmarshal(shell.Data, &resp.Data); err != nil {
		return Resp[T]{}, fmt.Errorf("解析 data 段失败: %w (原始响应: %s)", err, prettyJSON(shell.Data))
	}
	return resp, nil
}
