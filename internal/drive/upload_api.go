package drive

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// ErrUploadSizeChanged 表示上传前复核发现文件大小相对 init 阶段已变化（被外部重写/截断）。
// 属可自愈的瞬时状态，调用方应降级为警告并交由后续扫描重传，不应视为致命错误。
var ErrUploadSizeChanged = errors.New("上传前文件大小已变化")

// uploadInitData 是 /open/upload/init 响应 data 段的字段（115 返回字段多，只取需要的几个）。
type uploadInitData struct {
	Status    int    `json:"status"`
	FileID    string `json:"file_id"`
	PickCode  string `json:"pick_code"`
	SignKey   string `json:"sign_key"`
	SignCheck string `json:"sign_check"`
}

// parseUploadInit 解析 /open/upload/init 响应体中的 data 对象（缺失/非对象报错）。
func parseUploadInit(body []byte) (uploadInitData, error) {
	var res struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return uploadInitData{}, fmt.Errorf("解析初始化响应失败: %w, 响应体: %s", err, truncateBody(body))
	}
	trimmed := bytes.TrimSpace(res.Data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return uploadInitData{}, fmt.Errorf("解析初始化响应失败: data 字段缺失, 响应体: %s", truncateBody(body))
	}
	var data uploadInitData
	if err := json.Unmarshal(trimmed, &data); err != nil {
		return uploadInitData{}, fmt.Errorf("解析初始化响应失败: %w, 响应体: %s", err, truncateBody(body))
	}
	return data, nil
}

// 本文件是 115 上传 API：普通文件上传（UploadFile）。
//
// 【上传流程（秒传优先）】
//  1. 计算文件全量 SHA1（fileid）与前 128KB SHA1（preid），提交 /open/upload/init；
//  2. 服务端返回 status=2 → 秒传成功（云端已有同内容文件，零流量完成）；
//  3. status=7 → 需要二次校验：按 sign_check 指定的字节区间再算一次 SHA1 重提；
//  4. status=1 → 云端没有，走 OSS 分片上传真实文件内容（见 oss_upload.go）。
//
// 【重要约定】所有 SHA1 一律使用【大写】十六进制（%X）：115 服务端以大写存储与校验。

// UploadFileInfo 上传成功结果：云端 FID 与 pickcode。
type UploadFileInfo struct {
	Fid      string
	PickCode string
}

// UploadFile 上传本地文件到云端目录 cid。
// signKey/signVal 用于二次校验重提（status=7 场景），首次调用留空。
// 成功时统一在函数结束打印一条 Info：文件 + 上传类型（秒传/OSS单文件/OSS切片）+ 耗时，
// 上传类型由子函数（UploadFileWithSign/uploadReal/ossUpload）逐层回传。
func (d *Open115) UploadFile(ctx context.Context, pathStr, cid, signKey, signVal string) (info *UploadFileInfo, err error) {
	t0 := time.Now()
	upType := ""
	defer func() {
		if err == nil && info != nil {
			logs.Info(logs.ModuleCloud, "上传文件", "路径", pathStr, "上传类型", upType, "耗时", time.Since(t0))
		}
	}()
	if err = context.Cause(ctx); err != nil {
		return nil, err
	}
	fi, err := os.Stat(pathStr)
	if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %v", err)
	}
	fileSize := fi.Size()
	fileSha1, preSha1, err := fileSHA1WithPreid(pathStr)
	if err != nil {
		return nil, fmt.Errorf("计算文件SHA1失败: %w", err)
	}

	formData := map[string]string{
		"file_name": fi.Name(),
		"file_size": strconv.FormatInt(fileSize, 10),
		"target":    fmt.Sprintf("U_1_%s", cid),
		"fileid":    fileSha1,
		"preid":     preSha1,
		"topupload": "0",
	}
	if signKey != "" && signVal != "" {
		formData["sign_key"] = signKey
		formData["sign_val"] = signVal
	}
	body, err := doRawAPI(ctx, d, "POST", "/open/upload/init", withForm(formData))
	if err != nil {
		return nil, err
	}
	data, err := parseUploadInit(body)
	if err != nil {
		return nil, err
	}

	switch data.Status {
	case 2:
		upType = "秒传"
		return &UploadFileInfo{
			Fid:      data.FileID,
			PickCode: data.PickCode,
		}, nil

	case 7:
		// 二次校验：按指定字节区间重新计算 SHA1 后重提。
		parts := strings.Split(data.SignCheck, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("签名检查格式错误: %s", data.SignCheck)
		}
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end, _ := strconv.ParseInt(parts[1], 10, 64)
		info, upType, err = d.UploadFileWithSign(ctx, pathStr, cid, data.SignKey,
			fileSHA1Partial(pathStr, start, end))
		return info, err

	default:
		info, upType, err = d.uploadReal(ctx, pathStr, fileSize, body)
		return info, err
	}
}

