// Package media 持有 video_exts 与 upload_exclude 两份名单的「内置默认」
// 与它们的归一化逻辑，供 config 与 syncFile/core 共享，避免两份拷贝漂移。
//
// 历史背景：原先 config 与 syncFile/core 各自定义了一份 DefaultVideoExts /
// DefaultUploadExclude 以及对应的 normalize 函数（config 不能 import core，
// 故刻意重复）。抽成本包后，两方都 import internal/media，互不依赖。
package media

import "strings"

// DefaultVideoExts 视频文件扩展名内置默认白名单（常见视频格式）。
// 运行期可由配置覆盖（见 config 的更新逻辑 / syncFile/core 的 NewEnv）。
var DefaultVideoExts = []string{
	".mp4", ".mkv", ".avi", ".mov", ".ts", ".flv", ".wmv",
	".m4v", ".mpg", ".mpeg", ".webm", ".rmvb", ".3gp", ".vob",
}

// DefaultUploadExclude 上传排除名单内置默认（下载器/系统临时文件后缀）。
var DefaultUploadExclude = []string{
	".part", ".partial", ".aria2", ".crdownload", ".download",
	".tmp", ".!qB", ".DS_Store", "Thumbs.db",
}

// NormalizeVideoExts 清洗用户输入的扩展名白名单：去空格、统一小写、补前导点、
// 去空、去重；全空时回退内置默认（保证运行期不会因空白名单把一切判为非视频）。
func NormalizeVideoExts(in []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultVideoExts...)
	}
	return out
}

// NormalizeUploadExclude 清洗用户输入的上传排除名单：去空格、小写、去空、去重；
// 全空时回退内置默认（保证运行期不会因空白名单把一切临时文件都上传）。
// 注意：不强制补前导点——否则 .DS_Store 会被加成 .ds_store 而整名匹配失败，
// 这类无扩展名系统文件需原样保留。
func NormalizeUploadExclude(in []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultUploadExclude...)
	}
	return out
}
