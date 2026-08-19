package app

import (
	"context"
	"strings"

	"github.com/ytx-zhang/115tools/internal/drive"
)

// ──── 离线下载透传 ────
// web 层不能直连 drive（依赖单向 web → app → drive），离线下载操作在此薄透传。
// 一律经 App.API 取用（访问收口于 app 包）。

// OfflineTaskList 获取离线下载任务列表。
func (b *App) OfflineTaskList(ctx context.Context, page int) (*drive.OfflineTaskPage, error) {
	return b.API.ListTasks(ctx, page)
}

// OfflineQuotaInfo 获取离线下载配额。
func (b *App) OfflineQuotaInfo(ctx context.Context) (*drive.OfflineQuota, error) {
	return b.API.GetQuota(ctx)
}

// AddOfflineTasks 添加离线下载链接（薄透传至 drive 纯请求；链接的规范化/
// 校验已由 web 层完成）。savePath 为目标目录路径（仅日志定位用；FID 由调用方解析后传 dirID）。
func (b *App) AddOfflineTasks(ctx context.Context, urls []string, dirID, savePath string) ([]drive.OfflineAddResult, error) {
	return b.API.AddOfflineTasks(ctx, urls, dirID, savePath)
}

// AddTorrentTask 解析种子并添加 BT 任务（委托 drive 辅助函数：bencode→magnet→提交）。
func (b *App) AddTorrentTask(ctx context.Context, data []byte, name, savePath string) (*drive.OfflineAddResult, error) {
	return drive.AddTorrentFromData(ctx, b.API, data, name, savePath)
}

// DeleteOfflineTask 删除离线任务。
func (b *App) DeleteOfflineTask(ctx context.Context, infoHash string, deleteFiles bool) error {
	return b.API.DeleteTask(ctx, infoHash, deleteFiles)
}

// ClearOfflineTasks 批量清除离线任务。
func (b *App) ClearOfflineTasks(ctx context.Context, flag int) error {
	return b.API.ClearTasks(ctx, flag)
}

// ResolveCloudDir 把云端路径解析为目录 ID。空→strm_path，"/"→根目录("0")。
func (b *App) ResolveCloudDir(ctx context.Context, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = b.cfg.Snapshot().StrmPath
	}
	if path == "" || path == "/" {
		return "0", nil
	}
	info, err := b.API.GetDirInfo(ctx, path)
	if err != nil {
		return "", err
	}
	return info.Fid, nil
}
