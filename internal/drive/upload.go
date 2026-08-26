package drive

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// ──── 上传域：秒传 init → get_token → singleUpload（≤阈值 PUT）/ multipartUpload（分片） ────
// 参考 OpenList 115_open 的 Put 流程：UploadInit（秒传判断 + 签名校验）→ UploadGetToken
// （OSS 临时凭证）→ 按文件大小分流 OSS 单传或分片。

const ossMultipartThreshold int64 = 20 * 1024 * 1024 // 20MB：低于此值单次 PUT，超过则分片

// ossTarget 从 115 get_token / upload/init 数据中提取的 OSS 上传目标。
type ossTarget struct {
	client      *oss.Client
	bucket      string
	object      string
	cbBase64    string
	cbVarBase64 string
}

// OssTokenData 是 115 get_token 响应的 OSS 凭证字段。
type OssTokenData struct {
	AccessKeyID     string `json:"AccessKeyId"`
	AccessKeySecret string `json:"AccessKeySecret"`
	SecurityToken   string `json:"SecurityToken"`
	Endpoint        string `json:"endpoint"`
}

// OssCallback 是 115 上传目标中的 OSS 回调配置（上传完成回调 URL 与变量）。
type OssCallback struct {
	Callback    string `json:"callback"`
	CallbackVar string `json:"callback_var"`
}

// OssInitData 是 115 upload/init 响应 data 段的 OSS 上传目标字段。
type OssInitData struct {
	Bucket   string      `json:"bucket"`
	Object   string      `json:"object"`
	Callback OssCallback `json:"callback"`
}

// calPartSize 按文件大小动态计算分片大小（对齐 OpenList 的 calPartSize）。
// 阿里云 OSS 分片上限 10000 片，超大文件需加大分片避免超限；默认 20MB 起步。
func calPartSize(fileSize int64) int64 {
	const (
		mb = int64(1024 * 1024)
		gb = int64(1024 * 1024 * 1024)
		tb = int64(1024 * 1024 * 1024 * 1024)
	)
	partSize := 20 * mb
	if fileSize > 1*tb {
		partSize = 5 * gb
	} else if fileSize > 768*gb {
		partSize = 109951163 // ≈104.86MB（1TB 拆 1 万片）
	} else if fileSize > 512*gb {
		partSize = 82463373 // ≈78.64MB
	} else if fileSize > 384*gb {
		partSize = 54975582 // ≈52.43MB
	} else if fileSize > 256*gb {
		partSize = 41231687 // ≈39.32MB
	} else if fileSize > 128*gb {
		partSize = 27487791 // ≈26.21MB
	}
	return partSize
}

// singleUpload 小文件 OSS 单次 PUT（≤20MB），上传完成后 OSS 回调 115 记录文件信息。
func singleUpload(ctx context.Context, t *ossTarget, size int64, readerAt io.ReaderAt) (map[string]any, error) {
	result, err := t.client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket:      &t.bucket,
		Key:         &t.object,
		Body:        io.NewSectionReader(readerAt, 0, size),
		Callback:    &t.cbBase64,
		CallbackVar: &t.cbVarBase64,
	})
	if err != nil {
		return nil, err
	}
	return result.CallbackResult, nil
}

// multipartUpload 大文件分片上传（>20MB，分片大小按 calPartSize 动态计算）。
// 对齐 OpenList multpartUpload：每片上传失败重试 3 次（指数退避 1s 起）。
//
// ⚠️ 不要换成 SDK 自带的 oss.Uploader：其 UploadResult 不暴露 CallbackResult
// （uploader.go 两条路径都丢弃回调体），而回调体是 115 返回 data.file_id /
// data.pick_code 的唯一通道，拿不到 FID 上传链路就断了（2026-07 评估后保留手写）。
func multipartUpload(ctx context.Context, t *ossTarget, fileSize int64, readerAt io.ReaderAt) (map[string]any, error) {
	// Step 1: 初始化分片上传，加 sequential 参数使 OSS 返回不带 -N 后缀的 ETag
	initResult, err := t.client.InitiateMultipartUpload(ctx, &oss.InitiateMultipartUploadRequest{
		Bucket:     &t.bucket,
		Key:        &t.object,
		Parameters: map[string]string{"sequential": ""},
	})
	if err != nil {
		return nil, fmt.Errorf("初始化分片上传失败: %w", err)
	}
	uploadID := *initResult.UploadId

	// Step 2: 顺序上传每个分片，失败重试 3 次（指数退避）
	partSize := calPartSize(fileSize)
	totalParts := int((fileSize + partSize - 1) / partSize)
	parts := make([]oss.UploadPart, totalParts)

	offset := int64(0)
	for i := range totalParts {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		partNum := int32(i + 1)
		curSize := min(partSize, fileSize-offset)

		var etag string
		for attempt := 1; ; attempt++ {
			partResult, err := t.client.UploadPart(ctx, &oss.UploadPartRequest{
				Bucket:     &t.bucket,
				Key:        &t.object,
				UploadId:   &uploadID,
				PartNumber: partNum,
				Body:       io.NewSectionReader(readerAt, offset, curSize),
			})
			if err == nil {
				etag = *partResult.ETag
				break
			}
			if attempt >= 3 {
				return nil, fmt.Errorf("上传分片 %d/%d 失败: %w", partNum, totalParts, err)
			}
			select {
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			case <-time.After(time.Duration(1<<(attempt-1)) * time.Second):
			}
		}
		parts[i] = oss.UploadPart{PartNumber: partNum, ETag: &etag}
		offset += curSize
	}

	// Step 3: 完成分片上传，OSS 回调 115 通知上传完成
	completeResult, err := t.client.CompleteMultipartUpload(ctx, &oss.CompleteMultipartUploadRequest{
		Bucket:   &t.bucket,
		Key:      &t.object,
		UploadId: &uploadID,
		CompleteMultipartUpload: &oss.CompleteMultipartUpload{
			Parts: parts,
		},
		Callback:    &t.cbBase64,
		CallbackVar: &t.cbVarBase64,
	})
	if err != nil {
		return nil, fmt.Errorf("完成分片上传失败: %w", err)
	}
	return completeResult.CallbackResult, nil
}

