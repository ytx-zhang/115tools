// Package drive 封装 115 网盘的开放平台 HTTP API。
//
// 【文件划分】
//   - open115.go：客户端装配（限流、自动带 token、统一错误处理）与基础类型；
//   - token.go：访问令牌（AccessToken）的自动刷新；
//   - file_api.go：文件/目录操作（下载直链、列表、增删移改）；
//   - upload_api.go：上传（普通文件/内存字节）与 SHA1 工具；
//   - offline.go：BT/链接离线下载；
//   - oss_upload.go：大文件 OSS 分片上传；
//   - httpclient.go：全局共享 HTTP 客户端（连接池）。
package drive

import (
	"115tools/config"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"golang.org/x/time/rate"
)

// apiResponse 是 115 开放平台的统一响应外壳：state 为 false 时 message/code 是错误信息。
type apiResponse[T any] struct {
	State   bool   `json:"state"`
	Message string `json:"message"`
	Code    int    `json:"code"`
	Data    T      `json:"data"`
}

// Open115 是 115 API 客户端。它管理 token 刷新、请求限流（每秒 3 次，突发 5 次）
// 以及常用的文件/目录操作方法。业务方无需关心鉴权——每次请求自动携带有效 token。
type Open115 struct {
	Client    *resty.Client  // 底层 HTTP 客户端（已装配好限流/鉴权/重试中间件）
	cfg       *config.Config // 配置（token 存取）
	refreshMu sync.Mutex     // token 刷新互斥锁：并发请求同时发现过期时只刷一次
}

// checkCtx 返回 ctx 的取消原因（context.Cause；未取消时返回 nil）。
// 各 API 方法开头用它快速失败，避免向已取消的任务（如热重载中停掉的同步）
// 继续发起网络请求。
func checkCtx(ctx context.Context) error {
	return context.Cause(ctx)
}

// New115Drive 创建 115 API 客户端，并立即刷新一次 token 确保可用。
func New115Drive(cfg *config.Config) *Open115 {
	d := &Open115{
		cfg: cfg,
	}
	limiter := rate.NewLimiter(rate.Limit(3), 5)
	d.Client = resty.New().
		SetBaseURL("https://proapi.115.com").
		SetTimeout(30 * time.Second).
		SetRetryCount(2).
		SetRetryWaitTime(3 * time.Second).
		SetRetryMaxWaitTime(3 * time.Second).
		OnBeforeRequest(func(c *resty.Client, r *resty.Request) error {
			// 每个请求发出前：先排队等限流额度，再确保 token 有效，最后带上鉴权头
			if err := limiter.Wait(r.Context()); err != nil {
				return err
			}
			if err := d.refreshToken(r.Context()); err != nil {
				return err
			}
			r.SetHeader("Authorization", "Bearer "+d.cfg.GetAccessToken())
			return nil
		}).
		OnAfterResponse(func(c *resty.Client, r *resty.Response) error {
			// 每个响应回来后：统一把网络错误/解析失败/业务失败转成 Go error
			if r.Error() != nil {
				return fmt.Errorf("网络底层错误: %v", r.Error())
			}
			var base apiResponse[any]
			if err := json.Unmarshal(r.Body(), &base); err != nil {
				return fmt.Errorf("JSON 解析失败: %w", err)
			}
			if !base.State {
				return fmt.Errorf("[115报错]: %s code: %d", base.Message, base.Code)
			}
			return nil
		}).
		AddRetryCondition(func(r *resty.Response, err error) bool {
			// 仅当 115 明确说「稍后再试」（限流类提示）时才自动重试
			if err != nil {
				return strings.Contains(err.Error(), "稍后再试")
			}
			return false
		})
	if err := d.refreshToken(context.Background()); err != nil {
		slog.Error("[TOKEN] 更新token失败", "错误信息", err)
	}
	return d
}
