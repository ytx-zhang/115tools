// Package common 提供 sync 各任务子包共享的值对象与纯函数：
// 判定规则（Rules）、路径参数（Paths）、遍历协议（Entry/Visitor）、任务与进度封装（Task），
// 以及 .strm 路径 / 直链 URL 纯函数、云端落地辅助（下载）、落地编排（StrmIO）与云端目录树遍历（Walker）。
//
// 依赖方向严格单向：被全部任务子包 import，本包不 import 任何 sync 子包（因此不会成环）。
// 本包只依赖三个更底层的包（并非“无依赖工具包”）：
//   - drive：DownloadCloudFile 需要云端客户端，ParseStrmFile 需要 PickcodeToID 本地解码，Walker 需要列表接口；
//   - store：Core 依赖载体（DB 句柄类型）；
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

// ParseStrmFile 解析 .strm 内容，返回 pickcode 及本地解码出的 fid。
// 兼容 UTF-8 BOM 与首尾空白；格式 {url}/download?pickcode=xxx（fid 恒由
// pickcode 解码，旧格式自带的 fid 参数忽略）。读取或解析失败时返回空串，
// 由调用方按「无效 strm」处理。
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

// normalizeStrmFile 把 .strm 重写成本程序直链格式（{strmURL}/download?pickcode=），
// 仅修正 host、pickcode 不变。返回（是否实际覆写，写盘后文件 mtime，错误）；
// pickcode 无法解析或已是正确格式时 rewrote=false 并返回原文件 mtime。
// 仅由 NormalizeOwnedStrm 内部调用。
func normalizeStrmFile(strmURL, strmPath string) (bool, int64, error) {
	pc, _ := ParseStrmFile(strmPath)
	if pc == "" {
		if st, e := os.Stat(strmPath); e == nil {
			return false, st.ModTime().Unix(), nil
		}
		return false, 0, nil
	}
	want := fmt.Sprintf("%s/download?pickcode=%s", strings.TrimRight(strmURL, "/"), pc)
	if raw, rerr := os.ReadFile(strmPath); rerr == nil {
		cur := strings.TrimSpace(strings.TrimPrefix(string(raw), "\xEF\xBB\xBF"))
		if cur == want {
			if st, e := os.Stat(strmPath); e == nil {
				return false, st.ModTime().Unix(), nil
			}
			return false, 0, nil
		}
	}
	if werr := WriteStrmFile(strmURL, pc, strmPath); werr != nil {
		return false, 0, werr
	}
	if st, e := os.Stat(strmPath); e == nil {
		return true, st.ModTime().Unix(), nil
	}
	return true, 0, nil
}

// NormalizeOwnedStrm 把旧 strm 规范化为本程序直链，但仅当 strm 内 pickcode 解码出的
// fid 与 expectedFid 一致（确属同一云端文件）时才重写，避免张冠李戴。
//
// 返回（matched, rewrote, mt）：
//   - matched=false：pc 为空或 fid 不符，调用方应走其它逻辑（如清旧视频 + 重传）；
//   - matched=true ：mt 为写库应使用的 mtime——取写盘后实际 mtime，规范化失败（极少，
//     pickcode 可解析却写盘失败）退化为 fileModTime。rewrote 表示是否实际改写了文件内容，
//     供调用方决定是否记日志。
func NormalizeOwnedStrm(strmURL, strmPath, expectedFid string, fileModTime int64) (matched, rewrote bool, mt int64) {
	pc, localFid := ParseStrmFile(strmPath)
	if pc == "" || localFid != expectedFid {
		return false, false, fileModTime
	}
	if r, m, nerr := normalizeStrmFile(strmURL, strmPath); nerr == nil {
		return true, r, m
	}
	return true, false, fileModTime
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
//
// 覆盖已存在的文件时保留原 mtime（os.Chtimes 恢复）：
// 本程序自身重写 strm（规范化、改名、覆盖同名视频）不应被扫描当成「用户修改了文件」，
// 否则按 mtime 判变更会误触发删旧视频+重传。权限/属主不受影响（O_TRUNC 写已存在文件不改变权限）。
func WriteStrmFile(strmURL, pickcode, localPath string) error {
	content := fmt.Sprintf("%s/download?pickcode=%s", strings.TrimRight(strmURL, "/"), pickcode)
	var oldMod time.Time
	if st, err := os.Stat(localPath); err == nil {
		oldMod = st.ModTime()
	}
	if err := os.WriteFile(localPath, []byte(content), 0644); err != nil {
		return err
	}
	if !oldMod.IsZero() {
		return os.Chtimes(localPath, oldMod, oldMod)
	}
	return nil
}
