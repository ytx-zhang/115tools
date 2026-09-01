package drive

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

const ossMultipartThreshold int64 = 20 * 1024 * 1024 // 20MB：低于此值单次 PUT，超过则分片

// OssCallback 115 上传目标中的 OSS 回调配置。
type OssCallback struct {
	Callback    string `json:"callback"`
	CallbackVar string `json:"callback_var"`
}

// OssTokenData 是 115 get_token 响应的 OSS 凭证字段。
type OssTokenData struct {
	AccessKeyID     string `json:"AccessKeyId"`
	AccessKeySecret string `json:"AccessKeySecret"`
	SecurityToken   string `json:"SecurityToken"`
	Endpoint        string `json:"endpoint"`
}

// OssInitData 是 upload/init 响应 data 段的 OSS 上传目标字段。
type OssInitData struct {
	Bucket   string      `json:"bucket"`
	Object   string      `json:"object"`
	Callback OssCallback `json:"callback"`
}

// UploadInitInfo 上传初始化结果。Status：2 秒传命中、7 二次校验、1 走 OSS。
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

// ossTarget 从 115 返回数据中提取的 OSS 上传目标。
type ossTarget struct {
	client      *oss.Client
	bucket      string
	object      string
	cbBase64    string
	cbVarBase64 string
}

// calcPartSize 按文件大小动态计算分片大小（OSS 分片上限 10000）。
func calcPartSize(fileSize int64) int64 {
	const (
		mb = int64(1024 * 1024)
		gb = int64(1024 * 1024 * 1024)
		tb = int64(1024 * 1024 * 1024 * 1024)
	)
	partSize := 20 * mb
	switch {
	case fileSize > 1*tb:
		partSize = 5 * gb
	case fileSize > 768*gb:
		partSize = 109951163
	case fileSize > 512*gb:
		partSize = 82463373
	case fileSize > 384*gb:
		partSize = 54975582
	case fileSize > 256*gb:
		partSize = 41231687
	case fileSize > 128*gb:
		partSize = 27487791
	}
	return partSize
}

// singleUpload 小文件 OSS 单次 PUT（≤20MB）。
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

// multipartUpload 大文件分片上传（>20MB），每片失败重试 3 次（指数退避）。
// 手写分片而非 oss.Uploader：其 UploadResult 不暴露 CallbackResult（115 返回 file_id/pick_code 的唯一通道）。
func multipartUpload(ctx context.Context, t *ossTarget, fileSize int64, readerAt io.ReaderAt) (map[string]any, error) {
	initResult, err := t.client.InitiateMultipartUpload(ctx, &oss.InitiateMultipartUploadRequest{
		Bucket: &t.bucket,
		Key:    &t.object,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化分片上传失败: %w", err)
	}
	uploadID := *initResult.UploadId

	partSize := calcPartSize(fileSize)
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

// ossUpload 统一 OSS 真实内容上传（status 非 2/6/7/8 分支）。
func ossUpload(ctx context.Context, token OssTokenData, init OssInitData, size int64, readerAt io.ReaderAt) (map[string]any, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			token.AccessKeyID, token.AccessKeySecret, token.SecurityToken)).
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

// initUpload 提交上传初始化（秒传签名表单）。
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
	res, dur, err := Post[StructOrArray[uploadInitResp]](ctx, c, "/open/upload/init", formData)
	if err != nil {
		logCall(ctx, "上传初始化", err, dur, "路径", path)
		return nil, err
	}
	if res.Value == nil {
		logCall(ctx, "上传初始化", fmt.Errorf("data 字段缺失"), dur, "路径", path)
		return nil, fmt.Errorf("上传初始化失败: data 字段缺失")
	}
	data := *res.Value
	logCall(ctx, "上传初始化", nil, dur, "路径", path, "status", data.Status)
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

// getUploadToken 获取 OSS 上传凭证。
func (c *Client) getUploadToken(ctx context.Context, path string) (OssTokenData, error) {
	res, dur, err := Get[OssTokenData](ctx, c, "/open/upload/get_token", nil)
	logCall(ctx, "获取上传凭证", err, dur, "路径", path)
	return res, err
}

// UploadInitReq 上传初始化请求。
type UploadInitReq struct {
	FileName string
	FileSize int64
	Cid      string
	FileSha1 string
	PreSha1  string
	SignKey  string
	SignVal  string
}

// uploadCallbackData 是 OSS 回调返回体 data 段。
type uploadCallbackData struct {
	FileID   string `json:"file_id"`
	PickCode string `json:"pick_code"`
}

const maxUploadRetries = 3

// UploadHelper 上传本地文件到云端目录 cid（秒传 → 二次校验 → OSS）。
func UploadHelper(ctx context.Context, c *Client, path, cid, signKey, signVal string) (info *UploadFileInfo, err error) {
	t0 := time.Now()
	upType := ""
	defer func() {
		if err == nil && info != nil {
			slog.InfoContext(ctx, "上传文件", "路径", path, "上传类型", upType, "耗时", time.Since(t0))
		}
	}()
	if err = context.Cause(ctx); err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %w", err)
	}
	fileSize := fi.Size()
	fileSha1, preSha1, err := FileSHA1WithPreid(path)
	if err != nil {
		return nil, fmt.Errorf("计算文件SHA1失败: %w", err)
	}

	for range maxUploadRetries {
		if err = context.Cause(ctx); err != nil {
			return nil, err
		}
		init, err := c.initUpload(ctx, UploadInitReq{
			FileName: filepath.Base(path),
			FileSize: fileSize,
			Cid:      cid,
			FileSha1: fileSha1,
			PreSha1:  preSha1,
			SignKey:  signKey,
			SignVal:  signVal,
		}, path)
		if err != nil {
			return nil, err
		}
		switch init.Status {
		case 2:
			fid := init.Fid
			if fid == "" {
				if fid, err = PickcodeToID(init.PickCode); err != nil {
					return nil, err
				}
			}
			upType = "秒传"
			return &UploadFileInfo{Fid: fid, PickCode: init.PickCode}, nil
		case 6, 7, 8:
			start, end, perr := parseRange(init.SignCheck)
			if perr != nil {
				return nil, perr
			}
			signKey, signVal = init.SignKey, FileSHA1Partial(path, start, end)
		default:
			var up string
			info, up, err = uploadByOSS(ctx, c, path, fileSize, init)
			upType = up
			return info, err
		}
	}
	return nil, fmt.Errorf("秒传重试次数耗尽")
}

func uploadByOSS(ctx context.Context, c *Client, path string, fileSize int64, init *UploadInitInfo) (*UploadFileInfo, string, error) {
	token, err := c.getUploadToken(ctx, path)
	if err != nil {
		return nil, "", err
	}
	initData := OssInitData{Bucket: init.Bucket, Object: init.Object, Callback: init.Callback}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("打开文件失败: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			slog.DebugContext(ctx, "关闭上传文件失败", "错误", cerr)
		}
	}()
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
		Data uploadCallbackData `json:"data"`
	}
	if err := json.Unmarshal(raw, &cb); err != nil {
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

// parseRange 解析 "start-end" 闭区间（秒传二次校验签名范围）。
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
