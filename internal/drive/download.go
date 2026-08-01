package drive

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// passthroughUA 透传模式取链 & 回源 CDN 统一使用的 UA。
// ⚠️ 115 直链绑定取链 UA，回源必须与取链一致，故此值取链和回源两处共用，勿分叉。
const passthroughUA = "Fuck115"

// Redirector 把 115 网盘的 pickcode 转成真实直链。按请求 UA 分流：
//   - UA 为空或以 "Lavf" 开头 → 透传模式：后端回源 115 CDN 流式回传（供前置 nginx 切片缓存）；
//   - 其余 UA → 302 模式：直接重定向到 CDN 直链，由客户端自行跳转。
//
// 直链有时效，按 URL 的 t 参数动态计算缓存时长，并对同一 key 的并发做 singleflight 合并。
type Redirector struct {
	api   *Open115
	cache sync.Map
	sf    singleflight.Group
}

// errEmptyURL 表示 115 接口成功返回但直链为空。
var errEmptyURL = errors.New("115接口返回空直链")

// proxyClient 透传专用客户端：body 不限时（大文件流式），仅限响应头等待。
// ⚠️ MaxIdleConnsPerHost 默认仅 2，nginx slice 并发拉片会超出并丢弃空闲连接，
// 导致每片重做 TLS 握手（非对称运算，CPU 杀手）。必须显式放大。
var proxyClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false, // CDN 拉流用 HTTP/1.1 开销更低
	},
}

// NewRedirector 创建直链重定向器。
func NewRedirector(api *Open115) *Redirector {
	return &Redirector{api: api}
}

type cacheItem struct {
	url      string
	name     string
	expireAt time.Time
}

// loadCache 从缓存读取并做类型断言（存的是指针，供过期回调比对）。
func (s *Redirector) loadCache(key string) (*cacheItem, bool) {
	val, ok := s.cache.Load(key)
	if !ok {
		return nil, false
	}
	item, ok := val.(*cacheItem)
	return item, ok
}

// storeCache 写入缓存并注册过期回调。回调捕获本次写入的指针，刷新后旧定时器
// 比对指针不相等会放弃删除，避免误删新条目。
func (s *Redirector) storeCache(key, url, name string, expiration time.Duration) {
	item := &cacheItem{url: url, name: name, expireAt: time.Now().Add(expiration)}
	s.cache.Store(key, item)
	time.AfterFunc(expiration, func() {
		if v, ok := s.cache.Load(key); ok {
			if p, ok := v.(*cacheItem); ok && p == item {
				s.cache.Delete(key)
			}
		}
	})
}

// resolveURL 查缓存 → singleflight 取链 → 存缓存，返回可用直链。
// ua 决定 115 直链绑定关系，同时作为缓存 key 的一部分（透传/302 各自分桶）。
func (s *Redirector) resolveURL(r *http.Request, pickCode, ua string) (*cacheItem, error) {
	cacheKey := pickCode + "|" + ua

	if item, ok := s.loadCache(cacheKey); ok && !time.Now().After(item.expireAt) {
		slog.Debug("[strm后端] 缓存命中", "媒体名称", item.name, "UA", ua)
		return item, nil
	}

	// 单飞：同一 key 的并发请求合并为一次 115 调用，所有等待者共享同一份结果。
	ch := s.sf.DoChan(cacheKey, func() (any, error) {
		info, err := s.api.GetDownloadUrl(r.Context(), pickCode, ua)
		if err != nil {
			slog.Error("[strm后端] 115接口报错", "err", err)
			return nil, err
		}
		if info == nil || info.Url == "" {
			slog.Error("[strm后端] 115接口报错", "err", errEmptyURL)
			return nil, errEmptyURL
		}

		expiration := 30 * time.Minute
		if u, err := url.Parse(info.Url); err == nil {
			if tStr := u.Query().Get("t"); tStr != "" {
				if tInt, err := strconv.ParseInt(tStr, 10, 64); err == nil {
					if remaining := time.Until(time.Unix(tInt, 0).Add(-5 * time.Minute)); remaining > 0 {
						expiration = remaining
					}
				}
			}
		}

		s.storeCache(cacheKey, info.Url, info.Name, expiration)
		slog.Info("[strm后端] 获取新地址", "名称", info.Name, "UA", ua, "缓存时长", expiration.Round(time.Second).String())
		return &cacheItem{url: info.Url, name: info.Name}, nil
	})
	select {
	case <-r.Context().Done(): // 客户端断开
		return nil, r.Context().Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*cacheItem), nil
	}
}

