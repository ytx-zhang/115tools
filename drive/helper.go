package drive

import (
	"context"

	"github.com/go-resty/resty/v2"
)

// reqOption 配置单次 115 API 请求的差异项，把各 API 方法的个性参数与公共骨架解耦。
type reqOption func(*resty.Request)

// withForm 设置表单字段（POST 表单提交）。
func withForm(form map[string]string) reqOption {
	return func(r *resty.Request) { r.SetFormData(form) }
}

// withQuery 设置查询参数（GET 查询串）。
func withQuery(query map[string]string) reqOption {
	return func(r *resty.Request) { r.SetQueryParams(query) }
}

// withHeader 设置单个请求头（如 115 强制校验的 User-Agent）。
func withHeader(key, val string) reqOption {
	return func(r *resty.Request) { r.SetHeader(key, val) }
}

// doAPI 统一发起一次 115 API 请求并解析统一响应外壳 apiResponse[T]。
//
// 负责：ctx 取消检查（checkCtx）、context 透传、差异项装配（opts）、
// SetResult 解析、State 校验（由 OnAfterResponse 中间件统一完成，错误已带原始响应体片段）。
// 调用方从返回的 res.Data 取业务字段即可，无需重复样板。
//
// method 为 HTTP 方法（"POST"/"GET"），path 为 115 开放平台接口路径。
func doAPI[T any](ctx context.Context, d *Open115, method, path string, opts ...reqOption) (res apiResponse[T], err error) {
	if err = checkCtx(ctx); err != nil {
		return
	}
	req := d.Client.R().SetContext(ctx)
	for _, opt := range opts {
		opt(req)
	}
	req.SetResult(&res)
	_, err = req.Execute(method, path)
	return
}

// truncateBody 截断响应体用于错误提示，避免超长 HTML/二进制刷屏。
func truncateBody(b []byte) string {
	s := string(b)
	const maxLen = 512
	if len(s) > maxLen {
		s = s[:maxLen] + "...(截断)"
	}
	return s
}
