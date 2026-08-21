// Package web 的 /download 直链重定向器：把 115 的 pickcode 转成真实直链。
// 按 UA 分流：空 UA/Lavf 前缀走透传（回源 115 CDN 流式回传供 nginx 切片缓存），
// 其余走 302。
package web

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
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

// passthroughUA 透传模式取链 & 回源 CDN 统一使用的 UA。
// ⚠️ 115 直链绑定取链 UA，回源必须与取链一致，故此值取链和回源两处共用，勿分叉。
// 实测：取链虽可用 115Browser UA，但回源 CDN 用该 UA 会 403，统一固定 Fuck115。
const passthroughUA = "Fuck115"

// Redirector 把 115 网盘的 pickcode 转成真实直链。按请求 UA 分流：
//   - UA 为空或以 "Lavf" 开头 → 透传模式：后端回源 115 CDN 流式回传（供前置 nginx 切片缓存）；
//   - 其余 UA → 302 模式：直接重定向到 CDN 直链，由客户端自行跳转。
//
// 直链有时效，按 URL 的 t 参数动态计算缓存时长，并对同一 key 的并发做 singleflight 合并。
type Redirector struct {
	// api 115 客户端实例（直接持有，来自 App.API；当前实例固定不变）。
	api   *drive.Client
	cache sync.Map // URL 直链缓存（key=pickcode|ua），见 resolveURL/loadCache/storeCache
	// localCache 透传本地缓存层：命中本地文件即可跳过 115 上游回源（nil 时禁用本地命中）。
	localCache *cache.Cache
	sf         singleflight.Group
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

// originSem 回源 115 上游并发门禁：加权信号量，同一时刻最多 2 个回源请求在途。
// 这是 Go 官方扩展库 x/sync/semaphore 的计数信号量语义：
//   - 突发：2 个请求同时进入（容量内瞬时并发）；
//   - 之后：第 3 个起 Acquire 阻塞严格排队，前一个 Release 才放行下一个（事件驱动，
//     不是固定时间间隔），即只顺序下载；
//   - 空闲：无在途请求时信号量回满 2 个权重，下次又能突发 2 个。
var originSem = semaphore.NewWeighted(2)

// NewRedirector 创建直链重定向器。api 为 115 客户端实例（直接持有，来自 App.API）；
// localCache 为透传本地缓存层（nil 时禁用本地命中，仍走 115 上游回源）。
func NewRedirector(api *drive.Client, localCache *cache.Cache) *Redirector {
	return &Redirector{api: api, localCache: localCache}
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
		logs.Debug(logs.ModuleDrive, "缓存命中", "媒体名称", item.name, "UA", ua)
		return item, nil
	}

	// 单飞：同一 key 的并发请求合并为一次 115 调用，所有等待者共享同一份结果。
	ch := s.sf.DoChan(cacheKey, func() (any, error) {
		t0 := time.Now()
		info, err := s.api.GetDownloadUrl(r.Context(), pickCode, ua, "")
		if err != nil {
			logs.Error(logs.ModuleDrive, "获取直链失败", "错误", err)
			return nil, err
		}
		if info == nil || info.Url == "" {
			logs.Error(logs.ModuleDrive, "获取直链失败", "错误", errEmptyURL)
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
		// 取链结果只打结束一条 → Info（用户关注播放取链是否成功）
		logs.Info(logs.ModuleDrive, "获取新地址", "文件名", info.Name, "UA", ua,
			"缓存时长", expiration.Round(time.Second).String(), "耗时", time.Since(t0))
		return &cacheItem{url: info.Url, name: info.Name, expireAt: time.Now().Add(expiration)}, nil
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
		logs.Warn(logs.ModuleDrive, "未找到pickcode")
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
	// 把真实缓存时长透传给客户端：302 默认不被浏览器缓存，显式带 max-age 后，
	// 浏览器在直链有效期内直接复用本次跳转目标，不再回源 115tools（减少取链与单飞压力）。
	// max-age 基于 115 直链真实过期（已含 -5min 余量），不会让客户端跑到失效直链。
	if age := time.Until(item.expireAt); age > 0 {
		w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", int(age.Seconds())))
		w.Header().Set("Expires", item.expireAt.UTC().Format(http.TimeFormat))
	}
	http.Redirect(w, r, item.url, http.StatusFound)
}

// serveProxy 透传模式：用 passthroughUA 取链并回源 115 CDN，把 CDN 响应（状态码 +
// 全部响应头 + body）原样流式回传前置 nginx 做切片缓存。⚠️ 回程绝不重算任何长度
// 字段（nginx slice 靠 Content-Range 的 TOTAL 切分整文件）。
// 回源非 2xx 静默重试（最多 2 次，重试前清直链缓存取新链）。
// 回源 115 上游经信号量 originSem（容量 2）准入：突发 2 个并发下载允许，
// 第 3 个起严格 FIFO 排队（只顺序下载），一个完成才放下一个；全部空闲后信号量回满又突发。
func (s *Redirector) serveProxy(w http.ResponseWriter, r *http.Request, pickCode string) {
	const maxRetries = 2
	cacheKey := pickCode + "|" + passthroughUA
	var lastStatus int
	var lastName string
	rng := r.Header.Get("Range")

	// 本地缓存命中：直接流式回传本地文件（支持 Range/206），跳过 115 上游回源与信号量准入。
	// 仅透传模式生效（302 分支仍走 CDN）。缓存未启用或 miss 才走上游。
	// 命中判定与打开间极小窗口内被清理协程删除（IsNotExist）→ 返回 false 回退上游回源。
	if s.localCache != nil {
		if localPath, ok := s.localCache.LocalPath(pickCode); ok {
			if served := s.serveLocalFile(w, r, localPath, pickCode); served {
				return
			}
		}
	}

	// 信号量准入（覆盖整个回源生命周期：取链+回源+下载）：突发最多 2 个在途，
	// 第 3 个起 Acquire 阻塞严格排队（只顺序下载）；等待期客户端断开即返回。
	// 重试循环在同一连接内，只占一个槽（defer 释放）。
	if err := originSem.Acquire(r.Context(), 1); err != nil {
		return
	}
	defer originSem.Release(1)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			s.cache.Delete(cacheKey) // 旧直链可能已失效，重试前取新链
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

		// 回源降级为 http 省掉 TLS 解密；302 分支保持 https
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, downgradeToHTTP(item.url), nil)
		if err != nil {
			http.Error(w, "构造回源请求失败", http.StatusInternalServerError)
			return
		}
		req.Header.Set("User-Agent", passthroughUA)
		if rng != "" {
			req.Header.Set("Range", rng) // 原样转发 nginx 的切片 Range
		}

		resp, err := proxyClient.Do(req)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			logs.Error(logs.ModuleDrive, "回源CDN失败", "文件名", item.name, "错误", err)
			http.Error(w, "回源失败", http.StatusBadGateway)
			return
		}

		// 仅 2xx 透传；其余关 body 重试
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastStatus = resp.StatusCode
			if cerr := resp.Body.Close(); cerr != nil {
				logs.Debug(logs.ModuleDrive, "关闭回源响应体失败", "错误", cerr)
			}
			if attempt < maxRetries {
				// 还有重试机会：警告一条即可，带 range 便于定位哪个分片
				logs.Warn(logs.ModuleDrive, "回源非2xx，将重试",
					"文件名", item.name, "status", resp.StatusCode,
					"重试", attempt, "range", rng)
			}
			continue
		}

		// 原样透传 CDN 全部响应头（含同名多值），绝不重算长度字段
		// maps.Copy 整体复制（底层 []string 保留多值），dst 此时为空 Header，与逐条 Add 等价
		dst := w.Header()
		maps.Copy(dst, resp.Header)
		// 附加原始文件名头，UTF-8 原样写入，供客户端（ffprobe/下载器）直接读取
		dst.Set("X-Origin-Filename", item.name)

		w.WriteHeader(resp.StatusCode) // 原样透传 200 / 206
		streamStart := time.Now()
		written, copyErr := io.Copy(w, resp.Body)
		if cerr := resp.Body.Close(); cerr != nil {
			logs.Debug(logs.ModuleDrive, "透传后关闭回源响应体失败", "错误", cerr)
		}
		if copyErr != nil {
			if r.Context().Err() != nil {
				return // 客户端断开，正常
			}
			logs.Info(logs.ModuleDrive, "透传中断", "文件名", item.name, "错误", copyErr, "耗时", time.Since(streamStart))
			return
		}
		// 透传结果只打结束一条 → Info（用户关注播放透传是否完成）；status 异常时由 Warn/Error 体现，正常不显示
		dur := time.Since(streamStart)
		sizeMB := float64(written) / (1 << 20)
		speed := sizeMB / dur.Seconds()
		logs.Info(logs.ModuleDrive, "透传完成", "文件名", item.name,
			"大小", fmt.Sprintf("%.1fMB", sizeMB),
			"速度", fmt.Sprintf("%.1fMB/s", speed), "range", rng, "耗时", dur.Round(time.Millisecond))
		return
	}

	// 用尽重试仍非 2xx：打印错误并 502 返回
	logs.Error(logs.ModuleDrive, "回源多次重试仍失败",
		"文件名", lastName, "status", lastStatus, "重试", maxRetries, "range", rng)
	http.Error(w, "回源失败", http.StatusBadGateway)
}

