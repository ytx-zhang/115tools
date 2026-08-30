package shared

import (
	"path/filepath"
	"strings"

	"github.com/ytx-zhang/115tools/internal/conf"
)

// videoThreshold 视频体积阈值：命中扩展名且 ≥10MB 才按视频处理。
const videoThreshold = 10 * 1024 * 1024

// Rules 文件判定规则（视频扩展名白名单 + 上传排除名单），不可变值对象。
// 名单在构造时统一小写，并用集合存储，使逐文件判定为 O(1)。
type Rules struct {
	videoExts     map[string]struct{}
	uploadExclude []string // 已小写化的通配模式
}

// NewRules 从全局设置组装规则值对象（扩展名统一小写）。
func NewRules(cfg *conf.Config) Rules {
	exts := make(map[string]struct{}, len(cfg.Settings.VideoExts))
	for _, e := range cfg.Settings.VideoExts {
		exts[strings.ToLower(e)] = struct{}{}
	}
	exclude := make([]string, len(cfg.Settings.UploadExclude))
	for i, p := range cfg.Settings.UploadExclude {
		exclude[i] = strings.ToLower(p)
	}
	return Rules{videoExts: exts, uploadExclude: exclude}
}

// hasVideoExt 判断扩展名（需已小写）是否命中视频白名单。
func (r Rules) hasVideoExt(ext string) bool {
	_, ok := r.videoExts[ext]
	return ok
}

// CheckVideo 判断文件是否为视频（扩展名命中 + 体积 ≥ 10MB）。
func (r Rules) CheckVideo(ext string, size int64) bool {
	return r.hasVideoExt(strings.ToLower(ext)) && size >= videoThreshold
}

// IsVideoExt 仅按扩展名判断（不检查大小）。
func (r Rules) IsVideoExt(path string) bool {
	return r.hasVideoExt(strings.ToLower(filepath.Ext(path)))
}

// IsUploadExcluded 判断文件名是否命中上传排除规则（大小写不敏感，支持通配）。
func (r Rules) IsUploadExcluded(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range r.uploadExclude {
		if match, err := filepath.Match(p, lower); err == nil && match {
			return true
		}
	}
	return false
}
