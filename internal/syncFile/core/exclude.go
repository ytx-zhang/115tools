package core

import (
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
)

// uploadExclude 是运行期生效的上传排除名单（统一小写、原子可热更新）。
// 初始为空名单——空名单即「不排除任何文件」，与 IsUploadExcluded 的空列表语义一致；
// 实际值由 NewEnv 经 SetUploadExclude(cfg.UploadExclude) 从配置注入（config 为空则仍为空）。
// 匹配时大小写无关（ToLower 后比较）。
var uploadExclude atomic.Pointer[[]string]

func init() { uploadExclude.Store(&[]string{}) }

// SetUploadExclude 原子替换运行期排除名单（入参自动清洗：小写/去重/空则回退默认）。
func SetUploadExclude(patterns []string) []string {
	clean := normalizeUploadExclude(patterns)
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

// normalizeUploadExclude 清洗名单：去空格、小写、去重；空输入返回空名单（即不排除任何文件）。
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
	return out
}
