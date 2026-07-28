package core

import "time"

// Paths 集中保存路径配置与云端目录 FID。以指针存放在 Env 中，
// bootstrap 回填 FID 后对所有模块立即可见。
// SyncFid 索引落库但每次启动重新核对；TempFid 仅存内存不落库。
type Paths struct {
	SyncPath string
	SyncFid  string
	TempPath string
	TempFid  string
	StrmPath string
	StrmUrl  string
	Debounce time.Duration
}

// DebounceDuration 把秒数转为去抖窗口（0→默认5s，>10s钳到10s）。
func DebounceDuration(secs int) time.Duration {
	const (
		def  = 5 * time.Second
		maxd = 10 * time.Second
	)
	if secs <= 0 {
		return def
	}
	d := time.Duration(secs) * time.Second
	if d > maxd {
		return maxd
	}
	return d
}
