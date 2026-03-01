package strmServer

import (
	"115tools/open115"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type cacheItem struct {
	url  string
	name string
	time time.Time
}

var (
	urlCache     sync.Map
	pendingTasks sync.Map
)

func RedirectToRealURL(w http.ResponseWriter, r *http.Request) {
	urlCache.Range(func(key, value any) bool {
		if item, ok := value.(cacheItem); ok && time.Since(item.time) > 10*time.Minute {
			urlCache.Delete(key)
		}
		return true
	})
	pickCode := r.URL.Query().Get("pickcode")
	if pickCode == "" {
		slog.Warn("[strm请求] 未找到pickcode")
		http.Error(w, "未找到pickcode", http.StatusBadRequest)
		return
	}
	clientUA := r.Header.Get("User-Agent")
	cacheKey := pickCode + "_" + clientUA
	notifier := make(chan struct{})
	existingNotifier, exists := pendingTasks.LoadOrStore(cacheKey, notifier)
	if exists {
		<-existingNotifier.(chan struct{})
	} else {
		defer func() {
			pendingTasks.Delete(cacheKey)
			close(notifier)
		}()
	}
	w.Header().Set("Cache-Control", "public, max-age=600")
	w.Header().Set("Vary", "User-Agent")
	if val, ok := urlCache.Load(cacheKey); ok {
		item := val.(cacheItem)
		http.Redirect(w, r, item.url, http.StatusFound)
		return
	}
	_, url, name, err := open115.GetDownloadUrl(r.Context(), pickCode, clientUA)
	if err != nil || url == "" {
		if err != nil {
			slog.Error("[strm请求] 接口报错", "pickCode", pickCode, "错误信息", err)
		} else {
			slog.Error("[strm请求] 接口返回链接为空", "pickCode", pickCode)
		}
		http.NotFound(w, r)
		return
	}
	urlCache.Store(cacheKey, cacheItem{
		url:  url,
		time: time.Now(),
		name: name,
	})
	http.Redirect(w, r, url, http.StatusFound)
	slog.Info("[strm请求]", "媒体文件名", name, "UA", clientUA)
}