// serveLocalFile 透传命中本地缓存：直接流式回传本地文件，跳过 115 上游回源。
// 用标准库 http.ServeContent 自动处理 Range 请求（返回 206），供前置 nginx 切片缓存。
// 不消耗缓存（文件保留期由 cache.Cache 清理协程控制），也不占 originSem（本地磁盘 IO 不挤占上游）。
// 返回是否已完成响应：仅当命中判定后文件被并发清理（IsNotExist）返回 false，调用方应回退上游回源。
func (s *Redirector) serveLocalFile(w http.ResponseWriter, r *http.Request, localPath, pickCode string) bool {
	f, err := os.Open(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 命中判定后被清理协程并发删除：交回上游回源（调用方继续走回源逻辑）
			logs.Debug(logs.ModuleDrive, "本地缓存命中判定后被清理", "路径", localPath)
			return false
		}
		logs.Error(logs.ModuleDrive, "打开本地缓存失败", "路径", localPath, "错误", err)
		http.Error(w, "打开本地缓存失败", http.StatusInternalServerError)
		return true
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			logs.Debug(logs.ModuleDrive, "关闭本地缓存文件失败", "错误", cerr)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		logs.Error(logs.ModuleDrive, "读取本地缓存元信息失败", "路径", localPath, "错误", err)
		http.Error(w, "读取本地缓存元信息失败", http.StatusInternalServerError)
		return true
	}
	dst := w.Header()
	dst.Set("X-Origin-Filename", filepath.Base(localPath))
	// ⚠️ 用自定义头名而非 X-Cache：避免与 115 CDN 回源透传的头重名。nginx 据此头跳过写切片缓存（本地命中直接透传）。
	dst.Set("115tools-Cache", "HIT")
	// 与上游"透传完成"日志字段对齐（含 range），便于同一请求链路对照排查
	logs.Debug(logs.ModuleDrive, "透传命中本地缓存", "文件名", filepath.Base(localPath),
		"pickcode", pickCode, "大小", fi.Size(), "range", r.Header.Get("Range"))
	http.ServeContent(w, r, filepath.Base(localPath), fi.ModTime(), f)
	return true
}
