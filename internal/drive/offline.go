package drive

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// OfflineAddResult 单条链接的添加结果。
type OfflineAddResult struct {
	State    bool   `json:"state"`
	Message  string `json:"message"`
	InfoHash string `json:"info_hash"`
	URL      string `json:"url"`
}

// OfflineTask 一条云下载任务。
type OfflineTask struct {
	InfoHash    string  `json:"info_hash"`
	Name        string  `json:"name"`
	Size        int64   `json:"size"`
	PercentDone float64 `json:"percentDone"`
	// Status：-1 失败，0 分配中，1 下载中，2 成功
	Status int64 `json:"status"`
}

// OfflineTaskPage 任务列表分页结果。
type OfflineTaskPage struct {
	Page      int           `json:"page"`
	PageCount int           `json:"page_count"`
	Count     int           `json:"count"`
	Tasks     []OfflineTask `json:"tasks"`
}

// OfflineQuota 云下载配额信息。
type OfflineQuota struct {
	Count   int `json:"count"`
	Surplus int `json:"surplus"`
	Used    int `json:"used"`
}

// AddOfflineTasks 批量添加离线下载链接（http/https/magnet/ed2k）。不做输入校验（web 层负责）。
func (c *Client) AddOfflineTasks(ctx context.Context, urls []string, saveDirID, savePath string) ([]OfflineAddResult, error) {
	res, dur, err := Post[[]OfflineAddResult](ctx, c, "/open/offline/add_task_urls",
		Form{"urls": strings.Join(urls, "\n"), "wp_path_id": saveDirID})
	if err == nil {
		success := 0
		for _, r := range res {
			if r.State {
				success++
			}
		}
		logCall(ctx, "添加离线下载任务", nil, dur, "数量", len(urls), "路径", savePath, "成功", success)
	} else {
		logCall(ctx, "添加离线下载任务", err, dur, "数量", len(urls), "路径", savePath)
	}
	return res, err
}

// ListTasks 获取云下载任务列表一页（page 从 1 开始）。
func (c *Client) ListTasks(ctx context.Context, page int) (*OfflineTaskPage, error) {
	res, dur, err := Get[OfflineTaskPage](ctx, c, "/open/offline/get_task_list",
		Form{"page": strconv.Itoa(max(page, 1))})
	if err != nil {
		logCall(ctx, "获取云端任务列表", err, dur, "页码", max(page, 1))
		return nil, err
	}
	logCall(ctx, "获取云端任务列表", nil, dur, "页码", max(page, 1), "任务总数", res.Count, "本页条数", len(res.Tasks))
	return &res, nil
}

// DeleteTask 删除单个云下载任务；deleteFiles 为 true 时同时删除已下载的源文件。
func (c *Client) DeleteTask(ctx context.Context, infoHash string, deleteFiles bool) error {
	delSource := "0"
	if deleteFiles {
		delSource = "1"
	}
	_, dur, err := Post[jsontext.Value](ctx, c, "/open/offline/del_task",
		Form{"info_hash": infoHash, "del_source_file": delSource})
	logCall(ctx, "删除离线任务", err, dur, "info_hash", infoHash)
	return err
}

// ClearTasks 批量清除任务。flag：0 已完成，1 全部，2 失败，3 进行中，4 已完成且删源文件，5 全部且删源文件。
func (c *Client) ClearTasks(ctx context.Context, flag int) error {
	_, dur, err := Post[jsontext.Value](ctx, c, "/open/offline/clear_task", Form{"flag": strconv.Itoa(flag)})
	logCall(ctx, "批量清除任务", err, dur, "flag", flag)
	return err
}

// GetQuota 获取云下载配额信息。
func (c *Client) GetQuota(ctx context.Context) (*OfflineQuota, error) {
	res, dur, err := Get[OfflineQuota](ctx, c, "/open/offline/get_quota_info", nil)
	if err != nil {
		logCall(ctx, "获取云下载配额", err, dur)
		return nil, err
	}
	logCall(ctx, "获取云下载配额", nil, dur, "总数", res.Count, "已用", res.Used, "剩余", res.Surplus)
	return &res, nil
}

// AddTorrentFromData 添加 BT 种子离线任务：解析 bencode → 构造 magnet → 提交。
func AddTorrentFromData(ctx context.Context, c *Client, torrentData []byte, torrentName, savePath string) (*OfflineAddResult, error) {
	t0 := time.Now()
	slog.DebugContext(ctx, "添加种子离线任务", "文件名", torrentName, "save_path", savePath)
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
	results, err := c.AddOfflineTasks(ctx, []string{magnet}, saveDir.Fid, savePath)
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
	slog.InfoContext(ctx, "添加种子离线任务完成", "文件名", torrentName, "info_hash", r.InfoHash, "save_path", savePath, "耗时", time.Since(t0))
	return &r, nil
}
