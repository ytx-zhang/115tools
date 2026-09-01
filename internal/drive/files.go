package drive

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"strconv"
	"strings"
)

// GetFileList 拉取云端目录 cid 下全部子项（内部分页合并，每页 1150 条）。
func (c *Client) GetFileList(ctx context.Context, cid string) ([]FileInfo, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	var all []FileInfo
	offset := 0
	for {
		files, dur, err := Get[[]fileListResponse](ctx, c, "/open/ufile/files", Form{
			"cid":      cid,
			"show_dir": "1",
			"limit":    "1150",
			"offset":   strconv.Itoa(offset),
		})
		logCall(ctx, "拉取云端文件列表", err, dur, "页码", offset/1150+1, "条数", len(files))
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			break
		}
		for _, item := range files {
			if item.Aid != "1" {
				continue
			}
			all = append(all, FileInfo{
				Fid:      item.Fid,
				Name:     item.Name,
				PickCode: item.PickCode,
				Size:     item.Size,
				IsDir:    item.IsDir == "0",
				IsVideo:  item.IsVideo == 1,
			})
		}
		offset += len(files)
		if len(files) < 1150 {
			break
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
	}
	return all, nil
}

// GetDownloadURL 用 pickcode 换取真实下载地址（空结果/空直链判断，取第一条）。
func (c *Client) GetDownloadURL(ctx context.Context, pickCode, ua string) (*DownloadURLInfo, error) {
	items, dur, err := Post[StructOrArray[map[string]downItem]](ctx, c, "/open/ufile/downurl",
		Form{"pick_code": pickCode}, ReqWithUA(ua))
	if err != nil {
		logCall(ctx, "获取下载直链", err, dur, "pickcode", pickCode)
		return nil, err
	}
	if items.Value == nil || len(*items.Value) == 0 {
		logCall(ctx, "获取下载直链", fmt.Errorf("未提取到下载信息"), dur, "pickcode", pickCode)
		return nil, fmt.Errorf("未提取到下载信息")
	}
	for _, item := range *items.Value {
		if item.URL.URL == "" {
			logCall(ctx, "获取下载直链", fmt.Errorf("115接口返回空直链"), dur, "pickcode", pickCode)
			return nil, fmt.Errorf("115接口返回空直链")
		}
		logCall(ctx, "获取下载直链", nil, dur, "pickcode", pickCode, "文件名", item.FileName)
		return &DownloadURLInfo{URL: item.URL.URL, Name: item.FileName}, nil
	}
	logCall(ctx, "获取下载直链", fmt.Errorf("未提取到下载信息"), dur, "pickcode", pickCode)
	return nil, fmt.Errorf("未提取到下载信息")
}

// downItem 是下载直链响应 data 段的单条条目。
type downItem struct {
	FileName string  `json:"file_name"`
	URL      downURL `json:"url"`
}

type downURL struct {
	URL string `json:"url"`
}

// CreateFolder 在云端目录 pid 下创建子目录 name，返回新目录 FID；同名已存在则复用现有 FID。
func (c *Client) CreateFolder(ctx context.Context, pid, name, path string) (string, error) {
	res, dur, err := Post[struct {
		FileID string `json:"file_id"`
	}](ctx, c, "/open/folder/add", Form{"pid": pid, "file_name": name})
	if err == nil {
		logCall(ctx, "云端创建目录", nil, dur, "路径", path)
		return res.FileID, nil
	}
	if strings.Contains(err.Error(), "该目录名称已存在") {
		if info, gErr := c.GetDirInfo(ctx, path); gErr == nil && info != nil {
			return info.Fid, nil
		}
	}
	logCall(ctx, "云端创建目录", err, dur, "路径", path)
	return "", err
}

// MoveFile 把云端文件/目录移动到目标目录 cid（fid 支持逗号分隔批量）。
func (c *Client) MoveFile(ctx context.Context, fid, cid, path string) error {
	_, dur, err := Post[jsontext.Value](ctx, c, "/open/ufile/move", Form{"file_ids": fid, "to_cid": cid})
	logCall(ctx, "云端移动文件", err, dur, "路径", path, "数量", fidCount(fid))
	return err
}

// DeleteFile 删除云端文件/目录（fid 支持逗号分隔批量）。
func (c *Client) DeleteFile(ctx context.Context, fid, path string) error {
	_, dur, err := Post[jsontext.Value](ctx, c, "/open/ufile/delete", Form{"file_ids": fid})
	logCall(ctx, "云端删除文件", err, dur, "路径", path, "数量", fidCount(fid))
	return err
}

// RenameFile 重命名云端文件/目录，返回改名后的实际文件名。
func (c *Client) RenameFile(ctx context.Context, fid, name, path string) (string, error) {
	res, dur, err := Post[struct {
		FileName string `json:"file_name"`
	}](ctx, c, "/open/ufile/update", Form{"file_id": fid, "file_name": name})
	if err == nil {
		logCall(ctx, "云端重命名", nil, dur, "路径", path, "新文件名", res.FileName)
	} else {
		logCall(ctx, "云端重命名", err, dur, "路径", path)
	}
	return res.FileName, err
}

// GetDirInfo 按路径查询云端目录信息；根路径返回 fid="0"。
// 必须 POST + form：GET + query 返回非目标目录信息，会导致上层误判 FID 变更。
func (c *Client) GetDirInfo(ctx context.Context, path string) (*DirInfo, error) {
	if path == "" || strings.Trim(path, "/") == "" {
		return &DirInfo{Fid: "0"}, nil
	}
	path = "/" + strings.Trim(path, "/")
	info, dur, err := Post[StructOrArray[DirInfo]](ctx, c, "/open/folder/get_info", Form{"path": path})
	if err != nil {
		logCall(ctx, "查询云端目录信息", err, dur, "路径", path)
		return nil, err
	}
	if info.Value == nil {
		logCall(ctx, "查询云端目录信息", fmt.Errorf("未获取到目录信息"), dur, "路径", path)
		return nil, fmt.Errorf("未获取到目录信息")
	}
	logCall(ctx, "查询云端目录信息", nil, dur, "路径", path, "文件数", info.Value.FileCount, "目录数", info.Value.FolderCount)
	return info.Value, nil
}

func fidCount(fid string) int { return strings.Count(fid, ",") + 1 }
