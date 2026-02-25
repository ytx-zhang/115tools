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
