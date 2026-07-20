package strmServer

import (
	"115tools/drive"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cacheItem struct {
	url      string
	name     string
	expireAt time.Time
}

type Server struct {
	api          *drive.Open115
	cache        sync.Map
	pendingTasks sync.Map
}

func New(api *drive.Open115) *Server {
	return &Server{
		api: api,
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

	cacheKey := pickCode + "_" + clientUA
	if val, ok := s.cache.Load(cacheKey); ok {
		item := val.(cacheItem)
		// 惰性过期：命中但已过期则删除并当 miss，避免使用 time.AfterFunc 导致的
		// “刷新后旧定时器仍会误删”以及无滑动过期的问题。
		if time.Now().After(item.expireAt) {
			s.cache.Delete(cacheKey)
		} else {
			slog.Debug("[strm后端] 缓存命中", "媒体名称", item.name, "UA", clientUA)
			http.Redirect(w, r, item.url, http.StatusFound)
			return
		}
	}

	notifier := make(chan struct{})
	existingNotifier, exists := s.pendingTasks.LoadOrStore(cacheKey, notifier)
	if exists {
		ch, ok := existingNotifier.(chan struct{})
		if !ok {
			slog.Error("[strm后端] pendingTasks 类型断言失败")
			http.Error(w, "内部错误", http.StatusInternalServerError)
			return
		}
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		}
		if val, ok := s.cache.Load(cacheKey); ok {
			item := val.(cacheItem)
			if !time.Now().After(item.expireAt) {
				http.Redirect(w, r, item.url, http.StatusFound)
				return
			}
			s.cache.Delete(cacheKey)
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

	s.cache.Store(cacheKey, cacheItem{url: info.Url, name: info.Name, expireAt: time.Now().Add(expiration)})

	slog.Debug("[strm后端] 获取新地址", "名称", info.Name, "UA", clientUA, "缓存时长", expiration.Round(time.Second).String())
	http.Redirect(w, r, info.Url, http.StatusFound)
}
