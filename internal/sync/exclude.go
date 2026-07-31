package sync

import (
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/ytx-zhang/115tools/internal/config"
)

// uploadExclude 是运行期生效的上传排除名单（统一小写、原子可热更新）。
// 初始为空名单——空名单即「不排除任何文件」；实际值由 NewEnv 经 SetUploadExclude
// 从配置注入。匹配时大小写无关（ToLower 后比较）。
var uploadExclude atomic.Pointer[[]string]

func init() { uploadExclude.Store(&[]string{}) }

// SetUploadExclude 原子替换运行期排除名单（入参经 config 清洗：小写/去重/空则回退默认）。
func SetUploadExclude(patterns []string) []string {
	clean := config.NormalizeUploadExclude(patterns)
	uploadExclude.Store(&clean)
	slog.Info("[CORE] 上传排除名单已更新", "规则数", len(clean))
	return clean
}

// IsUploadExcluded 判断文件名是否应被排除（整名或扩展名命中名单）。
func IsUploadExcluded(name string) bool {
	lower := strings.ToLower(name)
	list := *uploadExclude.Load()
	return slices.Contains(list, lower) || slices.Contains(list, strings.ToLower(filepath.Ext(name)))
}