// ossUpload 统一 OSS 真实内容上传（status 非 2/6/7/8 分支），供磁盘文件与内存字节共用。
// token/init 为已解析的 OSS 凭证与上传目标；readerAt 提供内容
// （*os.File 或 bytes.Reader 均实现 io.ReaderAt，单传/分片通用）。
func ossUpload(ctx context.Context, token OssTokenData, init OssInitData, size int64, readerAt io.ReaderAt) (map[string]any, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			token.AccessKeyID,
			token.AccessKeySecret,
			token.SecurityToken)).
		WithRegion("cn-shenzhen").
		WithEndpoint(token.Endpoint)
	t := &ossTarget{
		client:      oss.NewClient(cfg),
		bucket:      init.Bucket,
		object:      init.Object,
		cbBase64:    base64.StdEncoding.EncodeToString([]byte(init.Callback.Callback)),
		cbVarBase64: base64.StdEncoding.EncodeToString([]byte(init.Callback.CallbackVar)),
	}
	if size > ossMultipartThreshold {
		return multipartUpload(ctx, t, size, readerAt)
	}
	return singleUpload(ctx, t, size, readerAt)
}

// ──── 上传原子方法 ────

// initUpload 提交上传初始化（秒传签名表单）。req.SignKey/SignVal 非空时为二次校验重提。
// path 为日志定位用的完整路径（调用方传入），仅用于动作日志。
func (c *Client) initUpload(ctx context.Context, req UploadInitReq, path string) (*UploadInitInfo, error) {
	formData := Form{
		"file_name": req.FileName,
		"file_size": strconv.FormatInt(req.FileSize, 10),
		"target":    fmt.Sprintf("U_1_%s", req.Cid),
		"fileid":    req.FileSha1,
		"preid":     req.PreSha1,
		"topupload": "0",
	}
	if req.SignKey != "" && req.SignVal != "" {
		formData["sign_key"] = req.SignKey
		formData["sign_val"] = req.SignVal
	}
	trimmed, dur, err := Post[jsontext.Value](ctx, c, "/open/upload/init", formData)
	if err != nil {
		logCloud("上传初始化", err, dur, "路径", path)
		return nil, err
	}
	trimmed = bytes.TrimSpace(trimmed)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		logCloud("上传初始化", fmt.Errorf("data 字段缺失"), dur, "路径", path)
		return nil, fmt.Errorf("上传初始化失败: data 字段缺失, 响应体: %s", prettyJSON(trimmed))
	}
	var data uploadInitResp
	if err := json.Unmarshal(trimmed, &data, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
		logCloud("上传初始化", err, dur, "路径", path)
		return nil, fmt.Errorf("解析初始化响应失败: %w, 响应体: %s", err, prettyJSON(trimmed))
	}
	// 成功：补充云端返回的 status（2 秒传/1 OSS/7 二次校验）
	logCloud("上传初始化", nil, dur, "路径", path, "status", data.Status)
	var cb OssCallback
	if data.Callback.Value != nil {
		cb = *data.Callback.Value
	}
	return &UploadInitInfo{
		Status:    data.Status,
		Fid:       data.FileID,
		PickCode:  data.PickCode,
		SignKey:   data.SignKey,
		SignCheck: data.SignCheck,
		Bucket:    data.Bucket,
		Object:    data.Object,
		Callback:  cb,
	}, nil
}

