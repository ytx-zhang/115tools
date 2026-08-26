// Package drive 提供 115 开放平台（refresh_token）驱动的完整实现。
// 架构借鉴 OpenList/115-sdk-go：标准库 net/http + 泛型请求入口（Get/Post）+
// 统一响应外壳（Resp[T]）+ 请求前 token 保活刷新 + StructOrArray[T] 泛型容错。
//
// 【代码划分】
//   - client.go：Client 装配（标准库 net/http：限流/鉴权/重试 + 全局 HTTP client）与泛型请求执行
//     （Get/Post 统一入口：自动解包 data 段 + state=false 自动报错）；
//   - types.go：数据结构与工具（文件/目录/上传/离线类型、StructOrArray 容错、IntString、
//     SHA1、私有响应结构）；
//   - token.go：访问令牌自动刷新（RefreshDaemon）与凭证验证（Verify）；
//   - files.go：文件/目录操作（下载直链、分页合并列表、增删移改）；
//   - upload.go：上传（秒传 init → get_token → singleUpload/multipartUpload + calPartSize 动态分片）；
//   - offline.go：离线下载（链接/BT 种子 + 辅助函数）；
//   - pickcode.go：pickcode 本地解码（PickcodeToID，秒传/strm fid 免网络补查）；
//   - bencode.go：种子 bencode 解析（ParseTorrentInfo）。
package drive

import (
	"bytes"
	"crypto/sha1"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// DownloadUrlInfo 下载直链查询结果。
type DownloadUrlInfo struct {
	Url  string // 真实下载地址（带时效）
	Name string // 云端文件名
}

// DirInfo 目录信息：FID、目录名与直属子项计数（用于云端遍历的「计数跳过」优化）。
// count/folder_count 实测为 JSON 数字（int），用 int64 直接解析。
type DirInfo struct {
	Fid         string `json:"file_id"`
	Name        string `json:"file_name"` // 目录名（同名文件夹场景用 GetDirInfo 回查时返回）
	FileCount   int64  `json:"count"`
	FolderCount int64  `json:"folder_count"`
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

// UploadFileInfo 上传成功结果：云端 FID 与 pickcode。
type UploadFileInfo struct {
	Fid      string
	PickCode string
}

// UploadCallbackData 是 OSS 上传完成后 115 回调返回体的 data 段。
type UploadCallbackData struct {
	FileID   string `json:"file_id"`
	PickCode string `json:"pick_code"`
}

// UploadInitReq 是上传初始化请求（业务层 → 传输层）。
type UploadInitReq struct {
	FileName string
	FileSize int64
	Cid      string
	FileSha1 string
	PreSha1  string
	SignKey  string
	SignVal  string
}

// UploadInitInfo 是上传初始化结果（传输层翻译为统一结构；业务层按 Status 分支）。
// Status：2 秒传命中（Fid 可能缺失需补查）、7 二次校验（按 SignCheck 区间重提）、1 走 OSS。
type UploadInitInfo struct {
	Status    int
	Fid       string
	PickCode  string
	SignKey   string
	SignCheck string
	Bucket    string
	Object    string
	Callback  OssCallback
}

// StructOrArray 兼容「对象字段可能返回对象 / 数组 / 布尔 / null」的多形态 JSON
// （借鉴 OpenList/115-sdk-go 的 json_types.StructOrArray）。
// 115（PHP 后端）在字段无值时常返回 []／false／null 而非 {}，直接按对象解析会
// 报 "cannot unmarshal array/bool into Go struct field"，导致整个响应解析失败。
// 非对象形态一律置零值放行，由业务层按需处理（如「空直链」分支）。
type StructOrArray[T any] struct {
	Value *T // 对象形态解析成功时指向它；非对象形态为 nil
}

// UnmarshalJSON 解析：仅接受以 '{' 开头的对象形态，其余（[]/false/null/空）置零放行。
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

// downItem 是下载直链响应 data 的单条条目（map key 即 fid）。
// 只保留消费的字段（file_name/url），其余附加字段无人读取，不解析。
type downItem struct {
	FileName string  `json:"file_name"`
	Url      downURL `json:"url"` // 实测 url 恒为对象；不可下载时 data 段为 []，不会解析到 url 字段
}

// downURL 是 downItem 内层的直链对象。
type downURL struct {
	Url string `json:"url"`
}

// OfflineAddResult 单条链接的添加结果。
type OfflineAddResult struct {
	State    bool   `json:"state"`     // 该链接是否添加成功
	Message  string `json:"message"`   // 状态描述
	InfoHash string `json:"info_hash"` // 任务 sha1，仅成功时返回
	Url      string `json:"url"`       // 原始链接
}

// OfflineTask 一条云下载任务。
// 只保留前端展示（offline.js：name/size/percentDone/status）与删除操作（info_hash）
// 所需的字段；115 返回的 url/add_time/last_update/file_id 等附加字段无人读取，不解析。
// size/status 实测为 JSON 数字（int），用 int64 直接解析。
type OfflineTask struct {
	InfoHash    string  `json:"info_hash"`   // 任务 sha1（删除任务用）
	Name        string  `json:"name"`        // 任务名
	Size        int64   `json:"size"`        // 总大小（字节）
	PercentDone float64 `json:"percentDone"` // 下载进度 0-100
	// Status 任务状态：-1 失败，0 分配中，1 下载中，2 成功
	Status int64 `json:"status"`
}

// OfflineTaskPage 任务列表分页结果。
type OfflineTaskPage struct {
	Page      int           `json:"page"`       // 当前页码
	PageCount int           `json:"page_count"` // 总页数
	Count     int           `json:"count"`      // 任务总数
	Tasks     []OfflineTask `json:"tasks"`      // 任务列表
}

// OfflineQuota 云下载配额信息（data 段：{count, surplus, used}，对齐 OpenList）。
type OfflineQuota struct {
	Count   int `json:"count"`   // 总配额
	Surplus int `json:"surplus"` // 剩余
	Used    int `json:"used"`    // 已用
}

// prettyJSON 把响应体转成可读文本：先按 any 解析（自动解码 \uXXXX 转义为中文明文），
// 再缩进重序列化。非 JSON 时原样返回（错误日志/调试用，绝不截断）。
func prettyJSON(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, err := json.Marshal(v, jsontext.WithIndent("  "))
	if err != nil {
		return string(b)
	}
	return string(out)
}

// ──── SHA1 工具（全部输出【大写】十六进制，115 服务端强制要求）────

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

// FileSHA1WithPreid 单次遍历文件，同时计算全量 SHA1 与前 128KB 的 SHA1（preid）。
func FileSHA1WithPreid(filePath string) (full, pre string, err error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", "", err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			logs.Debug(logs.ModuleCloud, "关闭文件失败", "错误", cerr)
		}
	}()

	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	buf := *bufPtr

	hFull := sha1.New()
	hPre := sha1.New()

	head := io.LimitReader(f, 128*1024)
	if _, err := io.CopyBuffer(io.MultiWriter(hFull, hPre), head, buf); err != nil {
		return "", "", err
	}
	pre = fmt.Sprintf("%X", hPre.Sum(nil))

	if _, err := io.CopyBuffer(hFull, f, buf); err != nil {
		return "", "", err
	}
	full = fmt.Sprintf("%X", hFull.Sum(nil))
	return full, pre, nil
}

