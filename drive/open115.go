package drive

import (
	"115tools/config"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
	"github.com/go-resty/resty/v2"
	"golang.org/x/time/rate"
)

type apiResponse[T any] struct {
	State   bool   `json:"state"`
	Message string `json:"message"`
	Code    int    `json:"code"`
	Data    T      `json:"data"`
}
type Open115 struct {
	Client    *resty.Client
	cfg       *config.Config
	refreshMu sync.Mutex
}

var fastSonic = sonic.Config{
	NoValidateJSONMarshaler: true,
	NoValidateJSONSkip:      true,
}.Froze()

func New115Drive(cfg *config.Config) *Open115 {
	d := &Open115{
		cfg: cfg,
	}
	limiter := rate.NewLimiter(rate.Limit(3), 5)
	d.Client = resty.New().
		SetBaseURL("https://proapi.115.com").
		SetJSONUnmarshaler(fastSonic.Unmarshal).
		SetJSONMarshaler(fastSonic.Marshal).
		SetTimeout(30 * time.Second).
		SetRetryCount(2).
		SetRetryWaitTime(3 * time.Second).
		SetRetryMaxWaitTime(3 * time.Second).
		OnBeforeRequest(func(c *resty.Client, r *resty.Request) error {
			if err := limiter.Wait(r.Context()); err != nil {
				return err
			}
			if err := d.refreshToken(r.Context()); err != nil {
				return err
			}
			r.SetHeader("Authorization", "Bearer "+d.cfg.GetAccessToken())
			return nil
		}).
		OnAfterResponse(func(c *resty.Client, r *resty.Response) error {
			if r.Error() != nil {
				return fmt.Errorf("网络底层错误: %v", r.Error())
			}
			var base apiResponse[any]
			if err := fastSonic.Unmarshal(r.Body(), &base); err != nil {
				return fmt.Errorf("JSON 解析失败: %w", err)
			}
			if !base.State {
				return fmt.Errorf("[115报错]: %s code: %d", base.Message, base.Code)
			}
			return nil
		}).
		AddRetryCondition(func(r *resty.Response, err error) bool {
			if err != nil {
				return strings.Contains(err.Error(), "稍后再试")
			}
			return false
		})
	if err := d.refreshToken(context.Background()); err != nil {
		slog.Error("[TOKEN] 更新token失败", "错误信息", err)
	}
	return d
}
func (d *Open115) refreshToken(ctx context.Context) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if time.Until(d.cfg.GetExpireAt()) > 5*time.Minute {
		return nil
	}

	form := url.Values{
		"refresh_token": {d.cfg.GetRefreshToken()},
	}

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://passportapi.115.com/open/refreshToken", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("[TOKEN] 网络请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var res struct {
		State   int    `json:"state"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			ExpiresIn    int64  `json:"expires_in"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := fastSonic.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("[TOKEN] 解析响应失败: %w", err)
	}
	if res.State == 1 {
		rt := res.Data.RefreshToken
		if rt == "" {
			rt = d.cfg.GetRefreshToken()
		}
		d.cfg.SaveToken(res.Data.AccessToken, rt, res.Data.ExpiresIn)
		nextDelay := time.Duration(res.Data.ExpiresIn)*time.Second - 10*time.Minute
		if nextDelay < 0 {
			nextDelay = 1 * time.Second
		}
		time.AfterFunc(nextDelay, func() {
			if err := d.refreshToken(context.Background()); err != nil {
				slog.Error("[TOKEN] 更新token失败", "错误信息", err)
			}
		})
		return nil
	}
	return fmt.Errorf("[TOKEN] 刷新失败: message: %s code: %d", res.Message, res.Code)
}

type DownloadUrlInfo struct {
	Fid  string
	Url  string
	Name string
}