// getUploadToken 获取 OSS 真实内容上传凭证（实际传输分支用）。
// path 为日志定位用的完整路径（调用方传入），仅用于动作日志。
func (c *Client) getUploadToken(ctx context.Context, path string) (OssTokenData, error) {
	res, dur, err := Get[OssTokenData](ctx, c, "/open/upload/get_token", nil)
	logCloud("获取上传凭证", err, dur, "路径", path)
	return res, err
}

// ──── 上传编排 ────

const maxUploadRetries = 3

// UploadHelper 上传本地文件到云端目录 cid（对齐 OpenList Put 流程）。
func UploadHelper(ctx context.Context, c *Client, pathStr, cid, signKey, signVal string) (info *UploadFileInfo, err error) {
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
		return nil, fmt.Errorf("获取文件信息失败: %w", err)
	}
	fileSize := fi.Size()
	fileSha1, preSha1, err := FileSHA1WithPreid(pathStr)
	if err != nil {
		return nil, fmt.Errorf("计算文件SHA1失败: %w", err)
	}

	for range maxUploadRetries {
		if err = context.Cause(ctx); err != nil {
			return nil, err
		}
		init, err := c.initUpload(ctx, UploadInitReq{
			FileName: filepath.Base(pathStr),
			FileSize: fileSize,
			Cid:      cid,
			FileSha1: fileSha1,
			PreSha1:  preSha1,
			SignKey:  signKey,
			SignVal:  signVal,
		}, pathStr)
		if err != nil {
			return nil, err
		}
		switch init.Status {
		case 2:
			// 秒传命中：init 正常直接返回 file_id，极少数缺失时用 pickcode 本地解码
			fid := init.Fid
			if fid == "" {
				fid, err = PickcodeToID(init.PickCode)
				if err != nil {
					return nil, err
				}
			}
			upType = "秒传"
			return &UploadFileInfo{Fid: fid, PickCode: init.PickCode}, nil
		case 6, 7, 8:
			// 双向校验：按 sign_check 区间计算 SHA1 作为 sign_val 重提 init
			start, end, perr := parseRange(init.SignCheck)
			if perr != nil {
				return nil, perr
			}
			signKey, signVal = init.SignKey, FileSHA1Partial(pathStr, start, end)
		default:
			var up string
			info, up, err = uploadByOSS(ctx, c, pathStr, fileSize, init)
			upType = up
			return info, err
		}
	}
	return nil, fmt.Errorf("秒传重试次数耗尽")
}

func uploadByOSS(ctx context.Context, c *Client, pathStr string, fileSize int64, init *UploadInitInfo) (*UploadFileInfo, string, error) {
	token, err := c.getUploadToken(ctx, pathStr)
	if err != nil {
		return nil, "", err
	}
	initData := OssInitData{Bucket: init.Bucket, Object: init.Object, Callback: init.Callback}
	f, err := os.Open(pathStr)
	if err != nil {
		return nil, "", fmt.Errorf("打开文件失败: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			logs.Debug(logs.ModuleCloud, "关闭上传文件失败", "错误", cerr)
		}
	}()
	// 上传前兜底：本地实际大小与上传参数 fileSize 不一致（文件在 init 后被重写/截断）→ 直接报错，不走上传。
	if fi, statErr := f.Stat(); statErr == nil && fi.Size() != fileSize {
		return nil, "", fmt.Errorf("上传前文件大小已变化: 期望=%d 实际=%d", fileSize, fi.Size())
	}
	cbResp, err := ossUpload(ctx, token, initData, fileSize, f)
	if err != nil {
		return nil, "", fmt.Errorf("OSS上传失败: %w", err)
	}
	raw, err := json.Marshal(cbResp)
	if err != nil {
		return nil, "", fmt.Errorf("OSS上传回调序列化失败: %w", err)
	}
	var cb struct {
		Data UploadCallbackData `json:"data"`
	}
	if err := json.Unmarshal(raw, &cb, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
		return nil, "", fmt.Errorf("OSS上传回调解析失败: %w, cbResp=%s", err, prettyJSON(raw))
	}
	fid, pc := cb.Data.FileID, cb.Data.PickCode
	if fid == "" || pc == "" {
		return nil, "", fmt.Errorf("OSS上传返回信息缺失: cbResp=%s", prettyJSON(raw))
	}
	upType := "OSS单文件上传"
	if fileSize > ossMultipartThreshold {
		upType = "OSS切片上传"
	}
	return &UploadFileInfo{Fid: fid, PickCode: pc}, upType, nil
}

// parseRange 解析 "start-end" 闭区间（秒传二次校验签名检查范围）。
func parseRange(s string) (start, end int64, err error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("签名检查格式错误: %s", s)
	}
	start, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	end, err = strconv.ParseInt(parts[1], 10, 64)
	return start, end, err
}
