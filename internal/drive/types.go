package drive

import (
	"bytes"
	"encoding/json/v2"
)

// DownloadURLInfo 下载直链查询结果。
type DownloadURLInfo struct {
	URL  string // 真实下载地址（带时效）
	Name string // 云端文件名
}

// DirInfo 目录信息：FID、名称与直属子项计数（用于云端遍历的「计数跳过」优化）。
type DirInfo struct {
	Fid         string `json:"file_id"`
	Name        string `json:"file_name"`
	FileCount   int64  `json:"count"`
	FolderCount int64  `json:"folder_count"`
}

// FileInfo 文件列表中的单个子项。
type FileInfo struct {
	Fid      string
	Name     string
	PickCode string
	Size     int64
	IsDir    bool
	IsVideo  bool
}

// UploadFileInfo 上传成功结果：云端 FID 与 pickcode。
type UploadFileInfo struct {
	Fid      string
	PickCode string
}

// StructOrArray 兼容「对象字段可能返回对象 / 数组 / 布尔 / null」的多形态 JSON。
// 115（PHP 后端）在字段无值时常返回 []/false/null 而非 {}，直接按对象解析会整体失败。
// 非对象形态一律置零放行，由业务层按需处理。
type StructOrArray[T any] struct {
	Value *T
}

// UnmarshalJSON 仅接受以 '{' 开头的对象形态，其余置零放行。
func (s *StructOrArray[T]) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		s.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return err
	}
	s.Value = &v
	return nil
}

// ──── 私有响应结构（云端原始返回 JSON 承载） ────

// fileListResponse 是 /open/ufile/files 响应 data 段的单条文件项。
type fileListResponse struct {
	Fid      string `json:"fid"`
	Name     string `json:"fn"`
	PickCode string `json:"pc"`
	Size     int64  `json:"fs"`
	IsVideo  int64  `json:"isv"`
	Aid      string `json:"aid"`
	IsDir    string `json:"fc"`
}

// uploadInitResp 是 /open/upload/init 响应 data 段字段（只取需要的）。
type uploadInitResp struct {
	Status    int                        `json:"status"`
	FileID    string                     `json:"file_id"`
	PickCode  string                     `json:"pick_code"`
	SignKey   string                     `json:"sign_key"`
	SignCheck string                     `json:"sign_check"`
	Bucket    string                     `json:"bucket"`
	Object    string                     `json:"object"`
	Callback  StructOrArray[OssCallback] `json:"callback"`
}