// UploadFileWithSign 走二次校验重提：复用 doAPI 但把 sign 直接带入 init 表单。
// 返回上传类型（秒传 / OSS 单文件 / OSS 切片）供 UploadFile 结束统一打印。
func (d *Open115) UploadFileWithSign(ctx context.Context, pathStr, cid, signKey, signVal string) (*UploadFileInfo, string, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, "", err
	}
	fi, err := os.Stat(pathStr)
	if err != nil {
		return nil, "", fmt.Errorf("获取文件信息失败: %v", err)
	}
	fileSha1, preSha1, shaErr := fileSHA1WithPreid(pathStr)
	if shaErr != nil {
		return nil, "", fmt.Errorf("计算文件SHA1失败: %w", shaErr)
	}
	formData := map[string]string{
		"file_name": fi.Name(),
		"file_size": strconv.FormatInt(fi.Size(), 10),
		"target":    fmt.Sprintf("U_1_%s", cid),
		"fileid":    fileSha1,
		"preid":     preSha1,
		"topupload": "0",
		"sign_key":  signKey,
		"sign_val":  signVal,
	}
	body, err := doRawAPI(ctx, d, "POST", "/open/upload/init", withForm(formData))
	if err != nil {
		return nil, "", err
	}
	data, err := parseUploadInit(body)
	if err != nil {
		return nil, "", err
	}
	if data.Status == 2 {
		return &UploadFileInfo{
			Fid:      data.FileID,
			PickCode: data.PickCode,
		}, "秒传", nil
	}
	return d.uploadReal(ctx, pathStr, fi.Size(), body)
}

// uploadReal 处理 status=1（云端无此文件）：取 OSS 凭证走真实内容上传。
// initBody 为 upload/init 的原始响应体，OSS 目标参数由 newOSSTarget 结构体解析。
// 返回上传类型（OSS单文件上传 / OSS切片上传）供 UploadFile 结束统一打印。
func (d *Open115) uploadReal(ctx context.Context, pathStr string, size int64, initBody []byte) (*UploadFileInfo, string, error) {
	tokenBody, err := doRawAPI(ctx, d, "GET", "/open/upload/get_token")
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(pathStr)
	if err != nil {
		return nil, "", fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()
	// init 阶段 stat 与此处 open 之间可能受外部重写/截断影响，复核大小一致再上传，
	// 避免 Content-Length 与实际 body 不符触发 "ContentLength=... with Body length ..."
	// 传输错误，也避免上传变长后的残缺内容。不一致则放弃本次，由后续扫描重新 init 重传。
	if fi, statErr := f.Stat(); statErr == nil && fi.Size() != size {
		return nil, "", fmt.Errorf("%w: 期望=%d 实际=%d", ErrUploadSizeChanged, size, fi.Size())
	}
	cbResp, upType, err := d.ossUpload(ctx, tokenBody, initBody, size, f)
	if err != nil {
		return nil, "", fmt.Errorf("OSS上传失败: %w", err)
	}
	raw, _ := json.Marshal(cbResp)
	var cb struct {
		Data struct {
			FileID   string `json:"file_id"`
			PickCode string `json:"pick_code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &cb); err != nil {
		return nil, "", fmt.Errorf("OSS上传回调解析失败: %w, cbResp=%s", err, truncateBody(raw))
	}
	fid, pc := cb.Data.FileID, cb.Data.PickCode
	if fid == "" || pc == "" {
		return nil, "", fmt.Errorf("OSS上传返回信息缺失: cbResp=%s", truncateBody(raw))
	}
	return &UploadFileInfo{Fid: fid, PickCode: pc}, upType, nil
}

// UploadBytes 上传内存字节数据（如 ≤10MB 的种子文件）到云端目录 cid（"0" 为根）。
// 实现为写临时文件后复用 UploadFile 流程，避免为内存上传另起一套 SHA1/OSS 逻辑。
func (d *Open115) UploadBytes(ctx context.Context, name string, data []byte, cid, signKey, signVal string) (*UploadFileInfo, error) {
	// 上传完成日志由 UploadFile 统一打印，入口不重复打
	tmp, err := os.CreateTemp("", "115up-*"+name)
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("关闭临时文件失败: %w", err)
	}
	return d.UploadFile(ctx, tmpPath, cid, signKey, signVal)
}

// ──── SHA1 工具（全部输出【大写】十六进制，115 服务端强制要求）────

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

// fileSHA1WithPreid 单次遍历文件，同时计算全量 SHA1 与前 128KB 的 SHA1（preid）。
func fileSHA1WithPreid(filePath string) (full, pre string, err error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

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

// fileSHA1Partial 计算文件 [start, end] 闭区间字节的 SHA1（二次校验用）。
func fileSHA1Partial(filePath string, start, end int64) string {
	f, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer f.Close()
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
