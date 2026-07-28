package core

import (
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
)

// DefaultUploadExclude 是「上传排除（下载器/系统临时文件）」的内置默认后缀名单。
// 与 config.DefaultUploadExclude 保持一致——配置未显式设置 upload_exclude 时使用它，
// 运行期也可由配置覆盖（见 SetUploadExclude / web 保存配置后实时刷新）。
//
// 匹配方式（见 IsUploadExcluded）：文件名小写后，若「整名命中」（如 .DS_Store、
// Thumbs.db 这类无扩展名的系统垃圾文件）或「扩展名命中」（如 .part、.crdownload）
// 任一成立，即跳过该文件——不上传，同时让云端已存在的同名项被判定为「本地已删」
// 而联动清理。
// 注意：名单统一小写——运行期匹配时文件名会被 ToLower 后比较（见 IsUploadExcluded），
// 故默认也用小写，保证「恢复默认值」回填与已清洗（小写）的生效值一致，前端不再出现「变大写」。
// 这类无扩展名系统文件（.ds_store / thumbs.db）虽磁盘上常为大写，但匹配是大小写无关（ToLower），
// 且 normalizeUploadExclude 同样会小写化，所以用小写定义不影响实际命中。
var DefaultUploadExclude = []string{
	".part",     // 通用分片下载（aria2 / wget / 多数下载器）
	".partial",  // 部分下载（Transmission / Deluge / 部分浏览器）
	".aria2",    // aria2 控制文件（与无后缀的未下完本体成对出现）
	".crdownload", // Chrome 下载中
	".download", // Firefox / Edge 下载中
	".tmp",      // 通用临时文件
	".!qb",      // qBittorrent 下载中（movie.mkv.!qB 的扩展名即 .!qb）
	".ds_store", // macOS 系统垃圾文件（无扩展名，需整名命中）
	"thumbs.db", // Windows 系统缩略图缓存（无扩展名，需整名命中）
}

// uploadExclude 当前生效的上传排除名单（小写、已去重），由 SetUploadExclude 原子替换。
// 用 atomic.Pointer 持有切片头，避免 readLocalDir 高频并发读取时切片头撕裂。
var uploadExclude atomic.Pointer[[]string]

func init() {
	// 初始值设为内置默认，保证程序在 NewEnv 尚未调用前即可生效。
	uploadExclude.Store(&DefaultUploadExclude)
}

// SetUploadExclude 用新的后缀名单原子替换运行期配置。
// 入参会先经 normalizeUploadExclude 清洗（去空格、小写、去空、去重、全空回退默认），
// 调用方无需预清洗。返回清洗后的名单副本，便于调用方核对。
func SetUploadExclude(patterns []string) []string {
	clean := normalizeUploadExclude(patterns)
	uploadExclude.Store(&clean)
	slog.Info("[CORE] 上传排除名单已更新", "规则数", len(clean))
	return clean
}

// IsUploadExcluded 判断一个文件名是否应被排除（不上传、且联动清理云端存量）。
// 命中条件（文件名小写后）：整名在名单内，或其扩展名（含点）在名单内。
// 这是上传来源的唯一入口 readLocalDir 的硬拦截，覆盖 FullScan/定时/监控触发的所有同步。
func IsUploadExcluded(name string) bool {
	lower := strings.ToLower(name)
	list := *uploadExclude.Load()
	return slices.Contains(list, lower) || slices.Contains(list, strings.ToLower(filepath.Ext(name)))
}

// normalizeUploadExclude 清洗上传排除名单：去空格、小写、去空、去重；
// 全空时回退内置默认（保证运行期不会因空白名单把一切临时文件都上传）。
// 注意：不强制补前导点（如 .DS_Store 已是带点的整名）；匹配时文件名会先 ToLower，
// 与已小写化的名单比较，故大小写无关，无需原样保留大小写。
func normalizeUploadExclude(in []string) []string {
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