func (d *Open115) GetDownloadUrl(ctx context.Context, pickCode, ua string) (*DownloadUrlInfo, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	var res apiResponse[map[string]struct {
		FileName string `json:"file_name"`
		Url      struct {
			Url string `json:"url"`
		} `json:"url"`
	}]
	req := d.Client.R().SetContext(ctx)
	if ua == "" {
		return nil, fmt.Errorf("请求ua为空")
	}
	req.SetHeader("User-Agent", ua)
	req.SetFormData(map[string]string{
		"pick_code": pickCode,
	})
	req.SetResult(&res)
	_, err := req.Post("/open/ufile/downurl")
	if err != nil {
		return nil, err
	}

	for fid, item := range res.Data {
		return &DownloadUrlInfo{
			Fid:  fid,
			Url:  item.Url.Url,
			Name: item.FileName,
		}, nil
	}
	return nil, fmt.Errorf("未提取到下载信息")
}
func (d *Open115) AddFolder(ctx context.Context, pid, name string) (fid string, err error) {
	if err = context.Cause(ctx); err != nil {
		return
	}
	var res apiResponse[struct {
		FileId string `json:"file_id"`
	}]
	req := d.Client.R().SetContext(ctx)
	req.SetFormData(map[string]string{
		"pid":       pid,
		"file_name": name,
	})
	req.SetResult(&res)
	if _, err = req.Post("/open/folder/add"); err != nil {
		return
	}
	fid = res.Data.FileId
	return
}
func (d *Open115) MoveFile(ctx context.Context, fid, cid string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	var res apiResponse[any]
	req := d.Client.R().SetContext(ctx)
	req.SetFormData(map[string]string{
		"file_ids": fid,
		"to_cid":   cid,
	})
	req.SetResult(&res)
	_, err := req.Post("/open/ufile/move")
	return err
}
func (d *Open115) DeleteFile(ctx context.Context, fid string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	req := d.Client.R().SetContext(ctx)
	req.SetFormData(map[string]string{
		"file_ids": fid,
	})
	_, err := req.Post("/open/ufile/delete")
	return err
}
func (d *Open115) UpdateFile(ctx context.Context, fid, name string) (newName string, err error) {
	if err = context.Cause(ctx); err != nil {
		return
	}
	var res apiResponse[struct {
		FileName string `json:"file_name"`
	}]
	req := d.Client.R().SetContext(ctx)
	req.SetFormData(map[string]string{
		"file_id":   fid,
		"file_name": name,
	})
	req.SetResult(&res)
	if _, err = req.Post("/open/ufile/update"); err != nil {
		return
	}
	newName = res.Data.FileName
	return
}

type DirInfo struct {
	Fid         string `json:"file_id"`
	FileCount   int64  `json:"count"`
	FolderCount int64  `json:"folder_count"`
}

func (d *Open115) GetDirInfo(ctx context.Context, path string) (*DirInfo, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	var res apiResponse[DirInfo]
	req := d.Client.R().SetContext(ctx)
	req.SetQueryParam("path", path)
	req.SetResult(&res)
	if _, err := req.Get("/open/folder/get_info"); err != nil {
		return nil, err
	}
	return &DirInfo{
		Fid:         res.Data.Fid,
		FileCount:   res.Data.FileCount,
		FolderCount: res.Data.FolderCount,
	}, nil
}

type FileInfo struct {
	Fid      string
	Name     string
	PickCode string
	Size     int64
	IsDir    bool
	IsVideo  bool
}

