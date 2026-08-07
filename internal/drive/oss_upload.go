package drive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

const (
	ossMultipartThreshold int64 = 20 * 1024 * 1024 // 20MB：低于此值单次 PUT，超过则分片（对齐 OpenList）
	ossPartSize           int64 = 20 * 1024 * 1024 // 20MB 每分片（对齐 OpenList 默认值）
)

// ossTarget 从 115 get_token / upload/init 响应中提取的 OSS 上传目标。
type ossTarget struct {
	client      *oss.Client
	bucket      string
	object      string
	cbBase64    string
	cbVarBase64 string
}

// ossTokenData 是 /open/upload/get_token 响应 data 段的 OSS 凭证字段。
type ossTokenData struct {
	AccessKeyID     string `json:"AccessKeyId"`
	AccessKeySecret string `json:"AccessKeySecret"`
	SecurityToken   string `json:"SecurityToken"`
	Endpoint        string `json:"endpoint"`
}

// ossInitData 是 /open/upload/init 响应 data 段的 OSS 上传目标字段。
type ossInitData struct {
	Bucket   string `json:"bucket"`
	Object   string `json:"object"`
	Callback struct {
		Callback    string `json:"callback"`
		CallbackVar string `json:"callback_var"`
	} `json:"callback"`
}

// newOSSTarget 从 115 get_token / upload/init 的原始响应体提取凭证与上传目标
// （bucket/object/回调），构造 OSS 客户端。字段缺失/解析失败按空值处理：
// 空值直接流入 OSS 调用，由 SDK 报错兜底。
func newOSSTarget(tokenBody, initBody []byte) *ossTarget {
	var token struct {
		Data ossTokenData `json:"data"`
	}
	_ = json.Unmarshal(tokenBody, &token)
	var init struct {
		Data ossInitData `json:"data"`
	}
	_ = json.Unmarshal(initBody, &init)

	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			token.Data.AccessKeyID,
			token.Data.AccessKeySecret,
			token.Data.SecurityToken)).
		WithRegion("cn-shenzhen").
		WithEndpoint(token.Data.Endpoint)

	return &ossTarget{
		client:      oss.NewClient(cfg),
		bucket:      init.Data.Bucket,
		object:      init.Data.Object,
		cbBase64:    base64.StdEncoding.EncodeToString([]byte(init.Data.Callback.Callback)),
		cbVarBase64: base64.StdEncoding.EncodeToString([]byte(init.Data.Callback.CallbackVar)),
	}
}

// ossUpload 统一 OSS 真实内容上传（status=1 分支），供磁盘文件与内存字节共用。
// tokenBody/initBody 为 115 响应原始体；readerAt 提供内容（*os.File 或 bytes.Reader
// 均实现 io.ReaderAt，单传/分片通用）；size 为内容总字节数。小于等于阈值走单次 PUT，
// 超过则走分片上传。返回上传类型供 UploadFile 结束统一打印（上传方式由调用方收敛）。
func (d *Open115) ossUpload(ctx context.Context, tokenBody, initBody []byte, size int64, readerAt io.ReaderAt) (map[string]any, string, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, "", err
	}
	if size > ossMultipartThreshold {
		res, err := d.ossUploadMultipart(ctx, tokenBody, initBody, size, readerAt)
		return res, "OSS切片上传", err
	}
	t := newOSSTarget(tokenBody, initBody)
	result, err := t.client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket:      &t.bucket,
		Key:         &t.object,
		Body:        io.NewSectionReader(readerAt, 0, size),
		Callback:    &t.cbBase64,
		CallbackVar: &t.cbVarBase64,
	})
	if err != nil {
		return nil, "", err
	}
	return result.CallbackResult, "OSS单文件上传", nil
}

// ossUploadMultipart 分片上传大文件（对齐 OpenList multpartUpload 实现）。
// 仅 ossUpload 在 size 超过阈值时调用；readerAt 须支持按偏移随机读取（io.ReaderAt）。
//
// ⚠️ 不要换成 SDK 自带的 oss.Uploader：其 UploadResult 不暴露 CallbackResult
// （uploader.go 两条路径都丢弃回调体），而回调体是 115 返回 data.file_id /
// data.pick_code 的唯一通道，拿不到 FID 上传链路就断了（2026-07 评估后保留手写）。
func (d *Open115) ossUploadMultipart(ctx context.Context, tokenBody, initBody []byte, fileSize int64, readerAt io.ReaderAt) (map[string]any, error) {
	t := newOSSTarget(tokenBody, initBody)

	// Step 1: 初始化分片上传，加 sequential 参数使 OSS 返回不带 -N 后缀的 ETag
	initResult, err := t.client.InitiateMultipartUpload(ctx, &oss.InitiateMultipartUploadRequest{
		Bucket: &t.bucket,
		Key:    &t.object,
		RequestCommon: oss.RequestCommon{
			Parameters: map[string]string{"sequential": ""},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("初始化分片上传失败: %w", err)
	}
	uploadID := *initResult.UploadId

	// Step 2: 顺序上传每个分片（OpenList 做法：逐片上传 + 重试）
	totalParts := int((fileSize + ossPartSize - 1) / ossPartSize)
	parts := make([]oss.UploadPart, totalParts)

	offset := int64(0)
	for i := range totalParts {
		partNum := int32(i + 1)
		partSize := min(ossPartSize, fileSize-offset)

		partResult, err := t.client.UploadPart(ctx, &oss.UploadPartRequest{
			Bucket:     &t.bucket,
			Key:        &t.object,
			UploadId:   &uploadID,
			PartNumber: partNum,
			Body:       io.NewSectionReader(readerAt, offset, partSize),
		})
		if err != nil {
			return nil, fmt.Errorf("上传分片 %d/%d 失败: %w", partNum, totalParts, err)
		}

		parts[i] = oss.UploadPart{
			PartNumber: partNum,
			ETag:       partResult.ETag,
		}
		offset += partSize
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

	// 上传类型与耗时已由 UploadFile 结束统一打印，这里不再重复打
	return completeResult.CallbackResult, nil
}
