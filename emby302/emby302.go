package emby302

import (
	"115tools/config"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	ListenAddr = ":8095"
)

var (
	embyURL      string
	embyApiKey   string
	fontInAssURL string
	strmUrl      string

	sharedTransport = &http.Transport{
		MaxIdleConnsPerHost: 100,
	}
	sharedClient = &http.Client{
		Transport: sharedTransport,
		Timeout:   30 * time.Second,
	}
	redirectClient = &http.Client{
		Transport: sharedTransport,
		Timeout:   30 * time.Second,
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
		if subProxy != nil && strings.Contains(path, "/Subtitles/") && strings.Contains(path, "/Stream.") {
			subProxy.ServeHTTP(w, r)
			return
		}
		if strings.HasSuffix(path, "/PlaybackInfo") {
			handlePlaybackInfo(w, r)
			return
		}
		if strings.Contains(path, "/videos/") && strings.Contains(path, "/original.") {
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

//go:embed patch.js
var jsPatch []byte

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

	w.Header().Set("Content-Type", "application/javascript")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
	w.Write(jsPatch)
}
func handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
	}
	query := r.URL.Query()
	if query.Get("MaxStreamingBitrate") != "200000000" {
		query.Set("MaxStreamingBitrate", "4000000")
	}
	proxyReq := func(query url.Values) (*http.Response, []byte, error) {
		apiURL := fmt.Sprintf("%s%s?%s", embyURL, r.URL.Path, query.Encode())
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, apiURL, bytes.NewReader(bodyBytes))
		maps.Copy(req.Header, r.Header)
		req.Header.Del("Accept-Encoding")
		resp, err := sharedClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp, body, err
	}
	resp, resBody, err := proxyReq(query)
	if err != nil {
		http.Error(w, "Proxy Error", http.StatusBadGateway)
		return
	}
	result := gjson.GetBytes(resBody, "MediaSources.0.DirectStreamUrl")

	if result.Exists() && strings.Contains(result.Raw, "ContainerBitrateExceedsLimit") {
		newQuery := r.URL.Query()
		newQuery.Set("MaxStreamingBitrate", "200000000")
		if r2, b2, err := proxyReq(newQuery); err == nil {
			newResult := gjson.GetBytes(b2, "MediaSources.0.DirectStreamUrl")
			if newResult.Exists() && strings.Contains(newResult.Raw, "original.") {
				resp, resBody = r2, b2
				slog.Info("[PlaybackInfo] 重试高码率成功，已获得 Original 路径")
			} else {
				slog.Warn("[PlaybackInfo] 强制码率重试失败（仍需转码），回退至原始请求")
			}
		}
	}
	res := gjson.GetBytes(resBody, "MediaSources.0")
	path := res.Get("Path").String()
	if path != "" && strings.HasPrefix(path, strmUrl) {
		streamUrl := res.Get("DirectStreamUrl").String()
		if streamUrl != "" {
			newStreamUrl := streamUrl + "&is302=1"
			updatedBody, err := sjson.SetBytes(resBody, "MediaSources.0.DirectStreamUrl", newStreamUrl)
			if err == nil {
				resBody = updatedBody
			}
		}
	}
	maps.Copy(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	w.Write(resBody)
}
func handleVideo302(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(r.URL.Path, "/")
	var itemID string
	for i, segment := range parts {
		if segment == "videos" && i+1 < len(parts) {
			itemID = parts[i+1]
			break
		}
	}
	if itemID == "" {
		return false
	}
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
func fetchMediaFromEmby(ctx context.Context, itemID string, query url.Values) (mediaPath, mediaName string, err error) {
	apiURL := fmt.Sprintf("%s/emby/Items/%s/PlaybackInfo?%s", embyURL, itemID, query.Encode())
	req, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	var resp *http.Response
	if resp, err = sharedClient.Do(req); err != nil {
		return
	}
	defer resp.Body.Close()

	var body []byte
	if body, err = io.ReadAll(resp.Body); err != nil {
		return
	}

	results := gjson.GetManyBytes(body, "MediaSources.0.Path", "MediaSources.0.Name")
	mediaPath = results[0].String()
	mediaName = results[1].String()
	return
}
func getFinalLocation(ctx context.Context, targetURL, ua string) (finalUrl string, err error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	req.Header.Set("User-Agent", ua)
	var resp *http.Response
	if resp, err = redirectClient.Do(req); err != nil {
		return
	}
	defer resp.Body.Close()

	if finalUrl = resp.Header.Get("Location"); finalUrl == "" {
		err = fmt.Errorf("获取的重定向链接为空")
	}
	return
}
