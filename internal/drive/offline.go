package drive

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// ──── 离线下载原子方法 ────

// addTasks 批量添加离线下载链接（http/https/magnet/ed2k），保存到 saveDirID。
func (c *Client) addTasks(ctx context.Context, urls []string, saveDirID string) ([]OfflineAddResult, error) {
	return Post[[]OfflineAddResult](ctx, c, "/open/offline/add_task_urls",
		Form{
			"urls":       strings.Join(urls, "\n"),
			"wp_path_id": saveDirID,
		})
}

// ListTasks 获取云下载任务列表的一页（page 从 1 开始）。
func (c *Client) ListTasks(ctx context.Context, page int) (*OfflineTaskPage, error) {
	res, err := Get[OfflineTaskPage](ctx, c, "/open/offline/get_task_list",
		Form{"page": strconv.Itoa(max(page, 1))})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// DeleteTask 删除单个云下载任务；deleteFiles 为 true 时同时删除已下载的源文件。
func (c *Client) DeleteTask(ctx context.Context, infoHash string, deleteFiles bool) error {
	t0 := time.Now()
	delSource := "0"
	if deleteFiles {
		delSource = "1"
	}
	_, err := Post[struct{}](ctx, c, "/open/offline/del_task",
		Form{"info_hash": infoHash, "del_source_file": delSource})
	return FinishLog(t0, "删除离线任务", err, "info_hash", infoHash, "删除源文件", deleteFiles)
}

// ClearTasks 批量清除任务。
// flag：0 已完成，1 全部，2 失败，3 进行中，4 已完成且删源文件，5 全部且删源文件。
func (c *Client) ClearTasks(ctx context.Context, flag int) error {
	t0 := time.Now()
	_, err := Post[struct{}](ctx, c, "/open/offline/clear_task",
		Form{"flag": strconv.Itoa(flag)})
	return FinishLog(t0, "批量清除离线任务", err, "flag", flag)
}

// GetQuota 获取云下载配额信息。
// data 段即 {count, surplus, used}（对齐 OpenList 的 OfflineQuotaInfo），用 Call 直接解析。
func (c *Client) GetQuota(ctx context.Context) (*OfflineQuota, error) {
	res, err := Get[OfflineQuota](ctx, c, "/open/offline/get_quota_info", nil)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// ──── 离线下载辅助函数 ────

// ValidateAndAddOffline 参数校验 + 添加离线任务。
func ValidateAndAddOffline(ctx context.Context, c *Client, urls []string, saveDirID string) ([]OfflineAddResult, error) {
	t0 := time.Now()
	if len(urls) == 0 {
		return nil, fmt.Errorf("没有可添加的链接")
	}
	logs.Info(logs.ModuleCloud, "添加离线下载任务", "数量", len(urls), "目标目录", saveDirID)
	results, err := c.addTasks(ctx, urls, saveDirID)
	if err != nil {
		logs.Error(logs.ModuleCloud, "添加离线下载任务失败", "数量", len(urls), "错误", err, "耗时", time.Since(t0))
		return nil, err
	}
	logs.Info(logs.ModuleCloud, "添加离线下载任务完成", "数量", len(urls), "目标目录", saveDirID, "耗时", time.Since(t0))
	return results, nil
}

// AddTorrentFromData 添加 BT 种子离线任务：解析 bencode→构造 magnet→提交。
func AddTorrentFromData(ctx context.Context, c *Client, torrentData []byte, torrentName, savePath string) (*OfflineAddResult, error) {
	t0 := time.Now()
	logs.Info(logs.ModuleCloud, "添加种子离线任务", "文件名", torrentName, "save_path", savePath)
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	infoHash, displayName, err := ParseTorrentInfo(torrentData)
	if err != nil {
		return nil, fmt.Errorf("解析种子失败: %w", err)
	}
	if displayName == "" {
		displayName = torrentName
	}
	saveDir, err := c.GetDirInfo(ctx, savePath)
	if err != nil {
		return nil, fmt.Errorf("解析保存目录失败: %w", err)
	}
	magnet := "magnet:?xt=urn:btih:" + infoHash + "&dn=" + url.QueryEscape(displayName)
	results, err := ValidateAndAddOffline(ctx, c, []string{magnet}, saveDir.Fid)
	if err != nil {
		return nil, fmt.Errorf("添加磁力链接失败: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("添加磁力链接失败: 无返回结果")
	}
	r := results[0]
	if r.InfoHash == "" {
		r.InfoHash = infoHash
	}
	logs.Info(logs.ModuleCloud, "添加种子离线任务完成", "文件名", torrentName,
		"info_hash", r.InfoHash, "save_path", savePath, "耗时", time.Since(t0))
	return &r, nil
}
