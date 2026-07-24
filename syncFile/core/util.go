package core

import (
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
)

// 本文件是与具体业务流程无关的纯工具函数集合。

// VideoExts 视频文件扩展名白名单。
// 提为包级变量，避免 CheckVideo 每次调用都重新分配切片。
var VideoExts = []string{".mp4", ".mkv", ".avi", ".mov", ".ts", ".flv", ".wmv"}

// CheckVideo 判断一个文件是否应按「视频」处理（视频上传后要替换为 .strm 索引）。
// 两个条件：扩展名在白名单内，且体积不小于 10MB
// （过小的视频通常是样本/广告片段，不值得走 strm 流程）。
func CheckVideo(ext string, size int64) bool {
	if size < 10*1024*1024 {
		return false
	}
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

// FailLog 统一记录一次任务失败，做且只做两件事：
//  1. 失败计数 +1（供任务卡片的「失败 N」与「零失败才移动文件」门控使用）；
//  2. 输出一条 ERROR 级结构化日志（经 logstream 进入前端日志卡片，
//     按「错误」级别过滤即可查看全部失败明细）。
//
// 旧的「失败明细列表」设计已删除——同一错误记两处是典型的重复记账，
// 现在日志管道是唯一权威来源。
func FailLog(stats *TaskStats, path, action string, err error) {
	stats.AddFailed(1)
	slog.Error(action, "文件", path, "错误", err)
}
