package drive

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/logs"
	"strconv"
	"strings"
	"time"
)

// 离线下载（云下载）相关接口封装。
// 实现方式参考 OpenList 115open 驱动（OpenListTeam/115-sdk-go offline.go）。

// OfflineAddResult 单条链接的添加结果。
type OfflineAddResult struct {
	State    bool   `json:"state"`     // 该链接是否添加成功
	Code     int64  `json:"code"`      // 状态码，成功为 0
	Message  string `json:"message"`   // 状态描述
	InfoHash string `json:"info_hash"` // 任务 sha1，仅成功时返回
	Url      string `json:"url"`       // 原始链接
}

// AddOfflineTasks 批量添加离线下载链接（http/https/magnet/ed2k），
// 保存到 saveDirID 对应的云端目录（"0" 表示根目录）。
func (d *Open115) AddOfflineTasks(ctx context.Context, urls []string, saveDirID string) ([]OfflineAddResult, error) {
	t0 := time.Now()
	logs.Info(logs.ModuleCloud, "添加离线下载任务", "数量", len(urls), "目标目录", saveDirID)
	res, err := doAPI[[]OfflineAddResult](ctx, d, "POST", "/open/offline/add_task_urls",
		withForm(map[string]string{
			"urls":       strings.Join(urls, "\n"),
			"wp_path_id": saveDirID,
		}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "添加离线下载任务失败", "数量", len(urls), "错误", err, "耗时", time.Since(t0))
		return nil, err
	}
	logs.Info(logs.ModuleCloud, "添加离线下载任务完成", "数量", len(urls), "目标目录", saveDirID, "耗时", time.Since(t0))
	return res.Data, nil
}

// OfflineTask 一条云下载任务。
type OfflineTask struct {
	InfoHash    string  `json:"info_hash"`   // 任务 sha1
	Name        string  `json:"name"`        // 任务名
	Url         string  `json:"url"`         // 链接
	AddTime     int64   `json:"add_time"`    // 添加时间戳
	LastUpdate  int64   `json:"last_update"` // 最后更新时间戳
	Size        int64   `json:"size"`        // 总大小（字节）
	PercentDone float64 `json:"percentDone"` // 下载进度 0-100
	// Status 任务状态：-1 失败，0 分配中，1 下载中，2 成功
	Status       int    `json:"status"`
	FileID       string `json:"file_id"`        // 下载完成后的文件/文件夹 ID
	DeleteFileID string `json:"delete_file_id"` // 删除源文件时需传递的 ID
	WpPathID     string `json:"wp_path_id"`     // 所在父目录 ID
}

// OfflineTaskPage 任务列表分页结果。
type OfflineTaskPage struct {
	Page      int           `json:"page"`       // 当前页码
	PageCount int           `json:"page_count"` // 总页数
	Count     int           `json:"count"`      // 任务总数
	Tasks     []OfflineTask `json:"tasks"`      // 任务列表
}

// OfflineTaskList 获取云下载任务列表（page 从 1 开始）。
func (d *Open115) OfflineTaskList(ctx context.Context, page int) (*OfflineTaskPage, error) {
	res, err := doAPI[OfflineTaskPage](ctx, d, "GET", "/open/offline/get_task_list",
		withQuery(map[string]string{"page": strconv.Itoa(max(page, 1))}),
	)
	if err != nil {
		return nil, err
	}
	return &res.Data, nil
}

// DeleteOfflineTask 删除单个云下载任务；deleteFiles 为 true 时同时删除已下载的源文件。
func (d *Open115) DeleteOfflineTask(ctx context.Context, infoHash string, deleteFiles bool) error {
	t0 := time.Now()
	logs.Info(logs.ModuleCloud, "删除离线任务", "info_hash", infoHash, "删除源文件", deleteFiles)
	form := map[string]string{
		"info_hash":       infoHash,
		"del_source_file": "0",
	}
	if deleteFiles {
		form["del_source_file"] = "1"
	}
	_, err := doAPI[any](ctx, d, "POST", "/open/offline/del_task",
		withForm(form),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "删除离线任务失败", "info_hash", infoHash, "错误", err, "耗时", time.Since(t0))
		return err
	}
	logs.Info(logs.ModuleCloud, "删除离线任务完成", "info_hash", infoHash, "耗时", time.Since(t0))
	return nil
}

// ClearOfflineTasks 批量清除任务。
// flag：0 已完成，1 全部，2 失败，3 进行中，4 已完成且删源文件，5 全部且删源文件。
func (d *Open115) ClearOfflineTasks(ctx context.Context, flag int) error {
	t0 := time.Now()
	logs.Info(logs.ModuleCloud, "批量清除离线任务", "flag", flag)
	_, err := doAPI[any](ctx, d, "POST", "/open/offline/clear_task",
		withForm(map[string]string{"flag": strconv.Itoa(flag)}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "批量清除离线任务失败", "flag", flag, "错误", err, "耗时", time.Since(t0))
		return err
	}
	logs.Info(logs.ModuleCloud, "批量清除离线任务完成", "flag", flag, "耗时", time.Since(t0))
	return nil
}

// OfflineQuota 云下载配额信息。
type OfflineQuota struct {
	Count   int `json:"count"`   // 总配额
	Used    int `json:"used"`    // 已用
	Surplus int `json:"surplus"` // 剩余
}

// OfflineQuotaInfo 获取云下载配额信息。
func (d *Open115) OfflineQuotaInfo(ctx context.Context) (*OfflineQuota, error) {
	res, err := doAPI[OfflineQuota](ctx, d, "GET", "/open/offline/get_quota_info")
	if err != nil {
		return nil, err
	}
	return &res.Data, nil
}

// ────────────────────────────── 种子（BT）离线下载 ──────────────────────────────

