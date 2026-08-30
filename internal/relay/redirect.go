// Package relay 是 /download 直链服务：把 115 的 pickcode 转成真实直链。
// 按 UA 分流：空 UA/Lavf 前缀走透传（回源 115 CDN 流式回传供 nginx 切片缓存），其余走 302。
package relay

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/cache"
	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

// passthroughUA 透传模式取链 & 回源 CDN 统一使用的 UA（115 直链绑定取链 UA，两处勿分叉）。
const passthroughUA = "Fuck115"

// Redirector 把 115 网盘的 pickcode 转成真实直链。
type Redirector struct {
	api        *pan.Client
	cache      sync.Map // URL 直链缓存（key=pickcode|ua）
	localCache *cache.Cache
	sf         singleflight.Group
}

var errEmptyURL = errors.New("115接口返回空直链")

// proxyClient 透传专用客户端：body 不限时，仅限响应头等待；放大连接池供 nginx slice 并发拉片。
var proxyClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
	},
}

// originSem 回源 115 上游并发门禁：同一时刻最多 2 个回源请求在途。
var originSem = semaphore.NewWeighted(2)

// NewRedirector 创建直链重定向器。localCache 为透传本地缓存层（nil 时禁用本地命中）。
func NewRedirector(api *pan.Client, localCache *cache.Cache) *Redirector {
	return &Redirector{api: api, localCache: localCache}
}

type cacheItem struct {
	url      string
	name     string
	expireAt time.Time
}

func (s *Redirector) loadCache(key string) (*cacheItem, bool) {
	val, ok := s.cache.Load(key)
	if !ok {
		return nil, false
	}
	item, ok := val.(*cacheItem)
	return item, ok
}

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

// resolveURL 查缓存 → singleflight 取链 → 存缓存。
func (s *Redirector) resolveURL(r *http.Request, pickCode, ua string) (*cacheItem, error) {
	cacheKey := pickCode + "|" + ua
	if item, ok := s.loadCache(cacheKey); ok && !time.Now().After(item.expireAt) {
		journal.Debug(r.Context(), "缓存命中", "媒体名称", item.name, "UA", ua)
		return item, nil
	}

	ch := s.sf.DoChan(cacheKey, func() (any, error) {
		t0 := time.Now()
		info, err := s.api.GetDownloadURL(r.Context(), pickCode, ua)
		if err != nil {
			journal.Error(r.Context(), "获取直链失败", "错误", err)
			return nil, err
		}
		if info == nil || info.URL == "" {
			journal.Error(r.Context(), "获取直链失败", "错误", errEmptyURL)
			return nil, errEmptyURL
		}
		expiration := 30 * time.Minute
		if u, err := url.Parse(info.URL); err == nil {
			if tStr := u.Query().Get("t"); tStr != "" {
				if tInt, err := strconv.ParseInt(tStr, 10, 64); err == nil {
					if remaining := time.Until(time.Unix(tInt, 0).Add(-5 * time.Minute)); remaining > 0 {
						expiration = remaining
					}
				}
			}
		}
		s.storeCache(cacheKey, info.URL, info.Name, expiration)
		journal.Info(r.Context(), "获取新地址", "文件名", info.Name, "UA", ua,
			"缓存时长", expiration.Round(time.Second).String(), "耗时", time.Since(t0))
		return &cacheItem{url: info.URL, name: info.Name, expireAt: time.Now().Add(expiration)}, nil
	})

	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*cacheItem), nil
	}
}

func isPassthrough(ua string) bool {
	ua = strings.TrimSpace(ua)
	return ua == "" || strings.HasPrefix(ua, "Lavf")
}

// downgradeToHTTP 透传模式把 https 降级为 http，省掉回源 TLS 解密。
func downgradeToHTTP(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return raw
	}
	u.Scheme = "http"
	return u.String()
}

// RedirectToRealURL 处理 /download?pickcode=xxx。
func (s *Redirector) RedirectToRealURL(w http.ResponseWriter, r *http.Request) {
	pickCode := r.URL.Query().Get("pickcode")
	if pickCode == "" {
		http.Error(w, "未找到pickcode", http.StatusBadRequest)
		return
	}
	if isPassthrough(r.Header.Get("User-Agent")) {
		s.serveProxy(w, r, pickCode)
		return
	}
	s.serveRedirect(w, r, pickCode)
}

