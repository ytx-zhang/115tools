package open115

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"golang.org/x/time/rate"
	yaml "gopkg.in/yaml.v3"
)

const (
	baseDomain     = "https://proapi.115.com"
	configFileName = "/app/data/config.yaml"
)

var (
	Conf        atomic.Pointer[config]
	mySafeTrans = &SafeTransport{
		BaseTransport: http.DefaultTransport,
		RateLimiter:   rate.NewLimiter(rate.Limit(2), 3),
		Concurrency:   make(chan struct{}, 2),
	}

	httpClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: mySafeTrans,
	}
)

type SafeTransport struct {
	BaseTransport http.RoundTripper
	RateLimiter   *rate.Limiter
	Concurrency   chan struct{}
	PauseUntil    atomic.Int64
}

func (t *SafeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.Concurrency <- struct{}{}
	defer func() { <-t.Concurrency }()

	if sleepTime := time.Until(time.Unix(0, t.PauseUntil.Load())); sleepTime > 0 {
		time.Sleep(sleepTime)
	}

	if err := t.RateLimiter.Wait(req.Context()); err != nil {
		return nil, err
	}

	resp, err := t.BaseTransport.RoundTrip(req)

	if err != nil || (resp != nil && resp.StatusCode == 429) {
		t.PauseUntil.Store(time.Now().Add(10 * time.Second).UnixNano())
	}

	return resp, err
}
func request[T any](ctx context.Context, method, endpoint string, params any, ua string) (*T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	urlStr := baseDomain + endpoint
	var bodyBytes []byte
	var ct string

	if method == "GET" {
		if p, ok := params.(string); ok {
			urlStr += "?" + p
		}
	} else {
		form := url.Values{}
		if p, ok := params.(map[string]string); ok {
			for k, v := range p {
				form.Set(k, v)
			}
		}
		bodyBytes = []byte(form.Encode())
		ct = "application/x-www-form-urlencoded"
	}

	var reqBody io.Reader
	if method != "GET" {
		reqBody = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+Conf.Load().Token.AccessToken)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("User-Agent", ua)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	bodyBytes, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read body failed: %w", err)
	}
	var res apiResponse

	if err := json.Unmarshal(bodyBytes, &res); err != nil {
        return nil, fmt.Errorf("115报错: 接口响应格式非法: %w", err)
    }

	if !res.State {
		mySafeTrans.PauseUntil.Store(time.Now().Add(10 * time.Second).UnixNano())
		return nil, fmt.Errorf("115报错: %s (code: %d)", res.Message, res.Code)
	}
	var actualData T

    dataStr := string(res.Data)
    if dataStr == "" || dataStr == "null" || dataStr == "[]" {
        return &actualData, nil
    }

	if err := json.Unmarshal(res.Data, &actualData); err != nil {
        return nil, fmt.Errorf("115报错: 解析data出错: %w", err)
    }

	return &actualData, nil
}

type downloadInfo struct {
	URL      string
	FileID   string
	FileName string
}

func GetDownloadUrl(ctx context.Context, pickCode, ua string) (*downloadInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	params := map[string]string{"pick_code": pickCode}
	res, err := request[downloadUrlData](ctx, "POST", "/open/ufile/downurl", params, ua)
	if err != nil {
		return nil, err
	}
	for k, v := range *res {
		return &downloadInfo{
			FileID:   k,
			URL:      v.Url.Url,
			FileName: v.FileName,
		}, nil
	}
	return nil, fmt.Errorf("未提取到下载信息")
}

func AddFolder(ctx context.Context, pid, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	params := map[string]string{"pid": pid, "file_name": name}
	res, err := request[addfolderData](ctx, "POST", "/open/folder/add", params, "")
	if err != nil {
		return "", err
	}
	return res.FileId, nil
}

func MoveFile(ctx context.Context, fid, cid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	params := map[string]string{"file_ids": fid, "to_cid": cid}
	_, err := request[any](ctx, "POST", "/open/ufile/move", params, "")
	return err
}

func UpdataFile(ctx context.Context, fid, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	params := map[string]string{"file_id": fid, "file_name": name}
	res, err := request[updatafileData](ctx, "POST", "/open/ufile/update", params, "")
	if err != nil {
		return "", err
	}
	return res.FileName, nil
}

func FolderInfo(ctx context.Context, pathStr string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	params := url.Values{}
	params.Set("path", pathStr)
	res, err := request[folderinfoData](ctx, "GET", "/open/folder/get_info", params.Encode(), "")
	if err != nil {
		return "", err
	}
	return res.FileId, nil
}

