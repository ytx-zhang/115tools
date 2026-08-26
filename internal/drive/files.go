package drive

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ──── 目录 / 文件辅助函数 ────

// GetFileList 拉取云端目录 cid 下的全部子项（内部分页合并，每页 1150 条）。name 仅用于日志。
func (c *Client) GetFileList(ctx context.Context, cid, name string) ([]FileInfo, error) {
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
		// 每页一条日志（页码=offset/1150+1），并补充云端返回的该页条数
		logCloud("拉取云端文件列表", err, dur, "路径", name, "页码", offset/1150+1, "条数", len(files))
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
		offset += len(files) // 以原始返回条数推进（含被过滤项）
		if len(files) < 1150 {
			break // 返回不足一页说明已是最后一页
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
	}
	return all, nil
}

// GetDownloadUrl 用 pickcode 换取文件真实下载地址（空结果/空直链判断，取第一条）。
// path 为日志定位用的路径（可选，调用方有本地路径时传入；播放取链场景只有 pickcode 可传空）。
func (c *Client) GetDownloadUrl(ctx context.Context, pickCode, ua, path string) (*DownloadUrlInfo, error) {
	// ⚠️ pickcode 无效/文件不可用时 115 返回 [] 而非 {fid:{...}}，用 StructOrArray 按空结果放行
	logInfo := []any{"pickcode", pickCode}
	if path != "" {
		logInfo = slices.Concat([]any{"路径", path}, logInfo)
	}
	items, dur, err := Post[StructOrArray[map[string]downItem]](ctx, c, "/open/ufile/downurl",
		Form{"pick_code": pickCode}, ReqWithUA(ua))
	if err != nil {
		logCloud("获取下载直链", err, dur, logInfo...)
		return nil, err
	}
	if items.Value == nil || len(*items.Value) == 0 {
		logCloud("获取下载直链", fmt.Errorf("未提取到下载信息"), dur, logInfo...)
		return nil, fmt.Errorf("未提取到下载信息")
	}
	for _, item := range *items.Value {
		if item.Url.Url == "" {
			logCloud("获取下载直链", fmt.Errorf("115接口返回空直链"), dur, logInfo...)
			return nil, fmt.Errorf("115接口返回空直链")
		}
		// 成功：补充云端返回的文件名
		logCloud("获取下载直链", nil, dur, append(logInfo, "文件名", item.FileName)...)
		return &DownloadUrlInfo{
			Url:  item.Url.Url,
			Name: item.FileName,
		}, nil
	}
	logCloud("获取下载直链", fmt.Errorf("未提取到下载信息"), dur, logInfo...)
	return nil, fmt.Errorf("未提取到下载信息")
}

// ──── 文件 / 目录操作 ────

// CreateFolder 在云端目录 pid 下创建子目录 name，返回新目录的 FID。
// path 是目标完整路径（用于同名冲突时回查）；115 创建同名目录返回「该目录名称已存在」，
// 此时改用 GetDirInfo(path) 复用已存在目录的 FID 而非报错（幂等创建）。
// ⚠️ 用 message 判断而非 code：115 错误码不是稳定契约（20004 可能对应其他含义），
// message 是明确的语义信号。
func (c *Client) CreateFolder(ctx context.Context, pid, name, path string) (string, error) {
	res, dur, err := Post[struct {
		FileID string `json:"file_id"`
	}](ctx, c, "/open/folder/add", Form{"pid": pid, "file_name": name})
	if err == nil {
		logCloud("云端创建目录", nil, dur, "路径", path)
		return res.FileID, nil
	}
	// 同名目录已存在：复用现有 FID，幂等创建
	if strings.Contains(err.Error(), "该目录名称已存在") {
		if info, gErr := c.GetDirInfo(ctx, path); gErr == nil && info != nil {
			// 复用已存在目录，视为成功（不再打本次失败日志，GetDirInfo 已有自己的日志）
			return info.Fid, nil
		}
	}
	logCloud("云端创建目录", err, dur, "路径", path)
	return "", err
}

// MoveFile 把云端文件/目录移动到目标目录 cid。fid 支持逗号分隔批量。
// path 为日志定位用的路径（批量时传代表路径），仅用于 exec 动作日志。
// 成功时 115 返回 data=[]（空数组），用 RawMessage 容错（只关心 state，不消费 data）。
func (c *Client) MoveFile(ctx context.Context, fid, cid, path string) error {
	_, dur, err := Post[jsontext.Value](ctx, c, "/open/ufile/move", Form{"file_ids": fid, "to_cid": cid})
	logCloud("云端移动文件", err, dur, "路径", path, "数量", fidCount(fid))
	return err
}

// DeleteFile 删除云端文件/目录。fid 支持逗号分隔批量。
// path 为日志定位用的路径（批量时传代表路径），仅用于动作日志。
// 成功时 115 返回 data=[]（空数组），用 RawMessage 容错（只关心 state，不消费 data）。
func (c *Client) DeleteFile(ctx context.Context, fid, path string) error {
	_, dur, err := Post[jsontext.Value](ctx, c, "/open/ufile/delete", Form{"file_ids": fid})
	logCloud("云端删除文件", err, dur, "路径", path, "数量", fidCount(fid))
	return err
}

// fidCount 由逗号分隔的 fid 串估算文件数（用于日志计数）。
func fidCount(fid string) int {
	return strings.Count(fid, ",") + 1
}

// RenameFile 重命名云端文件/目录，返回改名后的实际文件名。
// path 为日志定位用的路径（调用方传入的源文件路径），仅用于 exec 动作日志。
func (c *Client) RenameFile(ctx context.Context, fid, name, path string) (string, error) {
	res, dur, err := Post[struct {
		FileName string `json:"file_name"`
	}](ctx, c, "/open/ufile/update", Form{"file_id": fid, "file_name": name})
	if err == nil {
		// 成功：补充云端返回的最终文件名
		logCloud("云端重命名", nil, dur, "路径", path, "新文件名", res.FileName)
	} else {
		logCloud("云端重命名", err, dur, "路径", path)
	}
	return res.FileName, err
}

// GetDirInfo 按路径查询云端目录信息。路径为空或仅含斜杠时短路返回根目录 fid="0"。
//
// ⚠️ 必须 POST + form（对齐 OpenList 的 GetFolderInfoByPath）：115 的 folder/get_info
// 按路径查询仅接受 POST 表单；用 GET + query 返回的不是目标目录的信息
// （count/folder_count 恒 0、file_id 错误），会导致上层误判 FID 变更并误重建目录（20004）。
func (c *Client) GetDirInfo(ctx context.Context, path string) (*DirInfo, error) {
	if path == "" || strings.Trim(path, "/") == "" {
		return &DirInfo{Fid: "0"}, nil
	}
	path = "/" + strings.Trim(path, "/")
	// data 段可能是对象或数组（115 两种形态都出现过），用 StructOrArray 兼容
	info, dur, err := Post[StructOrArray[DirInfo]](ctx, c, "/open/folder/get_info", Form{"path": path})
	if err != nil {
		logCloud("查询云端目录信息", err, dur, "路径", path)
		return nil, err
	}
	if info.Value == nil {
		logCloud("查询云端目录信息", fmt.Errorf("未获取到目录信息"), dur, "路径", path)
		return nil, fmt.Errorf("未获取到目录信息")
	}
	// 成功：补充云端返回的子项计数（FID 无可读性不显示）
	logCloud("查询云端目录信息", nil, dur,
		"路径", path, "文件数", info.Value.FileCount, "目录数", info.Value.FolderCount)
	return info.Value, nil
}
