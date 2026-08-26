package drive

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

	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/logs"
	"golang.org/x/time/rate"
)

// Resp 是 115 开放平台统一响应外壳：state 为 false 时 code/message 是错误信息。
type Resp[T any] struct {
	State   bool   `json:"state"`
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// Form 是表单/查询参数的键值集合（Get/Post 的参数入口，GET 走 query、POST 走 form）。
type Form map[string]string

// ReqOption 是单次请求的函数式装配选项（仅 ReqWithUA 使用；表单/查询参数由 Get/Post 直接传入）。
type ReqOption func(*http.Request)

// ReqWithUA 设置 User-Agent（下载直链取链用，直链绑定 UA）。
func ReqWithUA(ua string) ReqOption {
	return func(r *http.Request) { r.Header.Set("User-Agent", ua) }
}

// logCloud 统一云端操作日志：请求结果（含真实耗时）由通用请求函数返回，
// 各 API 方法自行打印——成功 Info(action+"完成")、失败 Error(action+"失败")。
// 字段统一为 info 后接「错误」与「耗时」（耗时用网络往返时间）。
// 调用方可在 info 中补充云端返回内容（FID/pickcode/文件名/数量等），
// 相比原 exec 内部统一打印，日志信息可更完整。
func logCloud(action string, err error, dur time.Duration, info ...any) {
	if err != nil {
		logs.Error(logs.ModuleCloud, action+"失败", slices.Concat(info, []any{"错误", err, "耗时", dur})...)
		return
	}
	logs.Info(logs.ModuleCloud, action+"完成", slices.Concat(info, []any{"耗时", dur})...)
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
// ⚠️ 必须包级而非实例字段：ApplyConfig 热切换会重建 Client，实例字段会让限流器随旧实例失效。
var apiLimiter = rate.NewLimiter(rate.Limit(2), 3)

// apiBaseURL 115 开放平台固定地址。
const apiBaseURL = "https://proapi.115.com"

// 重试参数：仅对 115 业务限流（HTTP 200 + state=false + message「稍后再试」）重试，
// 最多重试 maxRetries 次（共发出 maxRetries+1 次请求），每次等待时间递增 1s（1s → 2s → 3s）。
const (
	maxRetries    = 3
	retryWaitTime = time.Second
)

// Client 是 115 开放平台 API 客户端（refresh_token 登录）。
// 请求经统一入口 exec 装配限流（2/s burst 3）+ 30s 超时 + Bearer 鉴权。
// token 自动刷新由独立守护 RefreshDaemon 负责（见 token.go），请求前同步保活。
type Client struct {
	cfg *config.Config // 配置（token 存取）
}

// NewClient 创建 115 开放平台客户端（纯装配，无网络请求）。
// 固定 baseURL 为 proapi.115.com；重试由 exec 内循环处理。
func NewClient(cfg *config.Config) *Client {
	return &Client{cfg: cfg}
}

// Get 发送 GET 请求，把响应 data 段反序列化为 T 后返回。
// query 是查询参数（GET 走 query 串）；opts 用于附加特殊选项（如 ReqWithUA）。
// state=false 或网络错误时直接报错（错误含 115 code/message 或底层网络错误）。
// 返回真实网络耗时（纯网络往返、不含限流排队），供调用方自打日志。
func Get[T any](ctx context.Context, c *Client, url string, query Form, opts ...ReqOption) (T, time.Duration, error) {
	resp, dur, err := exec[T](ctx, c, http.MethodGet, url, query, opts...)
	return resp.Data, dur, err
}

// Post 发送 POST 请求，把响应 data 段反序列化为 T 后返回。
// form 是表单参数（POST 走 form body）；opts 用于附加特殊选项（如 ReqWithUA）。
// state=false 或网络错误时直接报错。data 段为 null/空时返回零值 T。
// 返回真实网络耗时（纯网络往返、不含限流排队），供调用方自打日志。
func Post[T any](ctx context.Context, c *Client, url string, form Form, opts ...ReqOption) (T, time.Duration, error) {
	resp, dur, err := exec[T](ctx, c, http.MethodPost, url, form, opts...)
	return resp.Data, dur, err
}

// exec 统一执行请求：按 method 自动决定 query/form 装配 → 解析统一外壳 Resp[T]
// → state=false 报错 → 返回外壳与真实耗时（Get/Post 取 Data 段）。
// ⚠️ 不打日志：日志由各 API 方法用返回的耗时自打（见 logCloud），以便补充云端返回内容。
func exec[T any](ctx context.Context, c *Client, method, url string, params Form, opts ...ReqOption) (Resp[T], time.Duration, error) {
	// ⚠️ 解析用 Resp[jsontext.Value] 而非 Resp[T]：115 错误场景（如创建同名目录 code 20004）
	// data 段是 []（与 T 结构不匹配），直接解析到 Resp[T] 会失败，导致 state/code 读不到
	// （报错信息丢失 115 的 code）。用 RawMessage 宽松解析外壳，data 段延后到
	// state=true 时再手动反序列化到 T。
	var shell Resp[jsontext.Value]
	status := http.StatusOK
	var lastBody []byte // 最后一次响应的完整 body（循环外作用域，供 [115报错] 展示原始响应）
	netDur := time.Duration(0)
	for attempt := 0; ; attempt++ {
		// 请求发出前：限流（2/s burst 3）+ 确保 token 有效（保活刷新）并注入 Bearer
		if err := apiLimiter.Wait(ctx); err != nil {
			return Resp[T]{}, 0, err
		}
		if err := refreshAccessToken(ctx, c.cfg, ""); err != nil {
			return Resp[T]{}, 0, err
		}
		attemptStart := time.Now()
		st, body, err := c.doOnce(ctx, method, url, params, opts...)
		netDur = time.Since(attemptStart)
		lastBody = body
		if err != nil {
			// 网络错误或响应体读取失败（如 115 突然改返回格式）：附上完整原始响应便于调试
			if len(body) > 0 {
				err = fmt.Errorf("%w (原始响应: %s)", err, prettyJSON(body))
			}
			return Resp[T]{}, netDur, err
		}
		status = st
		// 115 业务限流：HTTP 200 + state=false + message「稍后再试」→ 递增等待重试。
		// 与旧 resty AddRetryCondition 语义一致（网络错误/5xx 不重试）。
		var probe struct {
			State   bool   `json:"state"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &probe, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
			// body 非合法 JSON（空响应/非统一外壳）：probe 保持零值，
			// 「稍后再试」重试条件自然不命中，按普通响应继续处理。
			probe = struct {
				State   bool   `json:"state"`
				Message string `json:"message"`
			}{}
		}
		if status == http.StatusOK && !probe.State && strings.Contains(probe.Message, "稍后再试") && attempt < maxRetries {
			wait := retryWaitTime * time.Duration(attempt+1) // 1s → 2s → 3s
			select {
			case <-ctx.Done():
				return Resp[T]{}, netDur, context.Cause(ctx)
			case <-time.After(wait):
			}
			continue
		}
		// ⚠️ 重置 shell 再解析：json.Unmarshal 不清零目标结构体，若响应缺少某字段会残留
		// 上一次（重试前）的值，导致重试成功后误判（如 Data 残留空数组 [] 触发解析错误）。
		shell = Resp[jsontext.Value]{}
		if err := json.Unmarshal(body, &shell, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
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
		return resp, netDur, nil // data 缺失（null）按空处理，由调用方按需校验
	}
	if err := json.Unmarshal(shell.Data, &resp.Data, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
		perr := fmt.Errorf("解析 data 段失败: %w (原始响应: %s)", err, prettyJSON(shell.Data))
		return Resp[T]{}, netDur, perr
	}
	return resp, netDur, nil
}

// doOnce 单次 HTTP 请求（不含限流/token 装配，由 exec 循环统一处理）：
// GET 参数走 query 串、其余走 form body，返回状态码与完整响应体。
func (c *Client) doOnce(ctx context.Context, method, urlPath string, params Form, opts ...ReqOption) (int, []byte, error) {
	u, err := url.Parse(apiBaseURL + urlPath)
	if err != nil {
		return 0, nil, err
	}
	// url.Values 本质是 map[string][]string：直接索引赋值（等价 Set，少一次方法调用）。
	// GET 用 Encode 后拼 RawQuery；POST 用 Encode 后作为 form body。
	// ⚠️ 若 urlPath 本身带 query，请在此分支合并，否则会被覆盖（当前 urlPath 均不含 query）。
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
			logs.Debug(logs.ModuleCloud, "关闭响应体失败", "错误", cerr)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}
