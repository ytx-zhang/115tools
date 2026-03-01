package open115

import (
	"115tools/config"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"golang.org/x/time/rate"
)

const baseDomain = "https://proapi.115.com"

var (
	limiter   = rate.NewLimiter(rate.Limit(2), 3)
	sem       = make(chan struct{}, 1)
	apiClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 30 * time.Second,
	}
)

func request[T any](ctx context.Context, method, endpoint string, params url.Values, ua string) (*T, error) {
	sem <- struct{}{}
	defer func() { <-sem }()
	var lastErr error
	for range 3 {
		if err := limiter.Wait(ctx); err != nil {
			return nil, context.Cause(ctx)
		}
		urlStr := baseDomain + endpoint
		var reqBody io.Reader
		if method == "POST" {
			reqBody = strings.NewReader(params.Encode())
		} else if params != nil {
			urlStr += "?" + params.Encode()
		}
		req, _ := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
		req.Header.Set("Authorization", "Bearer "+config.Get().Token.AccessToken)
		req.Header.Set("User-Agent", ua)
		if method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := apiClient.Do(req)
		if err != nil {
			if context.Cause(ctx) != nil {
				return nil, context.Cause(ctx)
			}
			lastErr = err
			select {
			case <-time.After(2 * time.Second):
				continue
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			}
		}
		var res apiResponse
		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("解析响应失败: %w", err)
		}
		if !res.State {
			lastErr = fmt.Errorf("115报错: %s code: %d", res.Message, res.Code)
			if strings.Contains(res.Message, "稍后再试") {
				slog.Warn("[115报错]", "接口消息", res.Message)
				select {
				case <-time.After(5 * time.Second):
					continue
				case <-ctx.Done():
					return nil, context.Cause(ctx)
				}
			}
			return nil, lastErr
		}
		var data T
		if len(res.Data) > 0 && string(res.Data) != "null" {
			_ = json.Unmarshal(res.Data, &data)
		}
		return &data, nil
	}

	return nil, lastErr
}

func GetDownloadUrl(ctx context.Context, pickCode, ua string) (string, string, string, error) {
	if err := context.Cause(ctx); err != nil {
		return "", "", "", err
	}
	params := url.Values{}
	params.Set("pick_code", pickCode)
	res, err := request[downloadUrlData](ctx, "POST", "/open/ufile/downurl", params, ua)
	if err != nil {
		return "", "", "", err
	}
	for fid, v := range *res {
		return fid, v.Url.Url, v.FileName, nil
	}
	return "", "", "", fmt.Errorf("未提取到下载信息")
}

func AddFolder(ctx context.Context, pid, name string) (string, error) {
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	params := url.Values{}
	params.Set("pid", pid)
	params.Set("file_name", name)
	res, err := request[addfolderData](ctx, "POST", "/open/folder/add", params, "")
	if err != nil {
		return "", err
	}
	return res.FileId, nil
}

func MoveFile(ctx context.Context, fid, cid string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	params := url.Values{}
	params.Set("file_ids", fid)
	params.Set("to_cid", cid)
	_, err := request[any](ctx, "POST", "/open/ufile/move", params, "")
	return err
}

func DeleteFile(ctx context.Context, fid string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	params := url.Values{}
	params.Set("file_ids", fid)
	_, err := request[any](ctx, "POST", "/open/ufile/delete", params, "")
	return err
}

func UpdataFile(ctx context.Context, fid, name string) (string, error) {
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	params := url.Values{}
	params.Set("file_id", fid)
	params.Set("file_name", name)
	res, err := request[updatafileData](ctx, "POST", "/open/ufile/update", params, "")
	if err != nil {
		return "", err
	}
	return res.FileName, nil
}

func FolderInfo(ctx context.Context, path string) (string, int, int, error) {
	if err := context.Cause(ctx); err != nil {
		return "", 0, 0, err
	}
	params := url.Values{}
	params.Set("path", path)
	res, err := request[folderinfoData](ctx, "GET", "/open/folder/get_info", params, "")
	if err != nil {
		return "", 0, 0, err
	}
	return res.FileId, res.Count, res.FolderCount, nil
}

