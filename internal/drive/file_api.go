package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// 本文件是 115 文件/目录操作 API：下载直链、目录信息、文件列表、增删移改。

// DownloadUrlInfo 下载直链查询结果。
type DownloadUrlInfo struct {
	Fid  string // 文件 FID（.strm 缺 fid 时用它补全）
	Url  string // 真实下载地址（带时效）
	Name string // 云端文件名
}

// downloadItem 是 GetDownloadUrl 的 data 单条条目类型。
type downloadItem struct {
	FileName string `json:"file_name"`
	Url      struct {
		Url string `json:"url"`
	} `json:"url"`
}

// GetDownloadUrl 用 pickcode 换取文件的真实下载地址。
// ua 会被 115 记入直链绑定，回源 CDN 时须带相同 UA（透传模式已保证非空）。
// ⚠️ 115 在 pickcode 无效/文件不可用时可能返回 {"state":true,"data":[]}（空数组），
// 而非常规的 {"fid":{...}} 对象，故先用 json.RawMessage 承接再手动解析。
func (d *Open115) GetDownloadUrl(ctx context.Context, pickCode, ua string) (*DownloadUrlInfo, error) {
	t0 := time.Now()
	res, err := doAPI[json.RawMessage](ctx, d, "POST", "/open/ufile/downurl",
		withHeader("User-Agent", ua),
		withForm(map[string]string{"pick_code": pickCode}),
	)
	if err != nil {
		return nil, err
	}

	var data map[string]downloadItem
	if err := json.Unmarshal(res.Data, &data); err != nil {
		return nil, fmt.Errorf("解析下载信息失败: %w, 原始数据: %s", err, truncateBody(res.Data))
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("未提取到下载信息, 原始数据: %s", truncateBody(res.Data))
	}
	for fid, item := range data {
		logs.Info(logs.ModuleCloud, "获取下载直链完成", "pickcode", pickCode,
			"文件名", item.FileName, "耗时", time.Since(t0))
		return &DownloadUrlInfo{
			Fid:  fid,
			Url:  item.Url.Url,
			Name: item.FileName,
		}, nil
	}
	return nil, fmt.Errorf("未提取到下载信息")
}

// AddFolder 在云端目录 pid 下创建子目录 name，返回新目录的 FID。
func (d *Open115) AddFolder(ctx context.Context, pid, name string) (fid string, err error) {
	t0 := time.Now()
	res, err := doAPI[struct {
		FileId string `json:"file_id"`
	}](ctx, d, "POST", "/open/folder/add",
		withForm(map[string]string{"pid": pid, "file_name": name}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "云端创建目录失败", "文件名", name, "错误", err, "耗时", time.Since(t0))
		return
	}
	fid = res.Data.FileId
	// 已有文件名可读标识，fid 内部编号不打印
	logs.Info(logs.ModuleCloud, "云端创建目录完成", "文件名", name, "耗时", time.Since(t0))
	return
}

// MoveFile 把云端文件/目录移动到目标目录 cid。
// fid 支持逗号分隔的多个 ID（批量移动）。
func (d *Open115) MoveFile(ctx context.Context, fid, cid string) error {
	t0 := time.Now()
	_, err := doAPI[any](ctx, d, "POST", "/open/ufile/move",
		withForm(map[string]string{"file_ids": fid, "to_cid": cid}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "云端移动文件失败", "数量", strings.Count(fid, ",")+1, "错误", err, "耗时", time.Since(t0))
		return err
	}
	logs.Info(logs.ModuleCloud, "云端移动完成", "数量", strings.Count(fid, ",")+1, "耗时", time.Since(t0))
	return nil
}

// DeleteFile 删除云端文件/目录。fid 支持逗号分隔的多个 ID（批量删除）。
func (d *Open115) DeleteFile(ctx context.Context, fid string) error {
	t0 := time.Now()
	_, err := doAPI[any](ctx, d, "POST", "/open/ufile/delete",
		withForm(map[string]string{"file_ids": fid}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "云端删除文件失败", "数量", strings.Count(fid, ",")+1, "错误", err, "耗时", time.Since(t0))
		return err
	}
	logs.Info(logs.ModuleCloud, "云端删除完成", "数量", strings.Count(fid, ",")+1, "耗时", time.Since(t0))
	return nil
}

