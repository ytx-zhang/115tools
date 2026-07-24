package drive

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// 本文件是 115 上传 API：普通文件上传（UploadFile）与内存字节上传（UploadBytes）。
//
// 【上传流程（秒传优先）】
//  1. 计算文件全量 SHA1（fileid）与前 128 字节 SHA1（preid），提交 /open/upload/init；
//  2. 服务端返回 status=2 → 秒传成功（云端已有同内容文件，零流量完成）；
//  3. status=7 → 需要二次校验：按 sign_check 指定的字节区间再算一次 SHA1 重提；
//  4. status=1 → 云端没有，走 OSS 分片上传真实文件内容（见 oss_upload.go）。
//
// 【重要约定】所有 SHA1 一律使用【大写】十六进制（%X）：
// 115 服务端以大写存储与校验，小写会导致「文件不存在或已删除」类错误。

// UploadFileInfo 上传成功结果：云端 FID 与 pickcode。
type UploadFileInfo struct {
	Fid      string
	PickCode string
}

// UploadFile 上传本地文件到云端目录 cid。
// signKey/signVal 用于二次校验重提（status=7 场景），首次调用留空。
func (d *Open115) UploadFile(ctx context.Context, pathStr, cid, signKey, signVal string) (*UploadFileInfo, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	fileInfo, err := os.Stat(pathStr)
	if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %v", err)
	}
	fileName := fileInfo.Name()
	fileSize := fileInfo.Size()
	// 单次读取同时计算全量 SHA1 与前 128 字节 SHA1（preid），减少一次文件遍历
	fileSha1, preSha1, _ := fileSHA1WithPreid(pathStr)
	req := d.Client.R().SetContext(ctx)
	formData := map[string]string{
		"file_name": fileName,
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
	req.SetFormData(formData)

	resp, err := req.Post("/open/upload/init")
	if err != nil {
		return nil, err
	}
	var uploadResp map[string]any
	if err := json.Unmarshal(resp.Body(), &uploadResp); err != nil {
		return nil, fmt.Errorf("解析初始化响应失败: %w", err)
	}
	initData, ok := uploadResp["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("解析初始化响应失败: data 字段缺失")
	}
	status, _ := initData["status"].(float64)

	switch int64(status) {
	case 2:
		// 秒传成功
		fid, _ := initData["file_id"].(string)
		pc, _ := initData["pick_code"].(string)
		return &UploadFileInfo{Fid: fid, PickCode: pc}, nil

	case 7:
		// 二次校验：按指定字节区间重新计算 SHA1 后重提
		signCheck, _ := initData["sign_check"].(string)
		parts := strings.Split(signCheck, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("签名检查格式错误: %s", signCheck)
		}
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end, _ := strconv.ParseInt(parts[1], 10, 64)
		newSignKey, _ := initData["sign_key"].(string)
		newSignVal, _ := fileSHA1Partial(pathStr, start, end)
		return d.UploadFile(ctx, pathStr, cid, newSignKey, newSignVal)

	case 1:
		// 云端无此文件：取 OSS 上传凭证，走分片上传
		tokenReq := d.Client.R().SetContext(ctx)
		tokenResp, err := tokenReq.Get("/open/upload/get_token")
		if err != nil {
			return nil, err
		}
		var tokenRoot map[string]any
		if err := json.Unmarshal(tokenResp.Body(), &tokenRoot); err != nil {
			return nil, fmt.Errorf("解析上传Token响应失败: %w", err)
		}
		tokenData, ok := tokenRoot["data"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("解析上传Token响应失败: data 字段缺失")
		}
		cbResp, err := d.ossUploadFile(ctx, tokenData, initData, pathStr, fileSize)
		if err != nil {
			return nil, fmt.Errorf("OSS上传失败: %w", err)
		}
		fid := getMapString(cbResp, "data", "file_id")
		pc := getMapString(cbResp, "data", "pick_code")
		if fid == "" || pc == "" {
			return nil, fmt.Errorf("OSS上传返回信息缺失")
		}
		return &UploadFileInfo{Fid: fid, PickCode: pc}, nil

	default:
		return nil, fmt.Errorf("未知上传状态码: %.0f", status)
	}
}