// FileSHA1Partial 计算文件 [start, end] 闭区间字节的 SHA1（二次校验用）。
func FileSHA1Partial(filePath string, start, end int64) string {
	f, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			logs.Debug(logs.ModuleCloud, "关闭文件失败", "错误", cerr)
		}
	}()
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	readLength := end - start + 1
	h := sha1.New()
	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	buf := *bufPtr
	if _, err := io.CopyBuffer(h, io.LimitReader(f, readLength), buf); err != nil {
		return ""
	}
	return fmt.Sprintf("%X", h.Sum(nil))
}

// ──── 开放平台私有响应结构（云端原始返回的 JSON 承载）────

// fileListResponse 是 /open/ufile/files 响应 data 段的单条文件项。
// ⚠️ 该接口 count 在外壳平铺；分页终止用「返回条数不足一页」判定，不依赖 count。
type fileListResponse struct {
	Fid      string `json:"fid"`
	Name     string `json:"fn"`
	PickCode string `json:"pc"`
	Size     int64  `json:"fs"`
	IsVideo  int64  `json:"isv"` // 实测为数字（0 非视频 / 1 视频）
	Aid      string `json:"aid"`
	IsDir    string `json:"fc"`
}

// uploadInitResp 是 /open/upload/init 响应 data 段的字段（115 返回字段多，只取需要的几个）。
type uploadInitResp struct {
	Status    int                        `json:"status"`
	FileID    string                     `json:"file_id"`
	PickCode  string                     `json:"pick_code"`
	SignKey   string                     `json:"sign_key"`
	SignCheck string                     `json:"sign_check"`
	Bucket    string                     `json:"bucket"`
	Object    string                     `json:"object"`
	Callback  StructOrArray[OssCallback] `json:"callback"` // ⚠️ 秒传命中时返回 []，用 StructOrArray 容错
}
