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

// loadCache 从缓存读取并做类型断言，消除重复的 Load + 断言样板。
// 注意缓存里存的是指针（*cacheItem），这样 storeCache 注册的 time.AfterFunc
// 才能通过指针比对判断“自己要不要删”，从而解决刷新后旧定时器误删新条目的问题。
func (s *Server) loadCache(key string) (*cacheItem, bool) {
	val, ok := s.cache.Load(key)
	if !ok {
		return nil, false
	}
	item, ok := val.(*cacheItem)
	return item, ok
}

// storeCache 写入缓存并注册一个在过期时刻触发的 time.AfterFunc 负责删除，
// 无需周期性扫描。关键点：AfterFunc 捕获的是本次写入的指针；若之后同 key 被
// 刷新（写入新指针），旧定时器回调里比对指针不相等就放弃删除，从而解决
// “刷新后旧定时器误删新条目”这一当初不用 AfterFunc 的隐患。
func (s *Server) storeCache(key, url, name string, expiration time.Duration) {
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
func (s *Server) RedirectToRealURL(w http.ResponseWriter, r *http.Request) {
	pickCode := r.URL.Query().Get("pickcode")
	if pickCode == "" {
		slog.Warn("[strm后端] 未找到pickcode")
		http.Error(w, "未找到pickcode", http.StatusBadRequest)
		return
	}

	clientUA := strings.TrimSpace(r.Header.Get("User-Agent"))

	cacheKey := pickCode + "_" + clientUA
	// 命中：过期则当作未命中（删除由 storeCache 注册的 time.AfterFunc 处理）。
	if item, ok := s.loadCache(cacheKey); ok && !time.Now().After(item.expireAt) {
		slog.Debug("[strm后端] 缓存命中", "媒体名称", item.name, "UA", clientUA)
		http.Redirect(w, r, item.url, http.StatusFound)
		return
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
		if item, ok := s.loadCache(cacheKey); ok && !time.Now().After(item.expireAt) {
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
	if err != nil || info == nil || info.Url == "" {
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

	s.storeCache(cacheKey, info.Url, info.Name, expiration)

	slog.Info("[strm后端] 获取新地址", "名称", info.Name, "UA", clientUA, "缓存时长", expiration.Round(time.Second).String())
	http.Redirect(w, r, info.Url, http.StatusFound)
}
