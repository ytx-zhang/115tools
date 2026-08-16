package drive

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// FinishLog 统一收尾一次云端操作的日志：err 非空打 Error(失败)，否则打 Info(完成)，
// 自动附加「耗时」，并原样返回 err 供调用方直接 return。
//
//	return FinishLog(t0, "云端移动文件", err, "数量", n)
func FinishLog(t0 time.Time, action string, err error, kvs ...any) error {
	kvs = append(kvs, "耗时", time.Since(t0))
	if err != nil {
		logs.Error(logs.ModuleCloud, action+"失败", append(kvs, "错误", err)...)
		return err
	}
	logs.Info(logs.ModuleCloud, action+"完成", kvs...)
	return nil
}

// ──── 目录 / 文件辅助函数 ────

// GetFileList 拉取云端目录 cid 下的全部子项（内部分页合并，每页 1150 条）。name 仅用于日志。
func (c *Client) GetFileList(ctx context.Context, cid, name string) ([]FileInfo, error) {
	t0 := time.Now()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	var all []FileInfo
	offset := 0
	for {
		files, err := Get[[]fileListResponse](ctx, c, "/open/ufile/files", Form{
			"cid":      cid,
			"show_dir": "1",
			"limit":    "1150",
			"offset":   strconv.Itoa(offset),
		})
		if err != nil {
			logs.Error(logs.ModuleCloud, "拉取云端文件列表失败", "路径", name, "错误", err, "耗时", time.Since(t0))
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
				IsVideo:  int64(item.IsVideo) == 1,
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
	logs.Info(logs.ModuleCloud, "拉取云端文件列表完成", "路径", name, "数量", len(all), "耗时", time.Since(t0))
	return all, nil
}

// GetDownloadUrl 用 pickcode 换取文件真实下载地址（空结果/空直链判断，取第一条）。
func (c *Client) GetDownloadUrl(ctx context.Context, pickCode, ua string) (*DownloadUrlInfo, error) {
	t0 := time.Now()
	// ⚠️ pickcode 无效/文件不可用时 115 返回 [] 而非 {fid:{...}}，用 StructOrArray 按空结果放行
	items, err := Post[StructOrArray[map[string]downItem]](ctx, c, "/open/ufile/downurl",
		Form{"pick_code": pickCode}, ReqWithUA(ua))
	if err != nil {
		return nil, err
	}
	if items.Value == nil || len(*items.Value) == 0 {
		return nil, fmt.Errorf("未提取到下载信息")
	}
	for _, item := range *items.Value {
		if item.Url.Value == nil || item.Url.Value.Url == "" {
			return nil, fmt.Errorf("115接口返回空直链")
		}
		logs.Info(logs.ModuleCloud, "获取下载直链完成", "pickcode", pickCode,
			"文件名", item.FileName, "耗时", time.Since(t0))
		return &DownloadUrlInfo{
			Url:  item.Url.Value.Url,
			Name: item.FileName,
		}, nil
	}
	return nil, fmt.Errorf("未提取到下载信息")
}

// ──── 文件 / 目录操作 ────

// CreateFolder 在云端目录 pid 下创建子目录 name，返回新目录的 FID。
// path 是目标完整路径（用于同名冲突时回查）；115 创建同名目录返回「该目录名称已存在」，
// 此时改用 GetDirInfo(path) 复用已存在目录的 FID 而非报错（幂等创建）。
// ⚠️ 用 message 判断而非 code：115 错误码不是稳定契约（20004 可能对应其他含义），
// message 是明确的语义信号。
func (c *Client) CreateFolder(ctx context.Context, pid, name, path string) (string, error) {
	t0 := time.Now()
	res, err := Post[struct {
		FileID string `json:"file_id"`
	}](ctx, c, "/open/folder/add", Form{"pid": pid, "file_name": name})
	if err == nil {
		return res.FileID, FinishLog(t0, "云端创建目录", nil, "文件名", name)
	}
	// 同名目录已存在：复用现有 FID，幂等创建
	if strings.Contains(err.Error(), "该目录名称已存在") {
		if info, gErr := c.GetDirInfo(ctx, path); gErr == nil && info != nil {
			logs.Info(logs.ModuleCloud, "云端目录已存在，复用", "路径", path, "FID", info.Fid, "耗时", time.Since(t0))
			return info.Fid, nil
		}
	}
	return "", FinishLog(t0, "云端创建目录", err, "文件名", name)
}

// MoveFile 把云端文件/目录移动到目标目录 cid。fid 支持逗号分隔批量。
func (c *Client) MoveFile(ctx context.Context, fid, cid string) error {
	t0 := time.Now()
	_, err := Post[struct{}](ctx, c, "/open/ufile/move", Form{"file_ids": fid, "to_cid": cid})
	return FinishLog(t0, "云端移动文件", err, "数量", fidCount(fid))
}

// DeleteFile 删除云端文件/目录。fid 支持逗号分隔批量。
func (c *Client) DeleteFile(ctx context.Context, fid string) error {
	t0 := time.Now()
	_, err := Post[struct{}](ctx, c, "/open/ufile/delete", Form{"file_ids": fid})
	return FinishLog(t0, "云端删除文件", err, "数量", fidCount(fid))
}

// fidCount 由逗号分隔的 fid 串估算文件数（用于日志计数）。
func fidCount(fid string) int {
	return strings.Count(fid, ",") + 1
}

// RenameFile 重命名云端文件/目录，返回改名后的实际文件名。
func (c *Client) RenameFile(ctx context.Context, fid, name string) (string, error) {
	t0 := time.Now()
	res, err := Post[struct {
		FileName string `json:"file_name"`
	}](ctx, c, "/open/ufile/update", Form{"file_id": fid, "file_name": name})
	return res.FileName, FinishLog(t0, "云端重命名", err, "fid", fid, "文件名", name)
}

// GetDirInfo 按路径查询云端目录信息。路径为空或仅含斜杠时短路返回根目录 fid="0"。
//
// ⚠️ 必须 POST + form（对齐 OpenList 的 GetFolderInfoByPath）：115 的 folder/get_info
// 按路径查询仅接受 POST 表单；用 GET + query 返回的不是目标目录的信息
// （count/folder_count 恒 0、file_id 错误），会导致上层误判 FID 变更并误重建目录（20004）。
func (c *Client) GetDirInfo(ctx context.Context, path string) (*DirInfo, error) {
	t0 := time.Now()
	if path == "" || strings.Trim(path, "/") == "" {
		return &DirInfo{Fid: "0"}, nil
	}
	path = "/" + strings.Trim(path, "/")
	// data 段可能是对象或数组（115 两种形态都出现过），用 StructOrArray 兼容
	info, err := Post[StructOrArray[DirInfo]](ctx, c, "/open/folder/get_info", Form{"path": path})
	if err != nil {
		logs.Error(logs.ModuleCloud, "查询云端目录信息失败", "路径", path, "错误", err, "耗时", time.Since(t0))
		return nil, err
	}
	if info.Value == nil {
		return nil, fmt.Errorf("未获取到目录信息")
	}
	infoPtr := info.Value
	logs.Info(logs.ModuleCloud, "查询云端目录信息完成", "路径", path,
		"文件数", infoPtr.FileCount, "目录数", infoPtr.FolderCount, "耗时", time.Since(t0))
	return infoPtr, nil
}
