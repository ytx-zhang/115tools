package emby302

import (
	"115tools/open115"
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
	strmUrl  string
	itemIDRegex   = regexp.MustCompile(`/videos/(\w+)/original`)
	subtitleRegex = regexp.MustCompile(`(?i)Subtitles/.*Stream\.`)
)

type embyMedia struct {
    Path string `json:"Path"`
    Name string `json:"Name"`
}

func StartEmby302() {
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

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
				media, err := fetchMediaFromEmby(itemID, r.URL.Query())

				if err == nil && strings.HasPrefix(media.Path, strmUrl) {

					finalURL, err := getFinalLocation(media.Path, r.Header.Get("User-Agent"))
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

	log.Printf("Emby 302 已启动: %s", ListenAddr)
	log.Fatal(http.ListenAndServe(ListenAddr, nil))
}

func fetchMediaFromEmby(itemID string, query url.Values) (*embyMedia, error) {
	baseUrl, _ := strings.CutSuffix(embyURL, "/")
	apiURL := fmt.Sprintf("%s/emby/Items/%s/PlaybackInfo?%s",
		baseUrl, itemID, query.Encode())

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 5 * time.Second}
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

// getFinalLocation 保持不变
func getFinalLocation(targetURL, ua string) (string, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest("GET", targetURL, nil)
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

// handleJSInject 注入
func handleJSInject(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequest("GET", embyURL+r.URL.Path, nil)
	// 复制原始请求头
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
    // 1. 跨域补丁：解决直链播放可能存在的跨域限制
    var fixCross = function(){
        var m = window.defined ? window.defined["modules/htmlvideoplayer/plugin.js"] : null;
        if(m && m.default && m.default.prototype) {
            m.default.prototype.getCrossOriginValue = function(){ return null; };
        } else { setTimeout(fixCross, 100); }
    };
    fixCross();

    // 2. 核心劫持：重置逻辑
    // 通过劫持 HTMLMediaElement 的 src 属性，确保每次切换视频时状态都会重置
    var oldSetSrc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'src').set;
    Object.defineProperty(HTMLMediaElement.prototype, 'src', {
        set: function(val) {
            var self = this;
            this._is302Resource = false; // 初始状态设为 false
            
            // 过滤：仅对视频流地址进行探测
            if (val && val.indexOf('/videos/') !== -1 && val.indexOf('/original') !== -1) {
                // 主动发起 HEAD 请求探测
                fetch(val, { 
                    method: 'HEAD', 
                    redirect: 'manual' // 【关键】：手动拦截重定向，不跟随跳转到 115
                }).then(function(resp) {
                    // 如果 resp.type 是 opaqueredirect，说明后端执行了 302 重定向
                    // 此时不需要读取任何 Header，跳转行为本身就是最好的证明
                    if (resp.type === 'opaqueredirect') {
                        self._is302Resource = true;
                        console.log("[302] 探测结果：CDN 直链资源，已激活超时监控");
                    } else {
                        console.log("[302] 探测结果：普通本地资源");
                    }
                }).catch(function(err) {
                    // 即使探测失败（如网络波动），也不影响正常播放逻辑
                    self._is302Resource = false;
                });
            }
            oldSetSrc.call(this, val);
        }
    });

    // 3. 记录最后暂停时间
    var lastPauseTime = 0;
    document.addEventListener('pause', function(e) {
        if (e.target.tagName === 'VIDEO') {
            lastPauseTime = Date.now();
        }
    }, true);

    // 4. 拦截播放请求
    document.addEventListener('play', function(e) {
        var v = e.target;
        // 仅在视频标签、非刷新状态、且探测结果为 302 资源时才执行拦截
        if (v.tagName !== 'VIDEO' || v._isRefreshing || !v._is302Resource) return;

        var now = Date.now();
        var idle = lastPauseTime ? (now - lastPauseTime) : 0;

        // 暂停超过 5 分钟 (300,000 毫秒)
        if (lastPauseTime > 0 && idle > 300000) {
            console.warn("[302] CDN 链接已超时，正在物理重载地址...");
            
            e.stopImmediatePropagation();
            e.preventDefault();

            var t = v.currentTime;
            var s = v.src; 
            v._isRefreshing = true;
            lastPauseTime = 0;

            // 重新加载流程
            v.pause();
            v.src = s; // 重新赋值会再次触发后端的 302 逻辑，获取最新的 115 链接
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
