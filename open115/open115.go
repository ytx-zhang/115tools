package open115

import (
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
func request[T any](ctx context.Context, method, endpoint string, params url.Values, ua string) (*T, error) {
	// 重试配置
	const maxRetries = 3
	var lastErr error

	for i := range maxRetries {
		// 每次重试前检查 Context 是否已取消
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		urlStr := baseDomain + endpoint
		var reqBody io.Reader
		var ct string
		queryString := params.Encode()

		if method == "GET" {
			if queryString != "" {
				urlStr += "?" + queryString
			}
		} else {
			reqBody = strings.NewReader(queryString)
			ct = "application/x-www-form-urlencoded"
		}

		req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
		if err != nil {
			return nil, err
		}

		req.Header.Set("Authorization", "Bearer "+Conf.Load().Token.AccessToken)
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("User-Agent", ua)

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			// 网络层错误通常也值得短时间重试一次
			time.Sleep(3 * time.Second)
			continue
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close() // 提前关闭 Body 以免泄露
		if err != nil {
			lastErr = fmt.Errorf("read body failed: %w", err)
			continue
		}

		var res apiResponse
		if err := json.Unmarshal(bodyBytes, &res); err != nil {
			return nil, fmt.Errorf("115报错: 接口响应格式非法: %w", err)
		}

		// 核心重试逻辑：判断业务状态
		if !res.State {
			lastErr = fmt.Errorf("115报错: %s (code: %d)", res.Message, res.Code)

			// 触发重试的特定错误码：990019 (稍后再试)
			if strings.Contains(res.Message, "稍后再试") {
				log.Printf("115报错: %s (第 %d/%d 次重试): ", res.Message, i+1, maxRetries)

				// 全局暂停 10s，防止其他并发请求撞墙
				mySafeTrans.PauseUntil.Store(time.Now().Add(10 * time.Second).UnixNano())

				select {
				case <-time.After(10 * time.Second):
					continue // 进入下一次循环
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}

			// 其他业务错误（如 Token 失效等）直接返回，不重试
			return nil, lastErr
		}

		// 成功逻辑
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

	return nil, fmt.Errorf("115报错: %w", lastErr)
}

func GetDownloadUrl(ctx context.Context, pickCode, ua string) (string, string, string, error) {
	if err := ctx.Err(); err != nil {
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
	if err := ctx.Err(); err != nil {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	params := url.Values{}
	params.Set("file_ids", fid)
	params.Set("to_cid", cid)
	_, err := request[any](ctx, "POST", "/open/ufile/move", params, "")
	return err
}

func DeleteFile(ctx context.Context, fid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	params := url.Values{}
	params.Set("file_ids", fid)
	_, err := request[any](ctx, "POST", "/open/ufile/delete", params, "")
	return err
}

func UpdataFile(ctx context.Context, fid, name string) (string, error) {
	if err := ctx.Err(); err != nil {
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

func FolderInfo(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	params := url.Values{}
	params.Set("path", path)
	res, err := request[folderinfoData](ctx, "GET", "/open/folder/get_info", params, "")
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
		token, err := request[uploadtokenData](ctx, "GET", "/open/upload/get_token", url.Values{}, "")
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