// TorrentFile 种子内包含的单个文件信息（字段名对齐 115 /open/offline/torrent）。
type TorrentFile struct {
	Path   string `json:"path"`   // 文件相对路径
	Size   int64  `json:"size"`   // 字节大小
	Wanted int    `json:"wanted"` // 默认选中标志：1 选中 / 0 未选
}

// TorrentInfo 种子解析结果（字段名对齐 115 /open/offline/torrent 的 data 部分）。
type TorrentInfo struct {
	InfoHash  string        `json:"info_hash"`
	Name      string        `json:"torrent_name"`
	TotalSize int64         `json:"file_size"`
	FileCount int           `json:"file_count"` // 文件总数，用于构建 wanted 索引
	Files     []TorrentFile `json:"torrent_filelist"`
}

// ParseTorrent 用已有的 torrent_sha1 + pick_code 解析种子。
func (d *Open115) ParseTorrent(ctx context.Context, torrentSha1, pickCode string) (*TorrentInfo, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	// 响应为 {state, code, data:{...}} 包装，data 内才是种子详情
	res, err := doAPI[json.RawMessage](ctx, d, "POST", "/open/offline/torrent",
		withForm(map[string]string{
			"torrent_sha1": torrentSha1,
			"pick_code":    pickCode,
		}),
	)
	if err != nil {
		return nil, err
	}
	if res.Data == nil {
		return nil, fmt.Errorf("种子解析响应缺少 data 字段")
	}
	var info TorrentInfo
	if err := json.Unmarshal(res.Data, &info); err != nil {
		return nil, fmt.Errorf("解析种子信息失败: %w", err)
	}
	if info.InfoHash == "" {
		return nil, fmt.Errorf("种子解析失败，未获取到 info_hash")
	}
	logs.Info(logs.ModuleCloud, "解析种子成功",
		"info_hash", info.InfoHash,
		"文件名", info.Name,
		"file_count", info.FileCount,
		"文件列表长度", len(info.Files),
	)
	return &info, nil
}

// AddOfflineTaskBT 添加一个 BT 离线下载任务。wanted 为要下载的文件下标列表
// （0 基、逗号分隔），空字符串表示下载全部文件；savePath 为相对根目录的保存
// 路径串（必填非空，空串报 990002）；wpPathID 选填，传了则 savePath 相对该目录。
func (d *Open115) AddOfflineTaskBT(ctx context.Context, infoHash, wanted, torrentSha1, pickCode, wpPathID, savePath string) (*OfflineAddResult, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	form := map[string]string{
		"info_hash":    infoHash,
		"wanted":       wanted,
		"torrent_sha1": torrentSha1,
		"pick_code":    pickCode,
	}
	// save_path 必填非空（空串或省略均报 990002 参数错误），
	// 取值为相对根目录的路径串（如 "A/B" → 根目录/A/B/）。
	form["save_path"] = savePath
	// wp_path_id 选填：传了才带上（此时 save_path 相对该目录）。
	if wpPathID != "" {
		form["wp_path_id"] = wpPathID
	}
	// 开始日志不打印（结果由 web 层「添加种子任务」完成日志体现）
	// 成功时 data 可能为空/非数组，用 RawMessage 兼容解析
	res, err := doAPI[json.RawMessage](ctx, d, "POST", "/open/offline/add_task_bt",
		withForm(form),
	)
	if err != nil {
		return nil, err
	}
	// 走到这里说明 state=true（失败已被 OnAfterResponse 中间件拦截报错）。
	// data 若带结果则取第一条，否则用已知 info_hash 构造成功结果。
	var list []OfflineAddResult
	if json.Unmarshal(res.Data, &list) == nil && len(list) > 0 {
		return &list[0], nil
	}
	return &OfflineAddResult{State: true, InfoHash: infoHash}, nil
}

// AddTorrentTask 上传种子文件、解析、添加 BT 离线任务（一站式）。
// cid 为种子文件上传到的目录（云端临时存放，需目录 ID）；savePath 为离线下载
// 保存路径（相对根目录的路径串，如 "STRM/电影"，必填非空）。
func (d *Open115) AddTorrentTask(ctx context.Context, torrentData []byte, torrentName, cid, savePath string) (*OfflineAddResult, error) {
	logs.Info(logs.ModuleCloud, "添加种子离线任务", "文件名", torrentName, "save_path", savePath)
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	// 0. 计算种子文件 SHA1（115 要求大写；ParseTorrent / AddOfflineTaskBT 均用此值，而非云端 file_id）
	torrentSha1 := fmt.Sprintf("%X", sha1.Sum(torrentData))

	// 1. 上传种子文件
	ufi, err := d.UploadBytes(ctx, torrentName, torrentData, cid, "", "")
	if err != nil {
		return nil, fmt.Errorf("上传种子文件失败: %w", err)
	}
	// 2. 解析种子
	info, err := d.ParseTorrent(ctx, torrentSha1, ufi.PickCode)
	if err != nil {
		return nil, fmt.Errorf("解析种子失败: %w", err)
	}
	// 3. 构建 wanted：逗号分隔的“选中文件索引”，0 基（torrent_filelist 下标），
	//    下载全部即 0,1,...,N-1。优先用 file_count，缺失时退回文件列表长度。
	n := info.FileCount
	if n <= 0 {
		n = len(info.Files)
	}
	wanted := ""
	if n > 0 {
		idx := make([]string, 0, n)
		for i := range n {
			idx = append(idx, strconv.Itoa(i))
		}
		wanted = strings.Join(idx, ",")
	}
	// 4. 添加 BT 任务：save_path 传路径串（必填非空），wp_path_id 省略
	return d.AddOfflineTaskBT(ctx, info.InfoHash, wanted, torrentSha1, ufi.PickCode, "", savePath)
}
