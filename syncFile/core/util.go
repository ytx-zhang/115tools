package core

import (
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"

	"115tools/internal/media"
)

// 本文件是与具体业务流程无关的纯工具函数集合。

// VideoExts 当前生效的视频扩展名白名单，用 atomic.Pointer 持有切片头，
// 避免 sync/upload 并发 goroutine 读取时，与 web 保存配置 / 热重载的写入产生数据竞争。
// 初始值在 init 中 Store 内置默认，保证 Load 在 NewEnv 调用前也永不为 nil。
var VideoExts atomic.Pointer[[]string]

func init() {
	// 初始值设为内置默认（见 media.DefaultVideoExts），保证程序在 NewEnv 尚未调用前即可生效。
	defaults := append([]string(nil), media.DefaultVideoExts...)
	VideoExts.Store(&defaults)
}

// CheckVideo 判断一个文件是否应按「视频」处理（视频上传后要替换为 .strm 索引）。
// 两个条件：扩展名在白名单内，且体积不小于 10MB
// （过小的视频通常是样本/广告片段，不值得走 strm 流程）。
func CheckVideo(ext string, size int64) bool {
	if size < 10*1024*1024 {
		return false
	}
	return slices.Contains(*VideoExts.Load(), strings.ToLower(ext))
}

// ExtractPickcode 从 .strm 文件内容中解析出 pickcode 与 fid。
// .strm 内容形如 http://host/download?pickcode=xxx&fid=yyy。
// 文件不存在或内容不是合法 URL 时返回两个空串，调用方按「无 pickcode」处理。
func ExtractPickcode(fPath string) (pickcode, fid string) {
	content, err := os.ReadFile(fPath)
	if err != nil {
		return "", ""
	}
	u, err := url.Parse(strings.TrimSpace(string(content)))
	if err != nil {
		return "", ""
	}
	pickcode = u.Query().Get("pickcode")
	fid = u.Query().Get("fid")
	return
}
