package drive

import (
	"net/http"
	"time"
)

// sharedHTTPClient 供全项目各模块复用，避免每次请求新建 TCP 连接，
// 并设置 30s 超时防止挂死。下载大文件时若需更长超时，调用方应使用 http.Transport 自建。
var sharedHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
}

// HTTPClient 返回全局共享的 HTTP 客户端（连接池 + 超时）。
func HTTPClient() *http.Client {
	return sharedHTTPClient
}