func (d *Open115) GetFileList(ctx context.Context, cid string) ([]FileInfo, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	type fileListResponse struct {
		State   bool   `json:"state"`
		Message string `json:"message"`
		Code    int    `json:"code"`
		Count   int64  `json:"count"`
		Data    []struct {
			Fid      string `json:"fid"`
			Name     string `json:"fn"`
			PickCode string `json:"pc"`
			Size     int64  `json:"fs"`
			IsVideo  int    `json:"isv"`
			Aid      string `json:"aid"`
			IsDir    string `json:"fc"`
		} `json:"data"`
	}
	var allFiles []FileInfo
	offset := 0
	req := d.Client.R().SetContext(ctx)
	req.SetQueryParams(map[string]string{
		"cid":      cid,
		"show_dir": "1",
		"limit":    "1150",
	})

	for {
		var res fileListResponse
		req.SetQueryParam("offset", strconv.Itoa(offset))
		req.SetResult(&res)

		if _, err := req.Get("/open/ufile/files"); err != nil {
			return nil, err
		}
		if offset == 0 && res.Count > 0 {
			allFiles = make([]FileInfo, 0, res.Count)
		}
		items := res.Data
		if len(items) == 0 {
			break
		}

		for _, item := range items {
			if item.Aid != "1" {
				continue
			}
			allFiles = append(allFiles, FileInfo{
				Fid:      item.Fid,
				Name:     item.Name,
				PickCode: item.PickCode,
				Size:     item.Size,
				IsDir:    item.IsDir == "0",
				IsVideo:  item.IsVideo == 1,
			})
		}
		offset += len(items)
		if int64(len(allFiles)) >= res.Count {
			break
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
	}
	return allFiles, nil
}

type UploadFileInfo struct {
	Fid      string
	PickCode string
}

func (d *Open115) UploadFile(ctx context.Context, pathStr, cid, signKey, signVal string) (*UploadFileInfo, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	fileInfo, err := os.Stat(pathStr)
	if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %v", err)
	}
	fileName := fileInfo.Name()
	fileSize := fileInfo.Size()
	fileSha1, _ := fileSHA1(pathStr)
	preSha1, _ := fileSHA1Partial(pathStr, 0, 128)
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
	root, err := sonic.Get(resp.Body())
	if err != nil {
		return nil, fmt.Errorf("解析初始化响应失败: %w", err)
	}
	initData := root.Get("data")
	status, _ := initData.Get("status").Int64()

	switch status {
	case 2:
		fid, _ := initData.Get("file_id").String()
		pc, _ := initData.Get("pick_code").String()
		return &UploadFileInfo{Fid: fid, PickCode: pc}, nil

	case 7:
		signCheck, _ := initData.Get("sign_check").String()
		parts := strings.Split(signCheck, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("签名检查格式错误: %s", signCheck)
		}
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end, _ := strconv.ParseInt(parts[1], 10, 64)
		newSignKey, _ := initData.Get("sign_key").String()
		newSignVal, _ := fileSHA1Partial(pathStr, start, end)
		return d.UploadFile(ctx, pathStr, cid, newSignKey, newSignVal)

	case 1:
		tokenReq := d.Client.R().SetContext(ctx)
		tokenResp, err := tokenReq.Get("/open/upload/get_token")
		if err != nil {
			return nil, err
		}
		tokenRoot, _ := sonic.Get(tokenResp.Body())
		tokenData := tokenRoot.Get("data")
		cbResp, err := d.ossUploadFile(ctx, tokenData, initData, pathStr)
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
		return nil, fmt.Errorf("未知上传状态码: %d", status)
	}
}

func (d *Open115) ossUploadFile(ctx context.Context, token, init *ast.Node, filePath string) (map[string]any, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	ak, _ := token.Get("AccessKeyId").String()
	sk, _ := token.Get("AccessKeySecret").String()
	st, _ := token.Get("SecurityToken").String()
	endpoint, _ := token.Get("endpoint").String()

	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, st)).
		WithRegion("cn-shenzhen").
		WithEndpoint(endpoint)

	client := oss.NewClient(cfg)
	cbStr, _ := init.Get("callback").Get("callback").String()
	cbVarStr, _ := init.Get("callback").Get("callback_var").String()

	cbBase64 := base64.StdEncoding.EncodeToString([]byte(cbStr))
	cbVarBase64 := base64.StdEncoding.EncodeToString([]byte(cbVarStr))

	bucket, _ := init.Get("bucket").String()
	object, _ := init.Get("object").String()

	putRequest := &oss.PutObjectRequest{
		Bucket:      &bucket,
		Key:         &object,
		Callback:    &cbBase64,
		CallbackVar: &cbVarBase64,
	}

	result, err := client.PutObjectFromFile(ctx, putRequest, filePath)
	if err != nil {
		return nil, err
	}

	return result.CallbackResult, nil
}
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

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

func sha1Hash(s []byte) string {
	hash := sha1.Sum(s)
	return fmt.Sprintf("%X", hash)
}

func fileSHA1(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)

	h := sha1.New()
	if _, err := io.CopyBuffer(h, f, *buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%X", h.Sum(nil)), nil
}
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
