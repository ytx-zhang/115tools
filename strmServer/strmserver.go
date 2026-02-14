package strmServer

import (
	"115tools/open115"
	"log"
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
	// 清理超过10分钟的缓存项
	urlCache.Range(func(key, value any) bool {
		if item, ok := value.(cacheItem); ok && time.Since(item.time) > 10*time.Minute {
			urlCache.Delete(key)
		}
		return true
	})
	pickCode := r.URL.Query().Get("pickcode")
	if pickCode == "" {
		log.Printf("[strm请求] 未找到pickcode")
		http.Error(w, "未找到pickcode", http.StatusBadRequest)
		return
	}
	clientUA := r.Header.Get("User-Agent")
	cacheKey := pickCode + "_" + clientUA
	// 排队判断层
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
	// 读取缓存
	if val, ok := urlCache.Load(cacheKey); ok {
		item := val.(cacheItem)
		log.Printf("[strm请求:缓存命中]: %s | UA: %s", item.name, clientUA)
		http.Redirect(w, r, item.url, http.StatusFound)
		return
	}
	// 请求115
	downloadInfo, err := open115.GetDownloadUrl(r.Context(), pickCode, clientUA)
	if err != nil || downloadInfo.URL == "" {
		errMsg := "获取下载地址失败"
		if err != nil {
			log.Printf("[strm请求] 接口报错: %v", err)
		} else {
			log.Printf("[strm请求] 接口返回链接为空")
			errMsg = "115未返回有效链接"
		}
		http.Error(w, errMsg, http.StatusInternalServerError)
		return
	}
	url := downloadInfo.URL
	name := downloadInfo.FileName
	urlCache.Store(cacheKey, cacheItem{
		url:  url,
		time: time.Now(),
		name: name,
	})
	http.Redirect(w, r, url, http.StatusFound)
	log.Printf("[strm请求:云端获取]:  %s | UA: %s", name, clientUA)
}
