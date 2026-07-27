package core

import (
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"115tools/internal/media"
)

// uploadExclude 当前生效的上传排除名单（小写、已去重），由 SetUploadExclude 原子替换。
// 用 atomic.Pointer 持有切片头，避免 readLocalDir 高频并发读取时切片头撕裂。
var uploadExclude atomic.Pointer[[]string]

func init() {
	// 初始值设为内置默认（见 media.DefaultUploadExclude），保证程序在 NewEnv 尚未调用前即可生效。
	uploadExclude.Store(&media.DefaultUploadExclude)
}

// SetUploadExclude 用新的后缀名单原子替换运行期配置。
// 入参会先经 media.NormalizeUploadExclude 清洗（去空格、小写、去空、去重、全空回退默认），
// 调用方无需预清洗。返回清洗后的名单副本，便于调用方核对。
func SetUploadExclude(patterns []string) []string {
	clean := media.NormalizeUploadExclude(patterns)
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
