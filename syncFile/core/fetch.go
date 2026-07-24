package core

import (
	"115tools/drive"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本文件是「把云端内容落到本地磁盘」的存取工具集，供 cloud（云端同步）、
// strm（STRM 生成）两个模块共用；local 模块的 SaveStrmFile 也在这里。

// ProcessCloudFile 把云端文件条目映射为「本地保存路径 + 应记入数据库的大小值」。
//
// 规则：
//   - 视频文件 → 路径改为 .strm 后缀，大小值记录当前时间戳
//     （.strm 是刚生成的索引文件，内容大小无意义，用时间戳充当「版本号」供日后比对）；
//   - 普通文件 → 路径不变，大小值就是真实字节数。
func ProcessCloudFile(path string, e Entry) (savePath string, saveSize int64) {
	if e.IsVideo {
		return strings.TrimSuffix(path, filepath.Ext(path)) + ".strm", time.Now().Unix()
	}
	return path, e.Size
}

// FetchAndSave 按文件类型把云端文件落地：视频写 .strm 索引文件，普通文件真实下载。
// 成功返回 nil；失败已通过 FailLog 记录（计失败数 + ERROR 日志），调用方直接跳过即可。
func (e *Env) FetchAndSave(ctx context.Context, pickCode, fid, savePath string, isVideo bool, stats *TaskStats) error {
	if isVideo {
		if err := e.SaveStrmFile(pickCode, fid, savePath); err != nil {
			FailLog(stats, savePath, "创建strm文件失败", err)
			return err
		}
		slog.Info("新增STRM文件", "文件", savePath)
		return nil
	}
	if err := e.DownloadFile(ctx, pickCode, savePath); err != nil {
		FailLog(stats, savePath, "下载文件失败", err)
		return err
	}
	slog.Info("下载文件成功", "文件", savePath)
	return nil
}

// DownloadFile 用 pickcode 换取 115 下载直链，把文件完整下载到 localPath。
// 全程流式写入（io.Copy），不会因大文件占满内存；失败时删除半成品文件。
func (e *Env) DownloadFile(ctx context.Context, pickcode, localPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// 第一步：用 pickcode 换真实下载地址（User-Agent 是 115 的强制校验项）。
	info, err := e.API.GetDownloadUrl(ctx, pickcode, "115tools")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", info.Url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "115tools")

	// 第二步：发起下载（复用全局共享 HTTP 客户端，享受连接池）。
	resp, err := drive.SharedHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}

	// 第三步：流式落盘。
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, copyErr := io.Copy(out, resp.Body); copyErr != nil {
		os.Remove(localPath) // 删掉写了一半的文件，避免残留坏文件被当成已完成
		return copyErr
	}
	return nil
}

// SaveStrmFile 生成 .strm 索引文件。
// 内容是一行指向本程序 /download 接口的 URL，Emby 播放时请求它，
// 由 /download 实时换取 115 直链并 302 跳转，实现「本地只有几行文本、播放走云端」。
func (e *Env) SaveStrmFile(pickcode, fid, localPath string) error {
	content := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", e.Paths.StrmUrl, pickcode, fid)
	return os.WriteFile(localPath, []byte(content), 0644)
}
