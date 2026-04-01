package emby302

import (
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
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
)

const (
	ListenAddr = ":8095"
)

var (
	embyURL         string
	sharedTransport = &http.Transport{
		MaxIdleConnsPerHost: 100,
	}
	sharedClient = &http.Client{
		Transport: sharedTransport,
	}
	redirectClient = &http.Client{
		Transport: sharedTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	sem = make(chan struct{}, 1)
)

func StartEmby302(ctx context.Context, embyurl string) {
	embyURL = embyurl
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
	mux.HandleFunc("/web/modules/htmlvideoplayer/plugin.js", handleJSInject)
	mux.HandleFunc("/emby/Items/{itemID}/PlaybackInfo", handlePlaybackInfo)
	mux.HandleFunc("/emby/videos/{itemID}/{_}", func(w http.ResponseWriter, r *http.Request) {
		if handleVideo302(w, r) {
			return
		}
		proxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("/Videos/{itemID}/{mediaSource}/Attachments/{attachmentID}/Stream", handleAttachment)

	mux.HandleFunc("/", proxy.ServeHTTP)
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

	if br := query.Get("MaxStreamingBitrate"); br != "" {
		if bitrate, err := strconv.ParseInt(br, 10, 64); err == nil && bitrate < 200000000 {
			query.Set("MaxStreamingBitrate", "4000000")
		}
	}

	proxyReq := func(q url.Values) (*http.Response, []byte, error) {
		apiURL := embyURL + r.URL.Path + "?" + q.Encode()
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
		return
	}

	root, err := sonic.Get(resBody)
	if err != nil {
		w.Write(resBody)
		return
	}

	sourcesNode := root.Get("MediaSources")
	needsRetry := false
	sourcesNode.ForEach(func(path ast.Sequence, node *ast.Node) bool {
		protocol, _ := node.Get("Protocol").String()
		dsUrl, _ := node.Get("DirectStreamUrl").String()
		if protocol == "Http" && strings.Contains(dsUrl, "ContainerBitrateExceedsLimit") {
			needsRetry = true
			return false
		}
		return true
	})
	if needsRetry {
		slog.Debug("[302] 因码率限制转码,重试PlaybackInfo", "itemID", r.PathValue("itemID"))
		query.Del("MaxStreamingBitrate")
		if _, b2, err := proxyReq(query); err == nil {
			resBody = b2
			root, _ = sonic.Get(resBody)
			sourcesNode = root.Get("MediaSources")
		}
	}
	sourcesNode.ForEach(func(path ast.Sequence, node *ast.Node) bool {
		protocol, _ := node.Get("Protocol").String()
		streamURL, _ := node.Get("DirectStreamUrl").String()

		if protocol == "Http" && strings.Contains(streamURL, "original.") {
			pathStr, _ := node.Get("Path").String()
			name, _ := node.Get("Name").String()

			u, _ := url.Parse(streamURL)
			q := u.Query()
			q.Set("mediaPath", pathStr)
			q.Set("mediaName", name)
			u.RawQuery = q.Encode()
			slog.Debug("[302] 发现原始流,写入302标记", "媒体名称", name)
			node.Set("DirectStreamUrl", ast.NewString(u.String()))
			go warmUpMedia(pathStr, r.Header.Get("User-Agent"), name)
		}
		return true
	})
	finalBody, _ := root.MarshalJSON()
	maps.Copy(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	w.Write(finalBody)
}

func warmUpMedia(targetPath, ua, name string) {
	slog.Debug("[302] 预热需要302的媒体", "媒体名称", name)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", targetPath, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", ua)
	resp, err := redirectClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}
func handleVideo302(w http.ResponseWriter, r *http.Request) bool {
	u := r.URL.Query()
	mediaPath := u.Get("mediaPath")
	if mediaPath == "" {
		return false
	}
	mediaName := u.Get("mediaName")
	finalURL, err := getFinalLocation(r.Context(), mediaPath, r.Header.Get("User-Agent"))
	if err != nil {
		slog.Error("[302 失败] 获取直链失败", "媒体名称", mediaName, "路径", mediaPath, "错误", err)
		return false
	}

	slog.Info("[302 成功]", "媒体名称", mediaName)
	http.Redirect(w, r, finalURL, http.StatusFound)
	return true
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
	slog.Debug("[302] 获取重定向地址", "原始URL", targetURL, "最终URL", finalUrl, "状态码", resp.StatusCode)
	return
}
func handleAttachment(w http.ResponseWriter, r *http.Request) {
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-r.Context().Done():
		return
	}
	apiURL := embyURL + r.URL.RequestURI()
	req, _ := http.NewRequestWithContext(r.Context(), "GET", apiURL, nil)
	maps.Copy(req.Header, r.Header)
	req.Header.Del("Accept-Encoding")

	resp, err := sharedClient.Do(req)
	if err != nil {
		slog.Error("[媒体附件请求失败]", "错误", err)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("[媒体附件请求失败]", "路径", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		return
	}
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
