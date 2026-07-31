package drive

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Redirector 把 115 网盘的 pickcode 转成可直接播放的真实直链并重定向给 Emby。
// 直链有有效期，这里按 URL 里的 t 参数动态计算缓存时长，并对同一 pickcode+UA
// 并发请求做单飞合并（singleflight），避免重复打 115 接口。
type Redirector struct {
	api   *Open115
	cache sync.Map
	sf    singleflight.Group
}

// errEmptyURL 表示 115 接口成功返回但直链为空。
var errEmptyURL = errors.New("115接口返回空直链")

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

// RedirectToRealURL 处理 /download?pickcode=xxx：缓存命中直接重定向，否则取直链后缓存并跳转。
func (s *Redirector) RedirectToRealURL(w http.ResponseWriter, r *http.Request) {
	pickCode := r.URL.Query().Get("pickcode")
	if pickCode == "" {
		slog.Warn("[strm后端] 未找到pickcode")
		http.Error(w, "未找到pickcode", http.StatusBadRequest)
		return
	}

	clientUA := strings.TrimSpace(r.Header.Get("User-Agent"))
	cacheKey := pickCode + "_" + clientUA

	if item, ok := s.loadCache(cacheKey); ok && !time.Now().After(item.expireAt) {
		slog.Debug("[strm后端] 缓存命中", "媒体名称", item.name, "UA", clientUA)
		http.Redirect(w, r, item.url, http.StatusFound)
		return
	}

	// 单飞：同一 key 的并发请求合并为一次 115 调用，所有等待者共享同一份结果。
	ch := s.sf.DoChan(cacheKey, func() (any, error) {
		info, err := s.api.GetDownloadUrl(r.Context(), pickCode, clientUA)
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
		slog.Info("[strm后端] 获取新地址", "名称", info.Name, "UA", clientUA, "缓存时长", expiration.Round(time.Second).String())
		return info, nil
	})
	select {
	case <-r.Context().Done(): // 客户端断开即静默退出（请求已无所谓响应）
		return
	case res := <-ch:
		if res.Err != nil {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, res.Val.(*DownloadUrlInfo).Url, http.StatusFound)
	}
}
