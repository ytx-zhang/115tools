package emby302

import (
	"115tools/config"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	ListenAddr = ":8095"
)

var (
	embyURL            string
	fontInAssURL       string
	strmUrl            string
	forceOriginalImage bool
	itemIDRegex        = regexp.MustCompile(`/videos/(\w+)/original`)
	subtitleRegex      = regexp.MustCompile(`(?i)Subtitles/.*Stream\.`)

	sharedTransport = &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	sharedClient = &http.Client{
		Transport: sharedTransport,
		Timeout:   15 * time.Second,
	}
	redirectClient = &http.Client{
		Transport: sharedTransport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

func StartEmby302(ctx context.Context) {
	conf := config.Get()
	embyURL = conf.EmbyUrl
	fontInAssURL = conf.FontInAssUrl
	strmUrl = conf.StrmUrl
	if os.Getenv("FORCE_ORIGINAL_IMAGE") == "true" {
		forceOriginalImage = true
	}
	var subProxy *httputil.ReverseProxy
	if fontInAssURL != "" {
		subTarget, _ := url.Parse(fontInAssURL)
		subProxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(subTarget)
			},
			Transport: sharedTransport,
		}
	}

	target, _ := url.Parse(embyURL)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			if xff := pr.In.Header.Get("X-Forwarded-For"); xff != "" {
				pr.Out.Header.Set("X-Forwarded-For", xff)
			}
		},
		Transport: sharedTransport,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/web/modules/htmlvideoplayer/plugin.js" {
			handleJSInject(w, r)
			return
		}
		if subProxy != nil && subtitleRegex.MatchString(path) {
			subProxy.ServeHTTP(w, r)
			return
		}
		if forceOriginalImage && strings.Contains(path, "/Items/") && strings.Contains(path, "/Images/") {
			handleImage(w, r)
			return
		}
		if strings.HasSuffix(path, "/PlaybackInfo") {
			handlePlaybackInfo(w, r)
			return
		}
		if strings.Contains(path, "/videos/") && strings.Contains(path, "/original") {
			if handleVideo302(w, r) {
				return
			}
		}
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    ":8095",
		Handler: mux,
	}

	go func() {
		slog.Info("Emby 302 服务启动在 :8095")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("监听失败", "错误信息", err)
		}
	}()

	<-ctx.Done()

	slog.Info("正在关闭 Emby 302 服务...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("强制关闭服务失败", "错误信息", err)
	}

	slog.Info("Emby 302 服务已退出")
}
func handleJSInject(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequest("GET", embyURL+r.URL.Path, nil)
	maps.Copy(req.Header, r.Header)
	req.Header.Del("Accept-Encoding")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	jsPatch := `
;(function(){
    var fixCross = function(){
        var m = window.defined ? window.defined["modules/htmlvideoplayer/plugin.js"] : null;
        if(m && m.default && m.default.prototype) {
            m.default.prototype.getCrossOriginValue = function(){ return null; };
        } else { setTimeout(fixCross, 100); }
    };
    fixCross();
    var oldSetSrc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'src').set;
    Object.defineProperty(HTMLMediaElement.prototype, 'src', {
        set: function(val) {
            var self = this;
            this._is302Resource = false;
            if (val && val.indexOf('/videos/') !== -1 && val.indexOf('/original') !== -1) {
                fetch(val, { 
                    method: 'HEAD', 
                    redirect: 'manual'
                }).then(function(resp) {
                    if (resp.type === 'opaqueredirect') {
                        self._is302Resource = true;
                        console.log("[302] 探测结果：CDN 直链资源，已激活超时监控");
                    } else {
                        console.log("[302] 探测结果：普通本地资源");
                    }
                }).catch(function(err) {
                    self._is302Resource = false;
                });
            }
            oldSetSrc.call(this, val);
        }
    });
    var lastPauseTime = 0;
    document.addEventListener('pause', function(e) {
        if (e.target.tagName === 'VIDEO') {
            lastPauseTime = Date.now();
        }
    }, true);
    document.addEventListener('play', function(e) {
        var v = e.target;
        if (v.tagName !== 'VIDEO' || v._isRefreshing || !v._is302Resource) return;
        var now = Date.now();
        var idle = lastPauseTime ? (now - lastPauseTime) : 0;
        if (lastPauseTime > 0 && idle > 300000) {
            console.warn("[302] CDN 链接已超时，正在物理重载地址...");
            e.stopImmediatePropagation();
            e.preventDefault();
            var t = v.currentTime;
            var s = v.src; 
            v._isRefreshing = true;
            lastPauseTime = 0;
            v.pause();
            v.src = s;
            v.load();
            var onLoaded = function() {
                v.currentTime = t;
                v.play().then(function(){ 
                    v._isRefreshing = false; 
                });
                v.removeEventListener("loadedmetadata", onLoaded);
            };
            v.addEventListener("loadedmetadata", onLoaded);
        }
    }, true);
})();`

	newBody := append(body, []byte(jsPatch)...)
	w.Header().Set("Content-Type", "application/javascript")
	w.WriteHeader(http.StatusOK)
	w.Write(newBody)
}
func handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	baseUrl, _ := strings.CutSuffix(embyURL, "/")
	var bodyBytes []byte
	if r.Method == "POST" && r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}
	doRequest := func(query url.Values) (*http.Response, []byte, error) {
		apiURL := fmt.Sprintf("%s%s?%s", baseUrl, r.URL.Path, query.Encode())
		var bodyReader io.Reader
		if len(bodyBytes) > 0 {
			bodyReader = strings.NewReader(string(bodyBytes))
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, apiURL, bodyReader)
		maps.Copy(req.Header, r.Header)
		req.Header.Del("Accept-Encoding")
		resp, err := sharedClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		resBody, err := io.ReadAll(resp.Body)
		return resp, resBody, err
	}
	origQuery := r.URL.Query()
	resp1, resBody1, err := doRequest(origQuery)
	if err != nil {
		http.Error(w, "Backend Error", http.StatusBadGateway)
		return
	}

	shouldRetry := false
	if bytes.Contains(resBody1, []byte("DirectStreamUrl")) &&
		bytes.Contains(resBody1, []byte("ContainerBitrateExceedsLimit")) {
		shouldRetry = true
	}
	if shouldRetry {
		newQuery := r.URL.Query()
		newQuery.Set("MaxStreamingBitrate", "200000000")
		resp2, resBody2, err := doRequest(newQuery)
		if err == nil {
			maps.Copy(w.Header(), resp2.Header)
			w.WriteHeader(resp2.StatusCode)
			w.Write(resBody2)
			slog.Info("[PlaybackInfo] 检测到码率限制转码，重试高码率请求")
			return
		}
	}
	maps.Copy(w.Header(), resp1.Header)
	w.WriteHeader(resp1.StatusCode)
	w.Write(resBody1)
}
func handleImage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	pos := query.Get("PositionTicks")
	newQuery := url.Values{}
	if pos != "" {
		newQuery.Set("PositionTicks", pos)
	}
	targetPath := fmt.Sprintf("%s%s", strings.TrimSuffix(embyURL, "/"), r.URL.Path)
	if qStr := newQuery.Encode(); qStr != "" {
		targetPath += "?" + qStr
	}
	req, _ := http.NewRequestWithContext(r.Context(), "GET", targetPath, nil)
	maps.Copy(req.Header, r.Header)
	resp, err := sharedClient.Do(req)
	if err != nil {
		http.Error(w, "Backend Error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
func handleVideo302(w http.ResponseWriter, r *http.Request) bool {
	path := strings.ToLower(r.URL.Path)
	matches := itemIDRegex.FindStringSubmatch(path)
	if len(matches) > 1 {
		itemID := matches[1]
		mediaPath, mediaName, err := fetchMediaFromEmby(r.Context(), itemID, r.URL.Query())
		if err != nil {
			slog.Error("[302 失败] 获取媒体信息出错", "ItemID", itemID, "错误", err)
			return false
		}
		if !strings.HasPrefix(mediaPath, strmUrl) {
			slog.Info("[302 跳过] 非 strm 路径", "媒体名称", mediaName, "路径", mediaPath)
			return false
		}
		finalURL, err := getFinalLocation(r.Context(), mediaPath, r.Header.Get("User-Agent"))
		if err != nil {
			slog.Error("[302 失败] 获取直链失败", "媒体名称", mediaName, "路径", mediaPath, "错误", err)
			return false
		}
		slog.Info("[302 成功]", "媒体名称", mediaName)
		http.Redirect(w, r, finalURL, http.StatusFound)
		return true
	}
	return false
}
func fetchMediaFromEmby(ctx context.Context, itemID string, query url.Values) (string, string, error) {
	apiURL := fmt.Sprintf("%s/emby/Items/%s/PlaybackInfo?%s", embyURL, itemID, query.Encode())
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", "", err
	}

	resp, err := sharedClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var result struct {
		MediaSources []struct {
			Path string `json:"Path"`
			Name string `json:"Name"`
		} `json:"MediaSources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}
	if len(result.MediaSources) > 0 {
		source := result.MediaSources[0]
		return source.Path, source.Name, nil
	}
	return "", "", fmt.Errorf("no source found for this itemID")
}
func getFinalLocation(ctx context.Context, targetURL, ua string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	req.Header.Set("User-Agent", ua)
	resp, err := redirectClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}
	return "", fmt.Errorf("no redirect location found")
}