func FileList(ctx context.Context, cid string) ([]filelistData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	offset := 0
	limit := 1000
	var allFiles []filelistData
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		params := fmt.Sprintf("cid=%s&offset=%d&limit=%d&show_dir=1", cid, offset, limit)
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

func UploadFile(ctx context.Context, pathStr, cid, signKey, signVal string) (string, string, error) {
	if err := ctx.Err(); err != nil {
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
	params := map[string]string{
		"file_name": fileName,
		"file_size": fmt.Sprintf("%d", fileSize),
		"target":    fmt.Sprintf("U_1_%s", cid),
		"fileid":    fileSha1,
		"pre_id":    preSha1,
		"topupload": "0",
	}
	if signKey != "" && signVal != "" {
		params["sign_key"] = signKey
		params["sign_val"] = signVal
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
		// 将signCheck用_分割
		signParts := strings.Split(signCheck, "-")
		if len(signParts) != 2 {
			return "", "", fmt.Errorf("签名检查格式错误: %v", signParts)
		}
		offset := stringToInt64(signParts[0])
		length := stringToInt64(signParts[1])
		signVal, _ := fileSHA1Partial(pathStr, offset, length)
		return UploadFile(ctx, pathStr, cid, initData.SignKey, signVal)
	case 1:
		token, err := request[uploadtokenData](ctx, "GET", "/open/upload/get_token", "", "")
		if err != nil {
			return "", "", err
		}

		cbInfo := struct {
			Callback    string `json:"callback"`
			CallbackVar string `json:"callback_var"`
		}{}
		json.Unmarshal(initData.Callback, &cbInfo)

		res, err := ossUploadFile(ctx, token, initData, cbInfo.Callback, cbInfo.CallbackVar, pathStr)
		if err != nil {
			return "", "", err
		}
		if m, ok := res["message"].(string); ok && m != "" {
			return "", "", fmt.Errorf("OSS回调错误: %s", m)
		}
		fid := res["data"].(map[string]any)["file_id"].(string)
		pickcode := res["data"].(map[string]any)["pick_code"].(string)
		return fid, pickcode, nil
	}
	return "", "", fmt.Errorf("未知上传状态: %d", initData.Status)
}

func ossUploadFile(ctx context.Context, t *uploadtokenData, init *uploadInitData, cb, cbVar, filePath string) (map[string]any, error) {
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(t.AccessKeyId, t.AccessKeySecret, t.SecurityToken)).
		WithRegion("cn-shenzhen").
		WithEndpoint(t.Endpoint)

	client := oss.NewClient(cfg)

	putRequest := &oss.PutObjectRequest{
		Bucket:      oss.Ptr(init.Bucket),
		Key:         oss.Ptr(init.Object),
		Callback:    oss.Ptr(base64.StdEncoding.EncodeToString([]byte(cb))),
		CallbackVar: oss.Ptr(base64.StdEncoding.EncodeToString([]byte(cbVar))),
	}

	result, err := client.PutObjectFromFile(ctx, putRequest, filePath)
	if err != nil {
		return nil, err
	}
	return result.CallbackResult, nil
}

func refreshToken() bool {
	current := Conf.Load()
	if current == nil || current.Token.RefreshToken == "" {
		log.Printf("[TOKEN] 刷新失败: 缺少 RefreshToken")
		return false
	}
	if time.Until(current.Token.ExpireAt) > 10*time.Minute {
		return true
	}
	form := url.Values{"refresh_token": {strings.TrimSpace(current.Token.RefreshToken)}}
	resp, err := http.PostForm("https://passportapi.115.com/open/refreshToken", form)
	if err != nil {
		log.Printf("[TOKEN] 网络请求失败: %v", err)
		return false
	}
	defer resp.Body.Close()
	var res struct {
		State   int    `json:"state"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err == nil && res.State == 1 {
		newConf := *current
		newConf.Token.AccessToken = res.Data.AccessToken
		newConf.Token.ExpireAt = time.Now().Add(time.Duration(res.Data.ExpiresIn) * time.Second)
		if res.Data.RefreshToken != "" {
			newConf.Token.RefreshToken = res.Data.RefreshToken
		}

		Conf.Store(&newConf)
		d, _ := yaml.Marshal(&newConf)
		_ = os.WriteFile(configFileName, d, 0644)
		log.Printf("[TOKEN] 刷新成功! 有效期至: %v", newConf.Token.ExpireAt.Format("15:04:05"))
		return true
	}
	log.Printf("[TOKEN] 刷新失败: %v", res.Message)
	return false
}

func startCron() {
	go func() {
		for {
			if refreshToken() {
				time.Sleep(5 * time.Minute)
			} else {
				time.Sleep(1 * time.Minute)
			}
		}
	}()
}
func LoadConfig() {
	file, err := os.ReadFile(configFileName)
	if err != nil {
		Conf.Store(&config{})
		log.Fatalf("[CONFIG] 配置文件不存在: %s", configFileName)
	}

	var initialConf config
	if err := yaml.Unmarshal(file, &initialConf); err != nil {
		Conf.Store(&config{})
		log.Fatalf("[CONFIG] 配置文件格式解析失败: %v，使用空配置", err)
	}

	if initialConf.Token.RefreshToken == "" {
		log.Fatalf("[CONFIG] 警告: 配置不完整，缺少 RefreshToken")
	}

	Conf.Store(&initialConf)

	if initialConf.Token.ExpireAt.Unix() > 0 {
		log.Printf("[CONFIG] 已加载配置，Token 有效期至: %v", initialConf.Token.ExpireAt.Format("2006-01-02 15:04:05"))
	}

	startCron()
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
