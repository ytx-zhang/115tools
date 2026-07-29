// 包 httputil 提供全项目通用的 HTTP 基础设施（目前只有全局共享连接池）。
// 它不依赖任何业务包（drive/config/db 等），任何模块想发裸 HTTP 请求都直接用这里，
// 不必为了拿个客户端而被迫 import 115 业务包。
package httputil

import (
	"net"
	"net/http"
	"time"
)

// sharedHTTPClient 供全项目各模块复用，避免每次请求新建 TCP 连接，
// 降低 TIME_WAIT 端口耗尽风险。drive 的 token 刷新、core 的下载等都用它。
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
