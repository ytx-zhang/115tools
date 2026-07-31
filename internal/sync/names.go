package sync

import (
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
)

// 本文件是与具体业务流程无关的纯工具函数集合。

// videoExts 当前生效的视频扩展名白名单，用 atomic 承载——web 保存配置时写、
// 扫描/上传协程并发读，原 core.VideoExts 普通切片是真实 race，改 atomic 后读写安全。
// 初始为空名单（不识别任何视频），由 NewEnv 在启动/热重载时从配置注入。
var videoExts atomic.Pointer[[]string]

// SetVideoExts 原子更新视频扩展名名单（web 保存配置后调用）。
func SetVideoExts(exts []string) {
	videoExts.Store(&exts)
}

func currentVideoExts() []string {
	p := videoExts.Load()
	if p == nil {
		return nil
	}
	return *p
}

// CheckVideo 判断一个文件是否应按「视频」处理（视频上传后要替换为 .strm 索引）。
// 条件：扩展名在白名单内，且体积不小于 10MB（过小的视频通常是样本/广告片段）。
func CheckVideo(ext string, size int64) bool {
	if size < 10*1024*1024 {
		return false
	}
	return slices.Contains(currentVideoExts(), strings.ToLower(ext))
}

// IsVideoExt 仅按扩展名判断是否为视频（不关心体积），用于捕获「扩展名是视频、但体积过小
// （如未下完片段）」这类需特殊处理的文件。调用方：instance 的 doUpload。
func IsVideoExt(ext string) bool {
	return slices.Contains(currentVideoExts(), strings.ToLower(ext))
}

// ExtractPickcode 从 .strm 文件内容解析出 pickcode 与 fid。内容形如
// http://host/download?pickcode=xxx&fid=yyy；文件不存在或内容非法时返回空串。
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