// UpdateFile 重命名云端文件/目录，返回改名后的实际文件名
// （115 可能只改主名不动扩展名，调用方需按需二次修正）。
func (d *Open115) UpdateFile(ctx context.Context, fid, name string) (newName string, err error) {
	t0 := time.Now()
	res, err := doAPI[struct {
		FileName string `json:"file_name"`
	}](ctx, d, "POST", "/open/ufile/update",
		withForm(map[string]string{"file_id": fid, "file_name": name}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "云端重命名失败", "fid", fid, "文件名", name, "错误", err, "耗时", time.Since(t0))
		return
	}
	newName = res.Data.FileName
	// 已有文件名可读标识，fid 内部编号不打印
	logs.Info(logs.ModuleCloud, "云端重命名完成", "文件名", newName, "耗时", time.Since(t0))
	return
}

// DirInfo 目录信息：FID 与直属子项计数（用于云端遍历的「计数跳过」优化）。
type DirInfo struct {
	Fid         string `json:"file_id"`
	FileCount   int64  `json:"count"`
	FolderCount int64  `json:"folder_count"`
}

// GetDirInfo 按路径查询云端目录信息。调用云端 API，完成日志统一 Info（只打结束）。
func (d *Open115) GetDirInfo(ctx context.Context, path string) (*DirInfo, error) {
	t0 := time.Now()
	res, err := doAPI[DirInfo](ctx, d, "GET", "/open/folder/get_info",
		withQuery(map[string]string{"path": path}),
	)
	if err != nil {
		// 失败常由调用方回退全量同步（walk.go）或终止初始化（env.go），故只打 Warn
		logs.Warn(logs.ModuleCloud, "查询云端目录信息失败", "路径", path, "错误", err, "耗时", time.Since(t0))
		return nil, err
	}
	// 已有路径定位，FID 内部编号不打印
	logs.Info(logs.ModuleCloud, "查询云端目录信息完成", "路径", path,
		"文件数", res.Data.FileCount, "目录数", res.Data.FolderCount, "耗时", time.Since(t0))
	return &res.Data, nil
}

// FileInfo 文件列表中的单个子项。
type FileInfo struct {
	Fid      string // 文件/目录 FID
	Name     string // 名称
	PickCode string // pickcode（换下载直链用）
	Size     int64  // 大小（字节）
	IsDir    bool   // 是否目录
	IsVideo  bool   // 是否视频（115 服务端判定）
}

// GetFileList 拉取云端目录 cid 下的全部子项（自动处理分页，每页 1150 条）。
// name 为目录名（仅用于日志展示）。
// 注意：仅返回 Aid == "1" 的条目（过滤掉非常规挂载项）。
// 调用云端 API，完成日志统一 Info（只打结束）。
func (d *Open115) GetFileList(ctx context.Context, cid, name string) ([]FileInfo, error) {
	t0 := time.Now()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	type fileListResponse struct {
		State   bool   `json:"state"`
		Message string `json:"message"`
		Code    int    `json:"code"`
		Count   int64  `json:"count"`
		Data    []struct {
			Fid      string `json:"fid"`
			Name     string `json:"fn"`
			PickCode string `json:"pc"`
			Size     int64  `json:"fs"`
			IsVideo  int    `json:"isv"`
			Aid      string `json:"aid"`
			IsDir    string `json:"fc"`
		} `json:"data"`
	}
	var allFiles []FileInfo
	offset := 0
	req := d.Client.R().SetContext(ctx)
	req.SetQueryParams(map[string]string{
		"cid":      cid,
		"show_dir": "1",
		"limit":    "1150",
	})

	for {
		var res fileListResponse
		req.SetQueryParam("offset", strconv.Itoa(offset))
		req.SetResult(&res)

		if _, err := req.Get("/open/ufile/files"); err != nil {
			logs.Error(logs.ModuleCloud, "拉取云端文件列表失败", "路径", name, "错误", err, "耗时", time.Since(t0))
			return nil, err
		}
		if offset == 0 && res.Count > 0 {
			allFiles = make([]FileInfo, 0, res.Count)
		}
		items := res.Data
		if len(items) == 0 {
			break
		}

		for _, item := range items {
			if item.Aid != "1" {
				continue
			}
			allFiles = append(allFiles, FileInfo{
				Fid:      item.Fid,
				Name:     item.Name,
				PickCode: item.PickCode,
				Size:     item.Size,
				IsDir:    item.IsDir == "0",
				IsVideo:  item.IsVideo == 1,
			})
		}
		// 退出条件以已消耗的条目数与云端总数比对为准，避免 Aid 过滤导致的计数偏差
		offset += len(items)
		if int64(offset) >= res.Count {
			break
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
	}
	// walk 传完整路径 / token 传"根目录"；统一显示为路径定位
	logs.Info(logs.ModuleCloud, "拉取云端文件列表完成", "路径", name, "数量", len(allFiles), "耗时", time.Since(t0))
	return allFiles, nil
}
