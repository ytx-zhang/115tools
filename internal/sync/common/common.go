// Package common 提供 sync 各任务子包共享的值对象与纯函数：
// 判定规则（Rules）、路径参数（Paths）、遍历协议（Entry/Visitor）、任务与进度封装（Task），
// 以及 .strm 路径 / 直链 URL 纯函数与云端落地辅助（下载）。
//
// 依赖方向严格单向：被全部任务子包 import，本包不 import 任何 sync 子包（因此不会成环）。
// 本包只依赖三个更底层的包（并非“无依赖工具包”）：
//   - drive：DownloadCloudFile 需要云端客户端，ParseStrmFile 需要 PickcodeToID 本地解码；
//   - store：SyncDeps 依赖载体（DB 句柄类型）；
//   - logs：调试与状态日志。
package common

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// Rules 是文件判定规则（视频扩展名白名单 + 上传排除名单），不可变值对象。
type Rules struct {
	videoExts     []string // 小写扩展名白名单（含前导点）
	uploadExclude []string // 上传排除名单（后缀/整名，小写）
}

// NewRules 从配置组装规则值对象（扩展名统一小写）。
func NewRules(cfg *config.Config) Rules {
	exts := make([]string, len(cfg.VideoExts))
	for i, e := range cfg.VideoExts {
		exts[i] = strings.ToLower(e)
	}
	return Rules{videoExts: exts, uploadExclude: cfg.UploadExclude}
}

// CheckVideo 判断文件是否为视频（扩展名命中 + 体积 >= 10MB）。
func (r Rules) CheckVideo(ext string, size int64) bool {
	return slices.Contains(r.videoExts, strings.ToLower(ext)) && size >= 10*1024*1024
}

// IsVideoExt 仅按扩展名判断（不检查文件大小）。
func (r Rules) IsVideoExt(path string) bool {
	return slices.Contains(r.videoExts, strings.ToLower(filepath.Ext(path)))
}

// IsUploadExcluded 返回文件是否命中上传排除规则（大小写不敏感）。
func (r Rules) IsUploadExcluded(name string) bool {
	nameLower := strings.ToLower(name)
	for _, p := range r.uploadExclude {
		match, err := filepath.Match(strings.ToLower(p), nameLower)
		if err != nil {
			continue
		}
		if match {
			return true
		}
	}
	return false
}

// ──── .strm 路径纯函数 ────

// IsStrmPath 判断路径是否为 .strm 索引文件（大小写不敏感）。
func IsStrmPath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".strm")
}

// VideoToStrmPath 把视频文件路径改为同名 .strm 索引文件路径（去掉原扩展名，加 .strm）。
func VideoToStrmPath(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".strm"
}

// VideoStrmMeta 返回「视频路径 → .strm 路径 + Unix 秒版本号」的统一映射。
func VideoStrmMeta(path string) (strmPath string, version int64) {
	return VideoToStrmPath(path), time.Now().Unix()
}

// ProcessCloudFile 把云端文件条目映射为「本地保存路径 + 应记入数据库的大小值」。
// 视频文件 → 路径改为 .strm 后缀，大小值记录当前时间戳（作为版本号供日后比对）；
// 普通文件 → 路径不变，大小值就是真实字节数。
func ProcessCloudFile(path string, e Entry) (savePath string, saveSize int64) {
	if e.IsVideo {
		return VideoStrmMeta(path)
	}
	return path, e.Size
}

// ParseStrmFile 读取 .strm 文件，解析出 pickcode 并本地解码出 fid。
// 兼容 UTF-8 BOM 与首尾空白；格式：{url}/download?pickcode=xxx
// （新格式无 fid 参数，fid 恒由 pickcode 解码获得；旧格式若带 fid 参数一律忽略）。
// 读取失败或内容不可解析时返回空串（不报错，由调用方按「无效 strm」处理）。
func ParseStrmFile(strmPath string) (pickcode string, fid string) {
	data, err := os.ReadFile(strmPath)
	if err != nil {
		logs.Debug(logs.ModuleSync, "读取strm文件失败", "路径", strmPath, "错误", err)
		return "", ""
	}
	trimmed := strings.TrimSpace(strings.TrimPrefix(string(data), "\xEF\xBB\xBF"))
	u, err := url.Parse(trimmed)
	if err != nil {
		logs.Debug(logs.ModuleSync, "strm内容解析失败", "路径", strmPath, "错误", err)
		return "", ""
	}
	pc := u.Query().Get("pickcode")
	if decoded, derr := drive.PickcodeToID(pc); derr == nil {
		fid = decoded
	}
	return pc, fid
}

// ──── 云端 → 本地落地辅助 ────

// DownloadCloudFile 用 pickcode 换取 115 下载直链，把文件完整下载到 localPath。
// api 由调用方注入（strmIO 持有），ua 固定 "115tools"。
func DownloadCloudFile(ctx context.Context, api *drive.Client, pickcode, localPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := api.GetDownloadUrl(ctx, pickcode, "115tools", localPath)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", info.Url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "115tools")

	resp, err := drive.HTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, copyErr := io.Copy(out, resp.Body); copyErr != nil {
		if rmErr := os.Remove(localPath); rmErr != nil {
			logs.Debug(logs.ModuleSync, "清理下载失败的残留文件失败", "路径", localPath, "错误", rmErr)
		}
		return copyErr
	}
	return nil
}

// WriteStrmFile 生成 .strm 索引文件（一行指向 /download 的 URL）。
// 内容格式：{strmURL}/download?pickcode={pickcode}；strmURL 末尾多余的 "/" 会被去掉，避免拼出 "http://host//download?..."。
// ⚠️ 直链只允许携带 pickcode：不含 fid（fid 由 drive.PickcodeToID 本地解码）、
// 不含带过期时间的 115 CDN 地址（CDN 直链在播放时才实时取，见 web/redirector.go）。
func WriteStrmFile(strmURL, pickcode, localPath string) error {
	content := fmt.Sprintf("%s/download?pickcode=%s", strings.TrimRight(strmURL, "/"), pickcode)
	return os.WriteFile(localPath, []byte(content), 0644)
}
