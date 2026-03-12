package syncFile

import (
	"net/url"
	"strings"
)

func extractPickcode(content string) (pickcode, fid string) {
	u, err := url.Parse(strings.TrimSpace(content))
	if err != nil {
		return "", ""
	}
	pickcode = u.Query().Get("pickcode")
	fid = u.Query().Get("fid")
	return
}
func checkVideo(ext string, size int64) bool {
	if size < 10*1024*1024 {
		return false
	}
	switch strings.ToLower(ext) {
	case ".mp4", ".mkv", ".avi", ".mov", ".ts", ".flv", ".wmv":
		return true
	}
	return false
}
