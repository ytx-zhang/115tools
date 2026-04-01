package strmServer

import (
	"115tools/drive"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cacheItem struct {
	url  string
	name string
}

type Server struct {
	api          *drive.Open115
	proxy        *httputil.ReverseProxy
	cache        sync.Map
	pendingTasks sync.Map
}

func New(api *drive.Open115) *Server {
	return &Server{
		api: api,
		proxy: &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				var nginxAddr, _ = url.Parse("http://127.0.0.1:8081")
				pr.SetURL(nginxAddr)
			},
		},
	}
}
func (s *Server) RedirectToRealURL(w http.ResponseWriter, r *http.Request) {
	pickCode := r.URL.Query().Get("pickcode")
	if pickCode == "" {
		slog.Warn("[strm后端] 未找到pickcode")
		http.Error(w, "未找到pickcode", http.StatusBadRequest)
		return
	}

	clientUA := strings.TrimSpace(r.Header.Get("User-Agent"))
	isProxy := clientUA == "" || strings.HasPrefix(clientUA, "Lavf")
	if isProxy {
		slog.Debug("[strm后端] 使用 Nginx 代理", "请求地址", r.URL.String(), "UA", clientUA)
		s.proxy.ServeHTTP(w, r)
		return
	}

	cacheKey := pickCode + "_" + clientUA
	if val, ok := s.cache.Load(cacheKey); ok {
		item := val.(cacheItem)
		slog.Debug("[strm后端] 缓存命中", "媒体名称", item.name, "UA", clientUA)
		http.Redirect(w, r, item.url, http.StatusFound)
		return
	}

	notifier := make(chan struct{})
	existingNotifier, exists := s.pendingTasks.LoadOrStore(cacheKey, notifier)
	if exists {
		select {
		case <-existingNotifier.(chan struct{}):
		case <-r.Context().Done():
			return
		}
		if val, ok := s.cache.Load(cacheKey); ok {
			item := val.(cacheItem)
			http.Redirect(w, r, item.url, http.StatusFound)
			return
		}
		http.NotFound(w, r)
		return
	}
	defer func() {
		s.pendingTasks.Delete(cacheKey)
		close(notifier)
	}()

	info, err := s.api.GetDownloadUrl(r.Context(), pickCode, clientUA)
	if err != nil || info.Url == "" {
		slog.Error("[strm后端] 115接口报错", "err", err)
		http.NotFound(w, r)
		return
	}

	s.cache.Store(cacheKey, cacheItem{url: info.Url, name: info.Name})

	expiration := 30 * time.Minute
	if u, err := url.Parse(info.Url); err == nil {
		tStr := u.Query().Get("t")
		if tStr != "" {
			if tInt, err := strconv.ParseInt(tStr, 10, 64); err == nil {
				target := time.Unix(tInt, 0).Add(-5 * time.Minute)
				remaining := time.Until(target)
				if remaining > 0 {
					expiration = remaining
				}
			}
		}
	}
	time.AfterFunc(expiration, func() {
		s.cache.Delete(cacheKey)
	})

	slog.Debug("[strm后端] 获取新地址", "名称", info.Name, "UA", clientUA, "缓存时长", expiration.Round(time.Second).String())
	http.Redirect(w, r, info.Url, http.StatusFound)
}
