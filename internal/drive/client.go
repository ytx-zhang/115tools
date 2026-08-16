package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
	"resty.dev/v3"
)

// Resp 是 115 开放平台统一响应外壳：state 为 false 时 code/message 是错误信息。
type Resp[T any] struct {
	State   bool   `json:"state"`
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// Form 是表单/查询参数的键值集合（ReqWithForm/ReqWithQuery 使用）。
type Form map[string]string

// RestyOption 是单次请求的函数式装配选项（复用 OpenList/115-sdk-go 模式）。
type RestyOption func(*resty.Request)

// ReqWithForm 设置表单参数并过滤空值（115 空串字段直接省略）。
func ReqWithForm(form Form) RestyOption {
	return func(r *resty.Request) { r.SetFormData(removeEmpty(form)) }
}

// ReqWithQuery 设置查询参数并过滤空值。
func ReqWithQuery(query Form) RestyOption {
	return func(r *resty.Request) { r.SetQueryParams(removeEmpty(query)) }
}

// ReqWithResp 注册响应解析目标（doRequest 内部使用，把统一外壳解析进 resp）。
func ReqWithResp(v any) RestyOption {
	return func(r *resty.Request) { r.SetResult(v) }
}

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

// is401Code 判断错误码是否为 401 系列（115 用 4010/40100 表示 access_token 失效）。
func is401Code(code int64) bool {
	return strings.HasPrefix(strconv.FormatInt(code, 10), "401")
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

// Client 是 115 开放平台 API 客户端（refresh_token 登录）。
// 请求经 resty v3 装配统一限流（内置令牌桶 burst 3 + 2/s）+ 30s 超时 + Bearer 鉴权。
// token 自动刷新由独立守护 RefreshDaemon 负责（见 token.go），请求前同步保活。
type Client struct {
	rc  *resty.Client  // 底层 HTTP 客户端（已装配好限流/鉴权中间件）
	cfg *config.Config // 配置（token 存取）
}

// NewClient 创建 115 开放平台客户端（纯装配，无网络请求）。
// 固定 baseURL 为 proapi.115.com。
// ⚠️ resty v3 默认 SetResult 会消费 body，中间件里 Bytes() 将为空；
// 开启无限读取后 body 常驻内存，CallRaw 与 401 判定才可靠。
func NewClient(cfg *config.Config) *Client {
	c := &Client{cfg: cfg}
	c.rc = resty.NewWithClient(HTTPClient()).
		SetBaseURL("https://proapi.115.com").
		SetTimeout(30 * time.Second).
		SetResponseBodyUnlimitedReads(true).
		// 内置令牌桶限流：burst 3 允许突发 3 个瞬时并发，之后每 0.5s 补 1 个
		// 令牌（2/s），排队的请求按此速率逐个放行；空闲后桶回满又突发 3 个。
		SetRateLimiter(resty.NewRateLimitTokenBucket(2, 3)).
		AddRequestMiddleware(func(_ *resty.Client, r *resty.Request) error {
			// 请求发出前：确保 token 有效（保活刷新）并注入 Bearer
			if err := refreshAccessToken(r.Context(), c.cfg, ""); err != nil {
				return err
			}
			r.SetAuthToken(c.cfg.Token().AccessToken)
			return nil
		})
	return c
}

// doRequest 构造请求并执行（所有请求的最终出口，仅内部使用）。
func (c *Client) doRequest(ctx context.Context, url, method string, opts ...RestyOption) (*resty.Response, error) {
	req := c.rc.R().SetContext(ctx)
	for _, o := range opts {
		o(req)
	}
	return req.Execute(method, url)
}

// Call 调用 115 开放平台接口：解析统一外壳（Resp）后把 data 段反序列化到 respData。
// respData 必须是 data 段的目标类型（如 *DirInfo、*StructOrArray[T]、*[]T、*struct{...}），
// 不能传 *Resp[T]——那会导致 data 段被二次套壳解析成空。
// token 失效（code 99 / 401 系列）时自动刷新并重试一次；respData 传 nil 表示不关心返回体。
func (c *Client) Call(ctx context.Context, url, method string, respData any, opts ...RestyOption) error {
	return c.call(ctx, url, method, respData, true, false, opts...)
}

// CallRaw 调用接口但不解析外壳，直接把完整响应体反序列化到 respData
// （外壳不统一/需手动解析 data 的场景，如 upload/init、get_token、get_quota_info）。
// respData 传 *Resp[T]（完整外壳含 state/data）即可，调用方需自行校验 State。
func (c *Client) CallRaw(ctx context.Context, url, method string, respData any, opts ...RestyOption) error {
	return c.call(ctx, url, method, respData, false, false, opts...)
}

// call 核心实现。retry 为 true 表示已刷新过 token（最多重试一次）。
// 重试策略：限流类「稍后再试」自动退避重试（resty v3 的重试机制在中间件层不可靠，
// 收敛到此处显式处理）；access_token 失效（code 99 / 401 系列）刷新后重试一次。
func (c *Client) call(ctx context.Context, url, method string, respData any, extractData, retry bool, opts ...RestyOption) error {
	for attempt := 0; ; attempt++ {
		var resp Resp[json.RawMessage]
		response, err := c.doRequest(ctx, url, method, append(opts, ReqWithResp(&resp))...)
		if err != nil {
			return err
		}
		if resp.State {
			if respData == nil {
				return nil
			}
			var data []byte
			if extractData {
				data = resp.Data
			} else {
				// CallRaw：不解析外壳，直接用完整响应体
				data = response.Bytes()
			}
			if len(data) == 0 || string(data) == "null" {
				return nil // data 缺失（null）按空处理，由调用方按需校验
			}
			return json.Unmarshal(data, respData)
		}
		// state=false：按错误类别分流
		if !retry && (resp.Code == 99 || is401Code(resp.Code)) {
			// access_token 失效：刷新后重试一次
			if err := refreshAccessToken(ctx, c.cfg, ""); err != nil {
				return err
			}
			retry = true
			continue
		}
		if strings.Contains(resp.Message, "稍后再试") {
			// 限流：退避重试最多 3 次（1s 起指数退避）
			if attempt < 2 {
				delay := time.Duration(1<<attempt) * time.Second
				if err := sleepCtx(ctx, delay); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%w: %s (code: %d, HTTP %d)", ErrRateLimited, resp.Message, resp.Code, response.StatusCode())
		}
		return fmt.Errorf("[115报错] %s (code: %d)", resp.Message, resp.Code)
	}
}

// sleepCtx 可取消的等待（限流退避用）。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
