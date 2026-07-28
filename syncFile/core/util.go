package core

import (
	"net/url"
	"os"
	"slices"
	"strings"
)

// 本文件是与具体业务流程无关的纯工具函数集合。

// VideoExts 当前生效的视频扩展名白名单。core 不内置默认——初始为空名单，
// 由 NewEnv 在每次启动/热重载时从 cfg.VideoExts 注入（config 负责提供默认白名单，
// 见 config.DefaultVideoExts）；web 保存配置后即时刷新。CheckVideo 直接读它判断文件是否视频。
// 为空名单时 CheckVideo/IsVideoExt 恒为 false（不识别任何视频）。
var VideoExts []string

// CheckVideo 判断一个文件是否应按「视频」处理（视频上传后要替换为 .strm 索引）。
// 两个条件：扩展名在白名单内，且体积不小于 10MB
// （过小的视频通常是样本/广告片段，不值得走 strm 流程）。
func CheckVideo(ext string, size int64) bool {
	if size < 10*1024*1024 {
		return false
	}
	return slices.Contains(VideoExts, strings.ToLower(ext))
}

// IsVideoExt 仅按扩展名判断是否为视频（不关心体积），供「体积未达阈值的视频文件」识别使用。
// 与 CheckVideo 的区别：CheckVideo 额外要求体积不小于 10MB 才算视频；
// 而本函数只要扩展名在白名单内即返回 true，用于捕获「扩展名是视频、但体积过小（如未下完的片段）」
// 这类需特殊处理的文件。调用方：syncFile/local 的 doUpload。
func IsVideoExt(ext string) bool {
	return slices.Contains(VideoExts, strings.ToLower(ext))
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
