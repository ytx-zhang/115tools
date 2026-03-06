package open115

import (
	"115tools/config"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/go-resty/resty/v2"
	"github.com/tidwall/gjson"
	"golang.org/x/time/rate"
)

const baseDomain = "https://proapi.115.com"

var (
	limiter     = rate.NewLimiter(rate.Limit(2), 3)
	sem         = make(chan struct{}, 2)
	waitSec     atomic.Int64
	restyClient = resty.New().
			SetBaseURL(baseDomain).
			SetTimeout(30 * time.Second).
			SetRetryCount(2).
			SetRetryWaitTime(1 * time.Second).
			OnBeforeRequest(func(c *resty.Client, r *resty.Request) error {
			if err := limiter.Wait(r.Context()); err != nil {
				return err
			}
			if ws := waitSec.Load(); ws > 0 {
				select {
				case <-time.After(time.Duration(ws) * time.Second):
				case <-r.Context().Done():
					return r.Context().Err()
				}
			}
			return nil
		}).
		AddRetryCondition(func(r *resty.Response, err error) bool {
			if err != nil {
				waitSec.Store(1)
				return true
			}
			if strings.Contains(r.String(), "稍后再试") {
				slog.Warn("[115报错]", "接口消息", "触发频率限制，等待3秒")
				waitSec.Store(3)
				return true
			}
			return false
		})
)

