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

const (
	// cleanupInterval 后台清理周期：定期回收过期/滞留的条目，防止内存无界增长。
	cleanupInterval = 5 * time.Minute
	// pendingMaxAge 兜底阈值：pendingTasks 正常随请求结束（defer）清理；
	// 仅当某条目远超过正常请求时长时才由清理协程回收，避免极端情况下的泄漏。
	pendingMaxAge = 1 * time.Hour
)

type cacheItem struct {
	url      string
	name     string
	expireAt time.Time
}

// pendingItem 包裹等待通知的 channel，并附带创建时间以便清理协程识别滞留条目。
type pendingItem struct {
	ch        chan struct{}
	createdAt time.Time
}

type Server struct {
	api          *drive.Open115
	cache        sync.Map
	pendingTasks sync.Map

	// 后台清理协程的生命周期控制（服务启动时创建，关闭时停止）。
	stopCh    chan struct{}
	stopOnce  sync.Once
	cleanupWg sync.WaitGroup
}

func New(api *drive.Open115) *Server {
	s := &Server{
		api:    api,
		stopCh: make(chan struct{}),
	}
	// 随服务启动后台清理协程，按 cleanupInterval 周期回收过期条目。
	s.cleanupWg.Add(1)
	go s.cleanupLoop()
	return s
}

// Stop 停止后台清理协程并等待其退出，应在服务关闭时调用，避免协程泄漏/竞态。
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.cleanupWg.Wait()
}

// cleanupLoop 后台清理循环：周期触发 sweep，收到停止信号后退出。
func (s *Server) cleanupLoop() {
	defer s.cleanupWg.Done()
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

// sweep 删除已过期的 cache 条目与滞留过久的 pendingTasks 条目。
// 使用 sync.Map 自带的并发安全 Range/Delete，无需额外锁。
func (s *Server) sweep() {
	now := time.Now()
	s.cache.Range(func(key, value any) bool {
		item, ok := value.(cacheItem)
		if !ok || now.After(item.expireAt) {
			s.cache.Delete(key)
		}
		return true
	})
	s.pendingTasks.Range(func(key, value any) bool {
		p, ok := value.(pendingItem)
		if !ok || now.Sub(p.createdAt) > pendingMaxAge {
			s.pendingTasks.Delete(key)
		}
		return true
	})
}

// loadCache 从缓存读取并做类型断言，消除重复的 Load + 断言样板。
func (s *Server) loadCache(key string) (cacheItem, bool) {
	val, ok := s.cache.Load(key)
	if !ok {
		return cacheItem{}, false
	}
	item, ok := val.(cacheItem)
	return item, ok
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
	if item, ok := s.loadCache(cacheKey); ok {
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
	existingNotifier, exists := s.pendingTasks.LoadOrStore(cacheKey, pendingItem{ch: notifier, createdAt: time.Now()})
	if exists {
		p, ok := existingNotifier.(pendingItem)
		if !ok {
			slog.Error("[strm后端] pendingTasks 类型断言失败")
			http.Error(w, "内部错误", http.StatusInternalServerError)
			return
		}
		select {
		case <-p.ch:
		case <-r.Context().Done():
			return
		}
		if item, ok := s.loadCache(cacheKey); ok {
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
		// 仅当 pendingTasks 仍指向本请求的 notifier 时才删除，避免清理协程或
		// 其他请求已替换/删除该 key 时误删他人的条目。
		if cur, ok := s.pendingTasks.Load(cacheKey); ok {
			if p, ok := cur.(pendingItem); ok && p.ch == notifier {
				s.pendingTasks.Delete(cacheKey)
			}
		}
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

	s.cache.Store(cacheKey, cacheItem{url: info.Url, name: info.Name, expireAt: time.Now().Add(expiration)})

	slog.Info("[strm后端] 获取新地址", "名称", info.Name, "UA", clientUA, "缓存时长", expiration.Round(time.Second).String())
	http.Redirect(w, r, info.Url, http.StatusFound)
}