// UploadBytes 上传内存中的字节数据（如种子文件）到云端目录 cid（"0" 为根目录）。
// signKey/signVal 含义同 UploadFile，首次调用留空。
func (d *Open115) UploadBytes(ctx context.Context, name string, data []byte, cid, signKey, signVal string) (*UploadFileInfo, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	dataSize := int64(len(data))
	fileSha1, preSha1 := bytesSHA1WithPreid(data)

	formData := map[string]string{
		"file_name": name,
		"file_size": strconv.FormatInt(dataSize, 10),
		"target":    fmt.Sprintf("U_1_%s", cid),
		"fileid":    fileSha1,
		"preid":     preSha1,
		"topupload": "0",
	}
	if signKey != "" && signVal != "" {
		formData["sign_key"] = signKey
		formData["sign_val"] = signVal
	}

	resp, err := d.Client.R().SetContext(ctx).
		SetFormData(formData).
		Post("/open/upload/init")
	if err != nil {
		return nil, err
	}
	var uploadResp map[string]any
	if err := json.Unmarshal(resp.Body(), &uploadResp); err != nil {
		return nil, fmt.Errorf("解析上传初始化响应失败: %w", err)
	}
	initData, ok := uploadResp["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("解析上传初始化响应失败: data 字段缺失")
	}
	status, _ := initData["status"].(float64)

	switch int64(status) {
	case 2:
		return &UploadFileInfo{
			Fid:      getMapString(initData, "file_id"),
			PickCode: getMapString(initData, "pick_code"),
		}, nil

	case 7:
		signCheck, _ := initData["sign_check"].(string)
		parts := strings.Split(signCheck, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("签名检查格式错误: %s", signCheck)
		}
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end, _ := strconv.ParseInt(parts[1], 10, 64)
		newSignKey := getMapString(initData, "sign_key")
		newSignVal := bytesSHA1Partial(data, start, end)
		return d.UploadBytes(ctx, name, data, cid, newSignKey, newSignVal)

	case 1:
		tokenResp, err := d.Client.R().SetContext(ctx).
			Get("/open/upload/get_token")
		if err != nil {
			return nil, err
		}
		var tokenRoot map[string]any
		if err := json.Unmarshal(tokenResp.Body(), &tokenRoot); err != nil {
			return nil, fmt.Errorf("解析上传Token响应失败: %w", err)
		}
		tokenData, ok := tokenRoot["data"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("解析上传Token响应失败: data 字段缺失")
		}
		cbResp, err := d.ossUploadBytes(ctx, tokenData, initData, data)
		if err != nil {
			return nil, fmt.Errorf("OSS上传失败: %w", err)
		}
		return &UploadFileInfo{
			Fid:      getMapString(cbResp, "data", "file_id"),
			PickCode: getMapString(cbResp, "data", "pick_code"),
		}, nil

	default:
		return nil, fmt.Errorf("未知上传状态码: %.0f", status)
	}
}

// ──── SHA1 工具（全部输出【大写】十六进制，115 服务端强制要求）────

// bytesSHA1WithPreid 一次计算内存字节的全量 SHA1 与前 128KB SHA1（preid）。
func bytesSHA1WithPreid(data []byte) (sha1Hex, preHex string) {
	h := sha1.New()
	h.Write(data)
	sha1Hex = fmt.Sprintf("%X", h.Sum(nil))

	preLen := min(len(data), 128*1024)
	ph := sha1.New()
	ph.Write(data[:preLen])
	preHex = fmt.Sprintf("%X", ph.Sum(nil))
	return
}

// bytesSHA1Partial 计算内存字节 [start:end] 区间的 SHA1（二次校验用）。
func bytesSHA1Partial(data []byte, start, end int64) string {
	if start < 0 {
		start = 0
	}
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	if start >= end {
		start, end = 0, int64(len(data))
	}
	h := sha1.New()
	h.Write(data[start:end])
	return fmt.Sprintf("%X", h.Sum(nil))
}

// getMapString 从嵌套 map 中按路径逐级取字符串值；任一层缺失返回空串。
// 用于解析上传/OSS 接口返回的非固定结构 JSON。
func getMapString(m map[string]any, keys ...string) string {
	var current any = m
	for _, key := range keys {
		if next, ok := current.(map[string]any); ok {
			current = next[key]
		} else {
			return ""
		}
	}
	s, _ := current.(string)
	return s
}

// bufPool 复用 32KB 读缓冲，避免每次算 SHA1 都分配新切片。
var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

// fileSHA1WithPreid 单次遍历文件，同时计算全量 SHA1 与前 128 字节（0-127）的 SHA1。
// 供上传初始化使用，避免 fileSHA1 + fileSHA1Partial 两次打开/遍历文件。
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

	// 前 128 字节同时写入两个哈希
	head := io.LimitReader(f, 128)
	if _, err := io.CopyBuffer(io.MultiWriter(hFull, hPre), head, buf); err != nil {
		return "", "", err
	}
	pre = fmt.Sprintf("%X", hPre.Sum(nil))

	// 剩余部分仅写入全量哈希
	if _, err := io.CopyBuffer(hFull, f, buf); err != nil {
		return "", "", err
	}
	full = fmt.Sprintf("%X", hFull.Sum(nil))
	return full, pre, nil
}

// fileSHA1Partial 计算文件 [start, end] 闭区间字节的 SHA1（二次校验用）。
func fileSHA1Partial(filePath string, start, end int64) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	readLength := end - start + 1
	h := sha1.New()
	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	buf := *bufPtr
	lr := io.LimitReader(f, readLength)
	if _, err := io.CopyBuffer(h, lr, buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%X", h.Sum(nil)), nil
}
