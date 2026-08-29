package kit

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

// ParseStrmFile 解析 .strm 内容，返回 pickcode 与本地解码出的 fid。
func ParseStrmFile(strmPath string) (pickcode, fid string) {
	data, err := os.ReadFile(strmPath)
	if err != nil {
		return "", ""
	}
	trimmed := strings.TrimSpace(strings.TrimPrefix(string(data), "\xEF\xBB\xBF"))
	u, err := url.Parse(trimmed)
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
	pc, _ := ParseStrmFile(strmPath)
	if pc == "" {
		if st, e := os.Stat(strmPath); e == nil {
			return false, st.ModTime().Unix(), nil
		}
		return false, 0, nil
	}
	want := strmContent(strmURL, pc)
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
	if isVideo {
		t0 := time.Now()
		if err := WriteStrmFile(s.paths.StrmURL, pickCode, savePath); err != nil {
			journal.Error(ctx, "创建 strm 失败", "路径", savePath, "错误", err)
			return 0, err
		}
		journal.Info(ctx, "新增 STRM 文件", "路径", savePath, "耗时", time.Since(t0))
		st, serr := os.Stat(savePath)
		if serr != nil {
			return 0, serr
		}
		return st.ModTime().Unix(), nil
	}
	t0 := time.Now()
	if err := DownloadCloudFile(ctx, s.api, pickCode, savePath); err != nil {
		journal.Error(ctx, "下载文件失败", "路径", savePath, "错误", err)
		return 0, err
	}
	journal.Info(ctx, "下载文件成功", "路径", savePath, "耗时", time.Since(t0))
	st, serr := os.Stat(savePath)
	if serr != nil {
		return 0, serr
	}
	return st.Size(), nil
}
