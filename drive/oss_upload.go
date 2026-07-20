package drive

import (
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

func (d *Open115) ossUploadFile(ctx context.Context, token, init map[string]any, filePath string, fileSize int64) (map[string]any, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	ak, _ := token["AccessKeyId"].(string)
	sk, _ := token["AccessKeySecret"].(string)
	st, _ := token["SecurityToken"].(string)
	endpoint, _ := token["endpoint"].(string)

	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, st)).
		WithRegion("cn-shenzhen").
		WithEndpoint(endpoint)

	client := oss.NewClient(cfg)
	cb, _ := init["callback"].(map[string]any)
	cbStr, _ := cb["callback"].(string)
	cbVarStr, _ := cb["callback_var"].(string)

	cbBase64 := base64.StdEncoding.EncodeToString([]byte(cbStr))
	cbVarBase64 := base64.StdEncoding.EncodeToString([]byte(cbVarStr))

	bucket, _ := init["bucket"].(string)
	object, _ := init["object"].(string)

	// 小文件：单次 PUT 上传（带 OSS 回调）
	if fileSize <= ossMultipartThreshold {
		result, err := client.PutObjectFromFile(ctx, &oss.PutObjectRequest{
			Bucket:      &bucket,
			Key:         &object,
			Callback:    &cbBase64,
			CallbackVar: &cbVarBase64,
		}, filePath)
		if err != nil {
			return nil, err
		}
		return result.CallbackResult, nil
	}

	// 大文件：分片上传（对齐 OpenList multpartUpload 实现）
	// Step 1: 初始化分片上传，加 sequential 参数使 OSS 返回不带 -N 后缀的 ETag
	initReq := &oss.InitiateMultipartUploadRequest{
		Bucket: &bucket,
		Key:    &object,
		RequestCommon: oss.RequestCommon{
			Parameters: map[string]string{"sequential": ""},
		},
	}
	initResult, err := client.InitiateMultipartUpload(ctx, initReq)
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

		partReq := &oss.UploadPartRequest{
			Bucket:     &bucket,
			Key:        &object,
			UploadId:   &uploadID,
			PartNumber: partNum,
			Body:       io.NewSectionReader(f, offset, partSize),
		}

		partResult, err := client.UploadPart(ctx, partReq)
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
	completeResult, err := client.CompleteMultipartUpload(ctx, &oss.CompleteMultipartUploadRequest{
		Bucket:   &bucket,
		Key:      &object,
		UploadId: &uploadID,
		CompleteMultipartUpload: &oss.CompleteMultipartUpload{
			Parts: parts,
		},
		Callback:    &cbBase64,
		CallbackVar: &cbVarBase64,
	})
	if err != nil {
		return nil, fmt.Errorf("完成分片上传失败: %w", err)
	}

	return completeResult.CallbackResult, nil
}
