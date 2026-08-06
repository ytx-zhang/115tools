package sync

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
	"sync/atomic"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// ──── 常量 ────

const DebounceDuration = 5 * time.Second // 写入停止后等待批量触发时间（上限 10s）

const uploadWorkerCount = 4 // 上传并发数

// ──── 视频类型过滤 ────

var videoExts atomic.Pointer[[]string]

// SetVideoExts 用外部配置替换视频扩展名名单（线程安全）。
func SetVideoExts(exts []string) {
	extsLower := make([]string, len(exts))
	for i, e := range exts {
		extsLower[i] = strings.ToLower(e)
	}
	videoExts.Store(&extsLower)
}

func currentVideoExts() []string {
	if p := videoExts.Load(); p != nil {
		return *p
	}
	return nil
}

// CheckVideo 判断文件是否为视频（扩展名命中 + 体积 >= 10MB）。
func CheckVideo(ext string, size int64) bool {
	exts := currentVideoExts()
	if exts == nil {
		return false
	}
	return slices.Contains(exts, strings.ToLower(ext)) && size >= 10*1024*1024
}

// IsVideoExt 仅按扩展名判断（不检查文件大小）。
func IsVideoExt(path string) bool {
	exts := currentVideoExts()
	if exts == nil {
		return false
	}
	return slices.Contains(exts, strings.ToLower(filepath.Ext(path)))
}

// ExtractPickcode 从 .strm 文件首行提取 pickcode 与 fid。
// 格式：{url}/download?pickcode=xxx&fid=yyy
func ExtractPickcode(strmPath string) (pickcode string, fid string) {
	data, err := os.ReadFile(strmPath)
	if err != nil {
		return "", ""
	}
	raw := strings.TrimSpace(strings.TrimPrefix(string(data), "\xEF\xBB\xBF"))
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	return u.Query().Get("pickcode"), u.Query().Get("fid")
}

// ──── 上传排除过滤 ────

var uploadExclude atomic.Pointer[[]string]

// SetUploadExclude 用外部配置替换上传排除规则。
func SetUploadExclude(patterns []string) {
	uploadExclude.Store(&patterns)
}

// IsUploadExcluded 返回文件是否命中上传排除规则（大小写不敏感）。
func IsUploadExcluded(name string) bool {
	pp := uploadExclude.Load()
	if pp == nil {
		return false
	}
	nameLower := strings.ToLower(name)
	for _, p := range *pp {
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

// ──── 云端文件落地 ────

// ProcessCloudFile 把云端文件条目映射为「本地保存路径 + 应记入数据库的大小值」。
// 视频文件 → 路径改为 .strm 后缀，大小值记录当前时间戳（作为版本号供日后比对）；
// 普通文件 → 路径不变，大小值就是真实字节数。
func ProcessCloudFile(path string, e Entry) (savePath string, saveSize int64) {
	if e.IsVideo {
		return strings.TrimSuffix(path, filepath.Ext(path)) + ".strm", time.Now().Unix()
	}
	return path, e.Size
}

// FetchAndSave 按文件类型把云端文件落地：视频写 .strm 索引文件，普通文件真实下载。
func (e *Env) FetchAndSave(ctx context.Context, pickCode, fid, savePath string, isVideo bool) error {
	if isVideo {
		t0 := time.Now()
		if err := e.SaveStrmFile(pickCode, fid, savePath); err != nil {
			logs.Error(logs.ModuleSync, "创建strm文件失败", "文件", savePath, "错误", err)
			return err
		}
		logs.Info(logs.ModuleSync, "新增STRM文件", "文件", savePath, "耗时", time.Since(t0))
		return nil
	}
	t0 := time.Now()
	if err := e.DownloadFile(ctx, pickCode, savePath); err != nil {
		logs.Error(logs.ModuleSync, "下载文件失败", "文件", savePath, "错误", err)
		return err
	}
	logs.Info(logs.ModuleSync, "下载文件成功", "文件", savePath, "耗时", time.Since(t0))
	return nil
}

// DownloadFile 用 pickcode 换取 115 下载直链，把文件完整下载到 localPath。
func (e *Env) DownloadFile(ctx context.Context, pickcode, localPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := e.API.GetDownloadUrl(ctx, pickcode, "115tools")
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
		os.Remove(localPath)
		return copyErr
	}
	return nil
}

// SaveStrmFile 生成 .strm 索引文件（一行指向 /download 的 URL）。
func (e *Env) SaveStrmFile(pickcode, fid, localPath string) error {
	content := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", e.Paths.StrmUrl, pickcode, fid)
	return os.WriteFile(localPath, []byte(content), 0644)
}