func request(ctx context.Context, method, endpoint string, params url.Values, ua string) (data gjson.Result, err error) {
	sem <- struct{}{}
	defer func() {
		<-sem
		waitSec.Store(0)
	}()
	var resp *resty.Response
	resp, err = restyClient.R().
		SetContext(ctx).
		SetHeader("Authorization", "Bearer "+config.Get().Token.AccessToken).
		SetHeader("User-Agent", ua).
		SetQueryParamsFromValues(params).
		SetFormDataFromValues(params).
		Execute(method, endpoint)

	if err != nil {
		return
	}
	res := gjson.ParseBytes(resp.Body())
	if !res.Get("state").Bool() {
		err = fmt.Errorf("115报错: %s code: %d", res.Get("message").String(), res.Get("code").Int())
		return
	}
	data = res.Get("data")
	return
}
func GetDownloadUrl(ctx context.Context, pickCode, ua string) (fid, downloadUrl, name string, err error) {
	if err = context.Cause(ctx); err != nil {
		return
	}
	params := url.Values{}
	params.Set("pick_code", pickCode)
	var data gjson.Result
	if data, err = request(ctx, "POST", "/open/ufile/downurl", params, ua); err == nil {
		firstItem := data.Get("@values|0")
		fid = data.Get("@keys|0").String()

		if !firstItem.Exists() || fid == "" {
			err = fmt.Errorf("未提取到下载信息: %s", data.Raw)
			return
		}
		downloadUrl = firstItem.Get("url.url").String()
		name = firstItem.Get("file_name").String()
	}
	return
}
func AddFolder(ctx context.Context, pid, name string) (fid string, err error) {
	if err = context.Cause(ctx); err != nil {
		return
	}
	params := url.Values{}
	params.Set("pid", pid)
	params.Set("file_name", name)
	var data gjson.Result
	if data, err = request(ctx, "POST", "/open/folder/add", params, ""); err == nil {
		fid = data.Get("file_id").String()
	}
	return
}
func MoveFile(ctx context.Context, fid, cid string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	params := url.Values{}
	params.Set("file_ids", fid)
	params.Set("to_cid", cid)
	_, err := request(ctx, "POST", "/open/ufile/move", params, "")
	waitSec.Store(1)
	return err
}
func DeleteFile(ctx context.Context, fid string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	params := url.Values{}
	params.Set("file_ids", fid)
	_, err := request(ctx, "POST", "/open/ufile/delete", params, "")
	return err
}
func UpdateFile(ctx context.Context, fid, name string) (newName string, err error) {
	if err = context.Cause(ctx); err != nil {
		return
	}
	params := url.Values{}
	params.Set("file_id", fid)
	params.Set("file_name", name)
	var data gjson.Result
	if data, err = request(ctx, "POST", "/open/ufile/update", params, ""); err == nil {
		newName = data.Get("file_name").String()
	}
	return
}
func FolderInfo(ctx context.Context, path string) (fid string, fileCount, folderCount int64, err error) {
	if err = context.Cause(ctx); err != nil {
		return
	}
	params := url.Values{}
	params.Set("path", path)
	var data gjson.Result
	if data, err = request(ctx, "GET", "/open/folder/get_info", params, ""); err == nil {
		fid = data.Get("file_id").String()
		fileCount = data.Get("count").Int()
		folderCount = data.Get("folder_count").Int()
	}
	return
}
func FileList(ctx context.Context, cid string) (allFiles []gjson.Result, err error) {
	if err = context.Cause(ctx); err != nil {
		return
	}
	offset := 0
	limit := 1150
	params := url.Values{}
	params.Set("cid", cid)
	params.Set("show_dir", "1")
	params.Set("limit", strconv.Itoa(limit))
	for {
		params.Set("offset", strconv.Itoa(offset))

		var data gjson.Result
		data, err = request(ctx, "GET", "/open/ufile/files", params, "")
		if err != nil {
			return
		}
		items := data.Array()
		count := len(items)
		if count == 0 {
			break
		}
		allFiles = append(allFiles, items...)
		offset += count
		if count < limit {
			break
		}
		if err = context.Cause(ctx); err != nil {
			return
		}
	}
	return
}
func DownloadFile(ctx context.Context, pickcode, localPath string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	_, url, _, err := GetDownloadUrl(ctx, pickcode, "")
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}

	out, err := os.Create(localPath)
	if err != nil {
		return err
	}

	defer func() {
		out.Close()
		if err != nil {
			os.Remove(localPath)
		}
	}()

	_, err = io.Copy(out, resp.Body)
	return err
}
func SaveStrmFile(pickcode, fid, localPath string) error {
	content := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", config.Get().StrmUrl, pickcode, fid)
	contentBytes := []byte(content)
	if err := os.WriteFile(localPath, contentBytes, 0644); err != nil {
		return err
	}
	return nil
}
func UploadFile(ctx context.Context, pathStr, cid, signKey, signVal string) (fid, pickCode string, err error) {
	if err = context.Cause(ctx); err != nil {
		return
	}
	fileInfo, err := os.Stat(pathStr)
	if err != nil {
		err = fmt.Errorf("获取文件信息失败: %v", err)
		return
	}
	fileName := fileInfo.Name()
	fileSize := fileInfo.Size()
	var fileSha1 string
	if fileSha1, err = fileSHA1(pathStr); err != nil {
		err = fmt.Errorf("计算文件 SHA1 失败: %v", err)
		return
	}
	var preSha1 string
	if preSha1, err = fileSHA1Partial(pathStr, 0, 128); err != nil {
		err = fmt.Errorf("计算文件前128位 SHA1 失败: %v", err)
		return
	}

	params := url.Values{}
	params.Set("file_name", fileName)
	params.Set("file_size", fmt.Sprintf("%d", fileSize))
	params.Set("target", fmt.Sprintf("U_1_%s", cid))
	params.Set("fileid", fileSha1)
	params.Set("preid", preSha1)
	params.Set("topupload", "0")
	if signKey != "" && signVal != "" {
		params.Set("sign_key", signKey)
		params.Set("sign_val", signVal)
	}
	var initData gjson.Result
	if initData, err = request(ctx, "POST", "/open/upload/init", params, ""); err != nil {
		return
	}
	status := initData.Get("status").Int()
	switch status {
	case 2:
		return initData.Get("file_id").String(), initData.Get("pick_code").String(), nil

	case 7:
		signCheck := initData.Get("sign_check").String()
		signParts := strings.Split(signCheck, "-")
		if len(signParts) != 2 {
			err = fmt.Errorf("签名检查格式错误: %v", signParts)
			return
		}
		offset := stringToInt64(signParts[0])
		length := stringToInt64(signParts[1])
		newSignVal, _ := fileSHA1Partial(pathStr, offset, length)
		return UploadFile(ctx, pathStr, cid, initData.Get("sign_key").String(), newSignVal)

	case 1:
		var tokenData gjson.Result
		if tokenData, err = request(ctx, "GET", "/open/upload/get_token", url.Values{}, ""); err != nil {
			return
		}
		var cbResp map[string]any
		if cbResp, err = ossUploadFile(ctx, tokenData, initData, pathStr); err != nil {
			err = fmt.Errorf("OSS调用底层报错: %w", err)
			return
		}
		state, ok := cbResp["state"].(bool)
		if !ok || !state {
			err = fmt.Errorf("OSS状态异常或格式错误: %+v", cbResp)
			return
		}
		data, ok := cbResp["data"].(map[string]any)
		if !ok || data == nil {
			err = fmt.Errorf("响应中缺少有效的 data 节点: %+v", cbResp)
			return
		}
		fid, _ = data["file_id"].(string)
		pickCode, _ = data["pick_code"].(string)

		if fid == "" || pickCode == "" {
			err = fmt.Errorf("data 节点中 fid/pickcode 缺失或类型错误: %+v", cbResp)
			return
		}
		return
	default:
		err = fmt.Errorf("未知上传状态: %d", status)
		return
	}
}
func ossUploadFile(ctx context.Context, t, init gjson.Result, filePath string) (map[string]any, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, context.Cause(ctx)
	}
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			t.Get("AccessKeyId").String(),
			t.Get("AccessKeySecret").String(),
			t.Get("SecurityToken").String(),
		)).
		WithRegion("cn-shenzhen").
		WithEndpoint(t.Get("endpoint").String())
	cb := init.Get("callback.callback").String()
	cbVar := init.Get("callback.callback_var").String()
	cbBase64 := base64.StdEncoding.EncodeToString([]byte(cb))
	cbVarBase64 := base64.StdEncoding.EncodeToString([]byte(cbVar))
	client := oss.NewClient(cfg)
	putRequest := &oss.PutObjectRequest{
		Bucket:      new(init.Get("bucket").String()),
		Key:         new(init.Get("object").String()),
		Callback:    new(cbBase64),
		CallbackVar: new(cbVarBase64),
	}

	result, err := client.PutObjectFromFile(ctx, putRequest, filePath)
	if err != nil {
		return nil, err
	}
	return result.CallbackResult, nil
}
func sha1Hash(s []byte) string {
	hash := sha1.Sum(s)
	return strings.ToUpper(hex.EncodeToString(hash[:]))
}
func fileSHA1(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha1.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(hash.Sum(nil))), nil
}
func fileSHA1Partial(filePath string, offset int64, length int64) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = f.Seek(offset, io.SeekStart)
	if err != nil {
		return "", err
	}
	buf := make([]byte, length-offset+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	return sha1Hash(buf[:n]), nil
}
func stringToInt64(s string) int64 {
	if s == "" {
		return 0
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return i
}
