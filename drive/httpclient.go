package drive

import (
	"net"
	"net/http"
	"time"
)

// sharedHTTPClient 供 drive 包及外部模块（strmServer、syncFile）复用，
// 避免每次请求新建 TCP 连接，降低 TIME_WAIT 端口耗尽风险。
var sharedHTTPClient = &http.Client{
	Timeout: 120 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// SharedHTTPClient 返回全局共享的 HTTP 客户端（连接池 + 超时）。
func SharedHTTPClient() *http.Client {
	return sharedHTTPClient
}
