package emby302

import (
	"115tools/open115"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	ListenAddr = ":8095"
)

var (
	embyURL       string
	fontInAssURL  string
	strmUrl       string
	itemIDRegex   = regexp.MustCompile(`/videos/(\w+)/original`)
	subtitleRegex = regexp.MustCompile(`(?i)Subtitles/.*Stream\.`)
)

type embyMedia struct {
	Path string `json:"Path"`
	Name string `json:"Name"`
}

func StartEmby302(ctx context.Context) {
	conf := open115.Conf.Load()
	embyURL = conf.EmbyUrl
	fontInAssURL = conf.FontInAssUrl
	strmUrl = conf.StrmUrl
	var subProxy *httputil.ReverseProxy
	if fontInAssURL != "" {
		subTarget, _ := url.Parse(fontInAssURL)
		subProxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(subTarget)
			},
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
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.ToLower(r.URL.Path)

		if path == "/web/modules/htmlvideoplayer/plugin.js" {
			handleJSInject(w, r)
			return
		}

		if subProxy != nil && subtitleRegex.MatchString(path) {
			log.Printf("[Subtitle Proxy] Path: %s -> Target: %s", path, fontInAssURL)
			subProxy.ServeHTTP(w, r)
			return
		}

		// 拦截所有包含 /Images/ 的请求
		if strings.Contains(path, "/items/") && strings.Contains(path, "/images/") {
			if r.URL.RawQuery != "" {
				query := r.URL.Query()
				if query.Get("quality") != "" {
					query.Set("quality", "100")
					r.URL.RawQuery = query.Encode()
				}
			}
		}

		// 拦截视频流 original 请求
		if strings.Contains(path, "/videos/") && strings.Contains(path, "/original") {
			matches := itemIDRegex.FindStringSubmatch(path)
			if len(matches) > 1 {
				itemID := matches[1]

				// 实时向 Emby 获取 MediaInfo
				media, err := fetchMediaFromEmby(r.Context(), itemID, r.URL.Query())

				if err == nil && strings.HasPrefix(media.Path, strmUrl) {

					finalURL, err := getFinalLocation(r.Context(), media.Path, r.Header.Get("User-Agent"))
					if err == nil && finalURL != "" {
						log.Printf("[302 Success] %s", media.Name)
						http.Redirect(w, r, finalURL, http.StatusFound)
						return
					} else {
						log.Printf("[302 Failed/Timeout] %s, Error: %v", media.Name, err)
					}
				}

				if err == nil {
					log.Printf("[Fallback] [%s] 不符合 302 条件)", media.Name)
				}
			}
		}

		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    ListenAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("Emby 302 服务已启动: %s", ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("监听失败: %s\n", err)
		}
	}()

	<-ctx.Done()

	log.Println("正在关闭 Emby 302 服务...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("强制关闭服务失败: %v", err)
	}

	log.Println("Emby 302 服务已退出")
}

func fetchMediaFromEmby(ctx context.Context, itemID string, query url.Values) (*embyMedia, error) {
	baseUrl, _ := strings.CutSuffix(embyURL, "/")
	apiURL := fmt.Sprintf("%s/emby/Items/%s/PlaybackInfo?%s",
		baseUrl, itemID, query.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		MediaSources []embyMedia `json:"MediaSources"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.MediaSources) > 0 {
		return &result.MediaSources[0], nil
	}
	return nil, fmt.Errorf("no source found for this itemID")
}

func getFinalLocation(ctx context.Context, targetURL, ua string) (string, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}
	return "", fmt.Errorf("no redirect location found")
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