// serveRedirect 302 模式。
func (s *Redirector) serveRedirect(w http.ResponseWriter, r *http.Request, pickCode string) {
	item, err := s.resolveURL(r, pickCode, strings.TrimSpace(r.Header.Get("User-Agent")))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.NotFound(w, r)
		return
	}
	if age := time.Until(item.expireAt); age > 0 {
		w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", int(age.Seconds())))
		w.Header().Set("Expires", item.expireAt.UTC().Format(http.TimeFormat))
	}
	http.Redirect(w, r, item.url, http.StatusFound)
}

// serveProxy 透传模式：取链并回源 115 CDN，流式回传。回源非 2xx 静默重试（最多 2 次）。
func (s *Redirector) serveProxy(w http.ResponseWriter, r *http.Request, pickCode string) {
	const maxRetries = 2
	cacheKey := pickCode + "|" + passthroughUA
	var lastStatus int
	var lastName string
	rng := r.Header.Get("Range")

	if s.localCache != nil {
		if localPath, ok := s.localCache.LocalPath(pickCode); ok {
			if served := s.serveLocalFile(w, r, localPath, pickCode); served {
				return
			}
		}
	}

	if err := originSem.Acquire(r.Context(), 1); err != nil {
		return
	}
	defer originSem.Release(1)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			s.cache.Delete(cacheKey)
		}
		item, err := s.resolveURL(r, pickCode, passthroughUA)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			http.NotFound(w, r)
			return
		}
		lastName = item.name

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, downgradeToHTTP(item.url), nil)
		if err != nil {
			http.Error(w, "构造回源请求失败", http.StatusInternalServerError)
			return
		}
		req.Header.Set("User-Agent", passthroughUA)
		if rng != "" {
			req.Header.Set("Range", rng)
		}

		resp, err := proxyClient.Do(req)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			journal.Error(r.Context(), "回源CDN失败", "文件名", item.name, "错误", err)
			http.Error(w, "回源失败", http.StatusBadGateway)
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastStatus = resp.StatusCode
			if cerr := resp.Body.Close(); cerr != nil {
				journal.Debug(r.Context(), "关闭回源响应体失败", "错误", cerr)
			}
			if attempt < maxRetries {
				journal.Warn(r.Context(), "回源非2xx，将重试", "文件名", item.name,
					"status", resp.StatusCode, "重试", attempt, "range", rng)
			}
			continue
		}

		dst := w.Header()
		maps.Copy(dst, resp.Header)
		dst.Set("X-Origin-Filename", item.name)
		w.WriteHeader(resp.StatusCode)

		streamStart := time.Now()
		written, copyErr := io.Copy(w, resp.Body)
		if cerr := resp.Body.Close(); cerr != nil {
			journal.Debug(r.Context(), "透传后关闭回源响应体失败", "错误", cerr)
		}
		if copyErr != nil {
			if r.Context().Err() != nil {
				return
			}
			journal.Info(r.Context(), "透传中断", "文件名", item.name, "错误", copyErr, "耗时", time.Since(streamStart))
			return
		}
		dur := time.Since(streamStart)
		journal.Debug(r.Context(), "透传完成", "文件名", item.name,
			"大小", fmt.Sprintf("%.1fMB", float64(written)/(1<<20)),
			"速度", fmt.Sprintf("%.1fMB/s", float64(written)/(1<<20)/dur.Seconds()),
			"range", rng, "耗时", dur.Round(time.Millisecond))
		return
	}

	journal.Error(r.Context(), "回源多次重试仍失败", "文件名", lastName, "status", lastStatus, "range", rng)
	http.Error(w, "回源失败", http.StatusBadGateway)
}

// serveLocalFile 透传命中本地缓存：用 http.ServeContent 流式回传（支持 Range/206）。
func (s *Redirector) serveLocalFile(w http.ResponseWriter, r *http.Request, localPath, pickCode string) bool {
	f, err := os.Open(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		journal.Error(r.Context(), "打开本地缓存失败", "路径", localPath, "错误", err)
		http.Error(w, "打开本地缓存失败", http.StatusInternalServerError)
		return true
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			journal.Debug(r.Context(), "关闭本地缓存文件失败", "错误", cerr)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "读取本地缓存元信息失败", http.StatusInternalServerError)
		return true
	}
	dst := w.Header()
	dst.Set("X-Origin-Filename", filepath.Base(localPath))
	dst.Set("115tools-Cache", "HIT")
	http.ServeContent(w, r, filepath.Base(localPath), fi.ModTime(), f)
	return true
}
