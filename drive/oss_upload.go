package drive

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"

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

// newOSSTarget 构造 OSS 客户端与上传目标参数（bucket/object/回调）。
func newOSSTarget(token, init map[string]any) *ossTarget {
	ak, _ := token["AccessKeyId"].(string)
	sk, _ := token["AccessKeySecret"].(string)
	st, _ := token["SecurityToken"].(string)
	endpoint, _ := token["endpoint"].(string)

	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, st)).
		WithRegion("cn-shenzhen").
		WithEndpoint(endpoint)

	cb, _ := init["callback"].(map[string]any)
	cbStr, _ := cb["callback"].(string)
	cbVarStr, _ := cb["callback_var"].(string)
	bucket, _ := init["bucket"].(string)
	object, _ := init["object"].(string)

	return &ossTarget{
		client:      oss.NewClient(cfg),
		bucket:      bucket,
		object:      object,
		cbBase64:    base64.StdEncoding.EncodeToString([]byte(cbStr)),
		cbVarBase64: base64.StdEncoding.EncodeToString([]byte(cbVarStr)),
	}
}

func (d *Open115) ossUploadFile(ctx context.Context, token, init map[string]any, filePath string, fileSize int64) (map[string]any, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	t := newOSSTarget(token, init)

	// 小文件：单次 PUT 上传（带 OSS 回调）
	if fileSize <= ossMultipartThreshold {
		result, err := t.client.PutObjectFromFile(ctx, &oss.PutObjectRequest{
			Bucket:      &t.bucket,
			Key:         &t.object,
			Callback:    &t.cbBase64,
			CallbackVar: &t.cbVarBase64,
		}, filePath)
		if err != nil {
			return nil, err
		}
		return result.CallbackResult, nil
	}

	// 大文件：分片上传（对齐 OpenList multpartUpload 实现）
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

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	offset := int64(0)
	for i := range totalParts {
		partNum := int32(i + 1)
		partSize := min(ossPartSize, fileSize-offset)

		partResult, err := t.client.UploadPart(ctx, &oss.UploadPartRequest{
			Bucket:     &t.bucket,
			Key:        &t.object,
			UploadId:   &uploadID,
			PartNumber: partNum,
			Body:       io.NewSectionReader(f, offset, partSize),
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

	return completeResult.CallbackResult, nil
}

// ossUploadBytes 从内存上传小文件（种子等，调用方已限制 ≤ 10MB，无需分片）。
func (d *Open115) ossUploadBytes(ctx context.Context, token, init map[string]any, data []byte) (map[string]any, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	if int64(len(data)) > ossMultipartThreshold {
		return nil, fmt.Errorf("内存上传仅支持 %dMB 以内的文件", ossMultipartThreshold>>20)
	}
	t := newOSSTarget(token, init)
	result, err := t.client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket:      &t.bucket,
		Key:         &t.object,
		Body:        bytes.NewReader(data),
		Callback:    &t.cbBase64,
		CallbackVar: &t.cbVarBase64,
	})
	if err != nil {
		return nil, err
	}
	return result.CallbackResult, nil
}
