package shared

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
)

// ──── .strm 路径纯函数 ────

// IsStrmPath 判断路径是否为 .strm 索引文件（大小写不敏感）。
func IsStrmPath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".strm")
}

// VideoToStrmPath 把视频路径改为同名 .strm 路径（去掉原扩展名）。
func VideoToStrmPath(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".strm"
}

// strmContent 构造 .strm 直链内容（统一格式：strmURL 去尾斜杠 + /download?pickcode=）。
func strmContent(strmURL, pickcode string) string {
	return fmt.Sprintf("%s/download?pickcode=%s", strings.TrimRight(strmURL, "/"), pickcode)
}

// trimBOM 去掉 UTF-8 BOM 与首尾空白（.strm 可能被外部工具带 BOM 写出）。
func trimBOM(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(s, "\xEF\xBB\xBF"))
}

// statMtime 取文件 mtime（Unix 秒），读取失败返回 0。
func statMtime(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime().Unix()
	}
	return 0
}

// ParseStrmFile 解析 .strm 内容，返回 pickcode 与本地解码出的 fid。
func ParseStrmFile(strmPath string) (pickcode, fid string) {
	data, err := os.ReadFile(strmPath)
	if err != nil {
		return "", ""
	}
	u, err := url.Parse(trimBOM(string(data)))
	if err != nil {
		return "", ""
	}
	pc := u.Query().Get("pickcode")
	if decoded, derr := pan.PickcodeToID(pc); derr == nil {
		fid = decoded
	}
	return pc, fid
}

// NormalizeStrmFile 把 .strm 重写成本程序直链格式，仅修正 host、pickcode 不变。
// 返回（是否覆写，写盘后 mtime，错误）；已正确时 rewrote=false。
func NormalizeStrmFile(strmURL, strmPath string) (bool, int64, error) {
	// 只读一次文件：同时用于解析 pickcode 与比对内容（解析失败即按无 pickcode 处理）
	raw, rerr := os.ReadFile(strmPath)
	cur := trimBOM(string(raw))
	pc := ""
	if rerr == nil {
		if u, uerr := url.Parse(cur); uerr == nil {
			pc = u.Query().Get("pickcode")
		}
	}
	if pc == "" {
		return false, statMtime(strmPath), nil
	}
	want := strmContent(strmURL, pc)
	if rerr == nil && cur == want {
		return false, statMtime(strmPath), nil
	}
	if werr := WriteStrmFile(strmURL, pc, strmPath); werr != nil {
		return false, 0, werr
	}
	return true, statMtime(strmPath), nil
}

// NormalizeOwnedStrm 把旧 strm 规范化为本程序直链，仅当 pickcode 解码出的 fid 与 expectedFid 一致时才重写。
// 返回（matched, rewrote, mt）。
func NormalizeOwnedStrm(strmURL, strmPath, expectedFid string, fileModTime int64) (matched, rewrote bool, mt int64) {
	pc, localFid := ParseStrmFile(strmPath)
	if pc == "" || localFid != expectedFid {
		return false, false, fileModTime
	}
	if r, m, nerr := NormalizeStrmFile(strmURL, strmPath); nerr == nil {
		return true, r, m
	}
	return true, false, fileModTime
}

// WriteStrmFile 生成 .strm 索引文件（一行指向 /download 的 URL）。
// 覆盖已存在文件时保留原 mtime（避免被扫描误判为变更）。
func WriteStrmFile(strmURL, pickcode, localPath string) error {
	content := strmContent(strmURL, pickcode)
	var oldMod time.Time
	if st, err := os.Stat(localPath); err == nil {
		oldMod = st.ModTime()
	}
	if err := os.WriteFile(localPath, []byte(content), 0o644); err != nil {
		return err
	}
	if !oldMod.IsZero() {
		return os.Chtimes(localPath, oldMod, oldMod)
	}
	return nil
}

// ──── 云端 → 本地落地 ────

// DownloadCloudFile 用 pickcode 换取直链，把文件完整下载到 localPath。
func DownloadCloudFile(ctx context.Context, api *pan.Client, pickcode, localPath string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	info, err := api.GetDownloadURL(ctx, pickcode, "115tools")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", info.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "115tools")

	resp, err := pan.HTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			journal.Debug(ctx, "关闭下载响应体失败", "错误", cerr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil {
			journal.Debug(ctx, "关闭下载文件失败", "路径", localPath, "错误", cerr)
		}
	}()
	if _, err := io.Copy(out, resp.Body); err != nil {
		if rmErr := os.Remove(localPath); rmErr != nil {
			journal.Debug(ctx, "清理下载失败残留失败", "路径", localPath, "错误", rmErr)
		}
		return err
	}
	return nil
}

// StrmIO 云端→本地落地：视频写 .strm，普通文件下载。
type StrmIO struct {
	api   *pan.Client
	paths *TaskPaths
}

// NewStrmIO 构造落地模块。
func NewStrmIO(deps *Deps) *StrmIO {
	return &StrmIO{api: deps.Pan, paths: deps.Paths}
}

// FetchAndSave 按文件类型落地，返回落盘后的「版本号」：视频=本地 strm 实际 mtime，普通=真实字节数。
func (s *StrmIO) FetchAndSave(ctx context.Context, pickCode, savePath string, isVideo bool) (int64, error) {
	t0 := time.Now()
	var err error
	if isVideo {
		err = WriteStrmFile(s.paths.StrmURL, pickCode, savePath)
	} else {
		err = DownloadCloudFile(ctx, s.api, pickCode, savePath)
	}
	if err != nil {
		if isVideo {
			journal.Error(ctx, "创建 strm 失败", "路径", savePath, "错误", err)
		} else {
			journal.Error(ctx, "下载文件失败", "路径", savePath, "错误", err)
		}
		return 0, err
	}
	if isVideo {
		journal.Info(ctx, "新增 STRM 文件", "路径", savePath, "耗时", time.Since(t0))
	} else {
		journal.Info(ctx, "下载文件成功", "路径", savePath, "耗时", time.Since(t0))
	}
	st, serr := os.Stat(savePath)
	if serr != nil {
		return 0, serr
	}
	if isVideo {
		return st.ModTime().Unix(), nil
	}
	return st.Size(), nil
}