// isPassthrough 判断是否走透传：UA 为空或以 "Lavf" 开头（FFmpeg/libavformat）。
func isPassthrough(ua string) bool {
	ua = strings.TrimSpace(ua)
	return ua == "" || strings.HasPrefix(ua, "Lavf")
}

// downgradeToHTTP 把直链的 https 降级为 http，省掉回源 TLS 解密（CPU 大头）。
// ⚠️ 仅用于透传模式（服务端到 CDN 的内部回源）。302 模式绝不可降级：
// 浏览器在 https 页面下会因混合内容拦截 http 跳转，导致页面加载失败。
func downgradeToHTTP(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return raw
	}
	u.Scheme = "http"
	return u.String()
}

// RedirectToRealURL 处理 /download?pickcode=xxx：按 UA 分流透传 / 302。
func (s *Redirector) RedirectToRealURL(w http.ResponseWriter, r *http.Request) {
	pickCode := r.URL.Query().Get("pickcode")
	if pickCode == "" {
		slog.Warn("[strm后端] 未找到pickcode")
		http.Error(w, "未找到pickcode", http.StatusBadRequest)
		return
	}

	if isPassthrough(r.Header.Get("User-Agent")) {
		s.serveProxy(w, r, pickCode)
		return
	}
	s.serveRedirect(w, r, pickCode)
}

// serveRedirect 302 模式：用客户端真实 UA 取链并重定向（客户端自行访问 CDN）。
func (s *Redirector) serveRedirect(w http.ResponseWriter, r *http.Request, pickCode string) {
	item, err := s.resolveURL(r, pickCode, strings.TrimSpace(r.Header.Get("User-Agent")))
	if err != nil {
		if r.Context().Err() != nil {
			return // 客户端断开，无需响应
		}
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, item.url, http.StatusFound)
}

// serveProxy 透传模式：用 passthroughUA 取链并回源 115 CDN，把 CDN 响应
// （状态码 + 全部响应头 + body）原样流式回传给前置 nginx，供其切片缓存。
// ⚠️ 只透传 Range + UA 到 CDN；回程原样透传 CDN 的所有头，绝不自己重算任何
//
//	长度字段（nginx slice 靠 Content-Range 的 TOTAL 切分整文件）。
func (s *Redirector) serveProxy(w http.ResponseWriter, r *http.Request, pickCode string) {
	item, err := s.resolveURL(r, pickCode, passthroughUA)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.NotFound(w, r)
		return
	}

	// 回源降级为 http：省掉 TLS 解密开销。仅限透传，302 分支保持 https。
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, downgradeToHTTP(item.url), nil)
	if err != nil {
		http.Error(w, "构造回源请求失败", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", passthroughUA)
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng) // 原样转发 nginx 的切片 Range
	}

	resp, err := proxyClient.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		slog.Error("[strm后端] 回源CDN失败", "名称", item.name, "err", err)
		http.Error(w, "回源失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 原样透传 CDN 的全部响应头（含同名多值），绝不重算任何长度字段
	// （nginx slice 靠 Content-Range 的 TOTAL 切分整文件）。
	dst := w.Header()
	for k, vs := range resp.Header {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode) // 原样透传 200 / 206
	if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
		slog.Debug("[strm后端] 透传中断", "名称", item.name, "err", err)
	}
}
