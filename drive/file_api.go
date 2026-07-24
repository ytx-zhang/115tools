package drive

import (
	"context"
	"fmt"
	"strconv"
)

// 本文件是 115 文件/目录操作 API：下载直链、目录信息、文件列表、增删移改。

// DownloadUrlInfo 下载直链查询结果。
type DownloadUrlInfo struct {
	Fid  string // 文件 FID（.strm 缺 fid 时用它补全）
	Url  string // 真实下载地址（带时效）
	Name string // 云端文件名
}

// GetDownloadUrl 用 pickcode 换取文件的真实下载地址。
// ua 是 115 的强制校验项（服务端按 User-Agent 区分调用方），不能为空。
func (d *Open115) GetDownloadUrl(ctx context.Context, pickCode, ua string) (*DownloadUrlInfo, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	var res apiResponse[map[string]struct {
		FileName string `json:"file_name"`
		Url      struct {
			Url string `json:"url"`
		} `json:"url"`
	}]
	req := d.Client.R().SetContext(ctx)
	if ua == "" {
		return nil, fmt.Errorf("请求ua为空")
	}
	req.SetHeader("User-Agent", ua)
	req.SetFormData(map[string]string{
		"pick_code": pickCode,
	})
	req.SetResult(&res)
	_, err := req.Post("/open/ufile/downurl")
	if err != nil {
		return nil, err
	}

	for fid, item := range res.Data {
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
	if err = checkCtx(ctx); err != nil {
		return
	}
	var res apiResponse[struct {
		FileId string `json:"file_id"`
	}]
	req := d.Client.R().SetContext(ctx)
	req.SetFormData(map[string]string{
		"pid":       pid,
		"file_name": name,
	})
	req.SetResult(&res)
	if _, err = req.Post("/open/folder/add"); err != nil {
		return
	}
	fid = res.Data.FileId
	return
}

// MoveFile 把云端文件/目录移动到目标目录 cid。
// fid 支持逗号分隔的多个 ID（批量移动）。
func (d *Open115) MoveFile(ctx context.Context, fid, cid string) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	var res apiResponse[any]
	req := d.Client.R().SetContext(ctx)
	req.SetFormData(map[string]string{
		"file_ids": fid,
		"to_cid":   cid,
	})
	req.SetResult(&res)
	_, err := req.Post("/open/ufile/move")
	return err
}

// DeleteFile 删除云端文件/目录。fid 支持逗号分隔的多个 ID（批量删除）。
func (d *Open115) DeleteFile(ctx context.Context, fid string) error {
	if err := checkCtx(ctx); err != nil {
		return err
	}
	req := d.Client.R().SetContext(ctx)
	req.SetFormData(map[string]string{
		"file_ids": fid,
	})
	_, err := req.Post("/open/ufile/delete")
	return err
}

// UpdateFile 重命名云端文件/目录，返回改名后的实际文件名
// （115 可能只改主名不动扩展名，调用方需按需二次修正）。
func (d *Open115) UpdateFile(ctx context.Context, fid, name string) (newName string, err error) {
	if err = checkCtx(ctx); err != nil {
		return
	}
	var res apiResponse[struct {
		FileName string `json:"file_name"`
	}]
	req := d.Client.R().SetContext(ctx)
	req.SetFormData(map[string]string{
		"file_id":   fid,
		"file_name": name,
	})
	req.SetResult(&res)
	if _, err = req.Post("/open/ufile/update"); err != nil {
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
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	var res apiResponse[DirInfo]
	req := d.Client.R().SetContext(ctx)
	req.SetQueryParam("path", path)
	req.SetResult(&res)
	if _, err := req.Get("/open/folder/get_info"); err != nil {
		return nil, err
	}
	return &DirInfo{
		Fid:         res.Data.Fid,
		FileCount:   res.Data.FileCount,
		FolderCount: res.Data.FolderCount,
	}, nil
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
	if err := checkCtx(ctx); err != nil {
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
		if err := checkCtx(ctx); err != nil {
			return nil, err
		}
	}
	return allFiles, nil
}