func FileList(ctx context.Context, cid string) ([]filelistData, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	offset := 0
	limit := 1150
	var allFiles []filelistData
	for {
		if err := context.Cause(ctx); err != nil {
			return nil, context.Cause(ctx)
		}
		params := url.Values{}
		params.Set("cid", cid)
		params.Set("offset", strconv.Itoa(offset))
		params.Set("limit", strconv.Itoa(limit))
		params.Set("show_dir", "1")
		dataList, err := request[[]filelistData](ctx, "GET", "/open/ufile/files", params, "")
		if err != nil {
			return nil, err
		}
		count := len(*dataList)
		if count == 0 {
			break
		}
		allFiles = append(allFiles, (*dataList)...)
		offset += count
		if count < limit {
			break
		}
	}
	return allFiles, nil
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
func UploadFile(ctx context.Context, pathStr, cid, signKey, signVal string) (string, string, error) {
	if err := context.Cause(ctx); err != nil {
		return "", "", err
	}
	fileInfo, err := os.Stat(pathStr)
	if err != nil {
		return "", "", fmt.Errorf("获取文件信息失败: %v", err)
	}
	fileName := fileInfo.Name()
	fileSize := fileInfo.Size()
	fileSha1, err := fileSHA1(pathStr)
	if err != nil {
		return "", "", fmt.Errorf("计算文件 SHA1 失败: %v", err)
	}
	preSha1, err := fileSHA1Partial(pathStr, 0, 128)
	if err != nil {
		return "", "", fmt.Errorf("计算文件前128位 SHA1 失败: %v", err)
	}
	params := url.Values{}
	params.Set("file_name", fileName)
	params.Set("file_size", fmt.Sprintf("%d", fileSize))
	params.Set("target", fmt.Sprintf("U_1_%s", cid))
	params.Set("fileid", fileSha1)
	params.Set("pre_id", preSha1)
	params.Set("topupload", "0")
	if signKey != "" && signVal != "" {
		params.Set("sign_key", signKey)
		params.Set("sign_val", signVal)
	}
	if err := context.Cause(ctx); err != nil {
		return "", "", context.Cause(ctx)
	}
	initData, err := request[uploadInitData](ctx, "POST", "/open/upload/init", params, "")
	if err != nil {
		return "", "", err
	}

	switch initData.Status {
	case 2:
		return initData.FileId, initData.PickCode, nil
	case 7:
		signCheck := initData.SignCheck
		signParts := strings.Split(signCheck, "-")
		if len(signParts) != 2 {
			return "", "", fmt.Errorf("签名检查格式错误: %v", signParts)
		}
		offset := stringToInt64(signParts[0])
		length := stringToInt64(signParts[1])
		signVal, _ := fileSHA1Partial(pathStr, offset, length)
		if err := context.Cause(ctx); err != nil {
			return "", "", context.Cause(ctx)
		}
		return UploadFile(ctx, pathStr, cid, initData.SignKey, signVal)
	case 1:
		if err := context.Cause(ctx); err != nil {
			return "", "", context.Cause(ctx)
		}
		token, err := request[uploadtokenData](ctx, "GET", "/open/upload/get_token", url.Values{}, "")
		if err != nil {
			return "", "", err
		}

		cbInfo := struct {
			Callback    string `json:"callback"`
			CallbackVar string `json:"callback_var"`
		}{}
		json.Unmarshal(initData.Callback, &cbInfo)
		if err := context.Cause(ctx); err != nil {
			return "", "", context.Cause(ctx)
		}
		res, err := ossUploadFile(ctx, token, initData, cbInfo.Callback, cbInfo.CallbackVar, pathStr)
		if err != nil {
			return "", "", err
		}
		var cbResp ossCallbackResp
		resBytes, _ := json.Marshal(res)
		if err := json.Unmarshal(resBytes, &cbResp); err != nil {
			return "", "", fmt.Errorf("解析OSS回调数据失败: %w", err)
		}

		if !cbResp.State {
			return "", "", fmt.Errorf("OSS回调错误: %s", cbResp.Message)
		}

		if cbResp.Data.FileId == "" {
			return "", "", fmt.Errorf("OSS回调成功但未获取到文件ID")
		}

		return cbResp.Data.FileId, cbResp.Data.PickCode, nil
	}
	return "", "", fmt.Errorf("未知上传状态: %d", initData.Status)
}

func ossUploadFile(ctx context.Context, t *uploadtokenData, init *uploadInitData, cb, cbVar, filePath string) (map[string]any, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, context.Cause(ctx)
	}
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(t.AccessKeyId, t.AccessKeySecret, t.SecurityToken)).
		WithRegion("cn-shenzhen").
		WithEndpoint(t.Endpoint)

	client := oss.NewClient(cfg)

	putRequest := &oss.PutObjectRequest{
		Bucket:      new(init.Bucket),
		Key:         new(init.Object),
		Callback:    new(base64.StdEncoding.EncodeToString([]byte(cb))),
		CallbackVar: new(base64.StdEncoding.EncodeToString([]byte(cbVar))),
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

type apiResponse struct {
	State   bool            `json:"state"`
	Message string          `json:"message"`
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
}

type downloadUrlData map[string]struct {
	FileName string `json:"file_name"`
	Url      struct {
		Url string `json:"url"`
	} `json:"url"`
}

type addfolderData struct {
	FileName string `json:"file_name"`
	FileId   string `json:"file_id"`
}

type updatafileData struct {
	FileName string `json:"file_name"`
}

type folderinfoData struct {
	FileId      string `json:"file_id"`
	Count       int    `json:"count"`
	FolderCount int    `json:"folder_count"`
}

type filelistData struct {
	Fid string `json:"fid"`
	Aid string `json:"aid"`
	Fc  string `json:"fc"`
	Fn  string `json:"fn"`
	Fs  int64  `json:"fs"`
	Pc  string `json:"pc"`
	Isv int    `json:"isv"`
}

type uploadInitData struct {
	PickCode  string          `json:"pick_code"`
	Status    int             `json:"status"`
	FileId    string          `json:"file_id"`
	Target    string          `json:"target"`
	Bucket    string          `json:"bucket"`
	Object    string          `json:"object"`
	SignKey   string          `json:"sign_key"`
	SignCheck string          `json:"sign_check"`
	Callback  json.RawMessage `json:"callback"`
}

type callbackInfo struct {
	Callback    string `json:"callback"`
	CallbackVar string `json:"callback_var"`
}

type uploadtokenData struct {
	Endpoint        string `json:"endpoint"`
	AccessKeySecret string `json:"AccessKeySecret"`
	SecurityToken   string `json:"SecurityToken"`
	Expiration      string `json:"Expiration"`
	AccessKeyId     string `json:"AccessKeyId"`
}

type ossCallbackResp struct {
	State   bool   `json:"state"`
	Message string `json:"message"`
	Data    struct {
		FileId   string `json:"file_id"`
		PickCode string `json:"pick_code"`
	} `json:"data"`
}
