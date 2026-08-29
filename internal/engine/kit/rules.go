package kit

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/ytx-zhang/115tools/internal/conf"
)

// videoThreshold 视频体积阈值：命中扩展名且 ≥10MB 才按视频处理。
const videoThreshold = 10 * 1024 * 1024

// Rules 文件判定规则（视频扩展名白名单 + 上传排除名单），不可变值对象。
type Rules struct {
	videoExts     []string
	uploadExclude []string
}

// NewRules 从全局设置组装规则值对象（扩展名统一小写）。
func NewRules(cfg *conf.Config) Rules {
	exts := make([]string, len(cfg.Settings.VideoExts))
	for i, e := range cfg.Settings.VideoExts {
		exts[i] = strings.ToLower(e)
	}
	return Rules{videoExts: exts, uploadExclude: cfg.Settings.UploadExclude}
}

// CheckVideo 判断文件是否为视频（扩展名命中 + 体积 ≥ 10MB）。
func (r Rules) CheckVideo(ext string, size int64) bool {
	return slices.Contains(r.videoExts, strings.ToLower(ext)) && size >= videoThreshold
}

// IsVideoExt 仅按扩展名判断（不检查大小）。
func (r Rules) IsVideoExt(path string) bool {
	return slices.Contains(r.videoExts, strings.ToLower(filepath.Ext(path)))
}

// IsUploadExcluded 判断文件名是否命中上传排除规则（大小写不敏感，支持通配）。
func (r Rules) IsUploadExcluded(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range r.uploadExclude {
		if match, err := filepath.Match(strings.ToLower(p), lower); err == nil && match {
			return true
		}
	}
	return false
}
