package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

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
	logs.Info(logs.ModuleCloud, "获取下载直链", "pickcode", pickCode)
	res, err := doAPI[json.RawMessage](ctx, d, "POST", "/open/ufile/downurl",
		withHeader("User-Agent", ua),
		withForm(map[string]string{"pick_code": pickCode}),
	)
	if err != nil {
		return nil, err
	}

	var data map[string]downloadItem
	if err := json.Unmarshal(res.Data, &data); err != nil {
		return nil, fmt.Errorf("解析下载信息失败: %w", err)
	}

	for fid, item := range data {
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
	logs.Info(logs.ModuleCloud, "云端创建目录", "name", name, "pid", pid)
	res, err := doAPI[struct {
		FileId string `json:"file_id"`
	}](ctx, d, "POST", "/open/folder/add",
		withForm(map[string]string{"pid": pid, "file_name": name}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "云端创建目录失败", "name", name, "err", err)
		return
	}
	fid = res.Data.FileId
	return
}

// MoveFile 把云端文件/目录移动到目标目录 cid。
// fid 支持逗号分隔的多个 ID（批量移动）。
func (d *Open115) MoveFile(ctx context.Context, fid, cid string) error {
	logs.Info(logs.ModuleCloud, "云端移动文件", "fid", fid, "cid", cid)
	_, err := doAPI[any](ctx, d, "POST", "/open/ufile/move",
		withForm(map[string]string{"file_ids": fid, "to_cid": cid}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "云端移动文件失败", "fid", fid, "err", err)
	}
	return err
}

// DeleteFile 删除云端文件/目录。fid 支持逗号分隔的多个 ID（批量删除）。
func (d *Open115) DeleteFile(ctx context.Context, fid string) error {
	logs.Info(logs.ModuleCloud, "云端删除文件", "fid", fid)
	_, err := doAPI[any](ctx, d, "POST", "/open/ufile/delete",
		withForm(map[string]string{"file_ids": fid}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "云端删除文件失败", "fid", fid, "err", err)
	}
	return err
}

// UpdateFile 重命名云端文件/目录，返回改名后的实际文件名
// （115 可能只改主名不动扩展名，调用方需按需二次修正）。
func (d *Open115) UpdateFile(ctx context.Context, fid, name string) (newName string, err error) {
	logs.Info(logs.ModuleCloud, "云端重命名", "fid", fid, "name", name)
	res, err := doAPI[struct {
		FileName string `json:"file_name"`
	}](ctx, d, "POST", "/open/ufile/update",
		withForm(map[string]string{"file_id": fid, "file_name": name}),
	)
	if err != nil {
		logs.Error(logs.ModuleCloud, "云端重命名失败", "fid", fid, "err", err)
		return
	}
	newName = res.Data.FileName
	return
}

// DirInfo 目录信息：FID 与直属子项计数（用于云端遍历的「计数跳过」优化）。
type DirInfo struct {
	Fid         string `json:"file_id"`
	FileCount   int64  `json:"count"`
	FolderCount int64  `json:"folder_count"`
}

// GetDirInfo 按路径查询云端目录信息。
func (d *Open115) GetDirInfo(ctx context.Context, path string) (*DirInfo, error) {
	logs.Info(logs.ModuleCloud, "查询云端目录信息", "path", path)
	res, err := doAPI[DirInfo](ctx, d, "GET", "/open/folder/get_info",
		withQuery(map[string]string{"path": path}),
	)
	if err != nil {
		return nil, err
	}
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
// 注意：仅返回 Aid == "1" 的条目（过滤掉非常规挂载项）。
func (d *Open115) GetFileList(ctx context.Context, cid string) ([]FileInfo, error) {
	logs.Info(logs.ModuleCloud, "拉取云端文件列表", "cid", cid)
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
	return allFiles, nil
}
