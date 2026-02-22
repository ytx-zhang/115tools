package open115

import (
	"encoding/json"
	"time"
)

type tokenConfig struct {
	AccessToken  string    `yaml:"access_token" json:"access_token"`
	RefreshToken string    `yaml:"refresh_token" json:"refresh_token"`
	ExpireAt     time.Time `yaml:"expire_at" json:"expire_at"`
}

// 导出类型供外部使用
type TokenConfig = tokenConfig
type Config = config

type config struct {
	Token        tokenConfig `yaml:"token" json:"token"`
	SyncPath     string      `yaml:"sync_path" json:"sync_path"`
	StrmPath     string      `yaml:"strm_path" json:"strm_path"`
	TempPath     string      `yaml:"temp_path" json:"temp_path"`
	StrmUrl      string      `yaml:"strm_url" json:"strm_url"`
	EmbyUrl      string      `yaml:"emby_url" json:"emby_url"`
	FontInAssUrl string      `yaml:"fontinass_url" json:"fontinass_url"`
}

type apiResponse struct {
	State   bool            `json:"state"`
	Message string          `json:"message"`
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
}

// 获取下载地址
type downloadUrlData map[string]struct {
	FileName string               `json:"file_name"`
	Url      struct{ Url string } `json:"url"`
}

// 新建文件夹
type addfolderData struct {
	FileName string `json:"file_name"`
	FileId   string `json:"file_id"`
}

// 文件(夹)更新
type updatafileData struct {
	FileName string `json:"file_name"`
}

// 获取文件夹fid
type folderinfoData struct {
	FileId string `json:"file_id"`
}

// 获取文件列表
type filelistData struct {
	Fid string `json:"fid"` //文件ID
	Aid string `json:"aid"` //文件的状态，aid 的别名。1 正常，7 删除(回收站)，120 彻底删除
	Fc  string `json:"fc"`  //文件分类。0 文件夹，1 文件
	Fn  string `json:"fn"`  //文件(夹)名称
	Fs  int64  `json:"fs"`  //文件大小
	Pc  string `json:"pc"`  //文件提取码
	Isv int    `json:"isv"` //是否为视频
}

// 上传初始化
type uploadInitData struct {
	PickCode  string          `json:"pick_code"`
	Status    int             `json:"status"`
	FileId    string          `json:"file_id"`
	Target    string          `json:"target"`
	Bucket    string          `json:"bucket"`
	Object    string          `json:"object"`
	SignKey   string          `json:"sign_key"`
	SignCheck string          `json:"sign_check"`
	Callback  json.RawMessage `json:"callback"` // 改为原始消息
}

// 定义真正的 Callback 内容结构
type callbackInfo struct {
	Callback    string `json:"callback"`
	CallbackVar string `json:"callback_var"`
}

// 获取上传 Token
type uploadtokenData struct {
	Endpoint        string `json:"endpoint"`
	AccessKeySecret string `json:"AccessKeySecret"`
	SecurityToken   string `json:"SecurityToken"`
	Expiration      string `json:"Expiration"`
	AccessKeyId     string `json:"AccessKeyId"`
}
