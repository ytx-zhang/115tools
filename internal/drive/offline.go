package drive

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// ──── 离线下载原子方法 ────

// AddOfflineTasks 批量添加离线下载链接（http/https/magnet/ed2k），保存到 saveDirID。
// 仅负责向 115 发起请求并解析响应，不做任何用户输入校验/规范化（由调用方 web 层负责）。
// savePath 为日志定位用的目标目录路径（调用方传入），仅用于动作日志。
func (c *Client) AddOfflineTasks(ctx context.Context, urls []string, saveDirID, savePath string) ([]OfflineAddResult, error) {
	res, dur, err := Post[[]OfflineAddResult](ctx, c, "/open/offline/add_task_urls",
		Form{
			"urls":       strings.Join(urls, "\n"),
			"wp_path_id": saveDirID,
		})
	if err == nil {
		// 成功：补充云端返回的实际添加成功条数
		success := 0
		for _, r := range res {
			if r.State {
				success++
			}
		}
		logCloud("添加离线下载任务", nil, dur, "数量", len(urls), "路径", savePath, "成功", success)
	} else {
		logCloud("添加离线下载任务", err, dur, "数量", len(urls), "路径", savePath)
	}
	return res, err
}

// ListTasks 获取云下载任务列表的一页（page 从 1 开始）。
func (c *Client) ListTasks(ctx context.Context, page int) (*OfflineTaskPage, error) {
	res, dur, err := Get[OfflineTaskPage](ctx, c, "/open/offline/get_task_list",
		Form{"page": strconv.Itoa(max(page, 1))})
	if err != nil {
		logCloud("获取云端任务列表", err, dur, "页码", max(page, 1))
		return nil, err
	}
	// 成功：补充云端返回的任务总数与本页条数
	logCloud("获取云端任务列表", nil, dur, "页码", max(page, 1), "任务总数", res.Count, "本页条数", len(res.Tasks))
	return &res, nil
}

// DeleteTask 删除单个云下载任务；deleteFiles 为 true 时同时删除已下载的源文件。
func (c *Client) DeleteTask(ctx context.Context, infoHash string, deleteFiles bool) error {
	delSource := "0"
	if deleteFiles {
		delSource = "1"
	}
	// 成功时 data=[]（空数组），用 RawMessage 容错（只关心 state，不消费 data）。
	_, dur, err := Post[jsontext.Value](ctx, c, "/open/offline/del_task",
		Form{"info_hash": infoHash, "del_source_file": delSource})
	logCloud("删除离线任务", err, dur, "info_hash", infoHash)
	return err
}

// ClearTasks 批量清除任务。
// flag：0 已完成，1 全部，2 失败，3 进行中，4 已完成且删源文件，5 全部且删源文件。
func (c *Client) ClearTasks(ctx context.Context, flag int) error {
	// 成功时 data=[]（空数组），用 RawMessage 容错（只关心 state，不消费 data）。
	_, dur, err := Post[jsontext.Value](ctx, c, "/open/offline/clear_task",
		Form{"flag": strconv.Itoa(flag)})
	logCloud("批量清除任务", err, dur, "flag", flag)
	return err
}

// GetQuota 获取云下载配额信息。
// data 段即 {count, surplus, used}（对齐 OpenList 的 OfflineQuotaInfo），用 Call 直接解析。
func (c *Client) GetQuota(ctx context.Context) (*OfflineQuota, error) {
	res, dur, err := Get[OfflineQuota](ctx, c, "/open/offline/get_quota_info", nil)
	if err != nil {
		logCloud("获取云下载配额", err, dur)
		return nil, err
	}
	// 成功：补充云端返回的配额（总数/已用/剩余）
	logCloud("获取云下载配额", nil, dur, "总数", res.Count, "已用", res.Used, "剩余", res.Surplus)
	return &res, nil
}

// ──── 离线下载辅助函数 ────

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
	logs.Info(logs.ModuleCloud, "添加种子离线任务完成", "文件名", torrentName,
		"info_hash", r.InfoHash, "save_path", savePath, "耗时", time.Since(t0))
	return &r, nil
}
