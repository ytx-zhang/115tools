package drive

import (
	"context"
	"fmt"
)

// GetUserInfo 获取用户空间信息（GET /open/user/info，需 Bearer 鉴权）。
// 返回用户名、空间占用，用于验证成功后打印账户概况。
// 鉴权头由 Client 的 resty 中间件在请求前自动注入（Bearer access_token）。
func (c *Client) GetUserInfo(ctx context.Context) (*UserInfo, error) {
	data, dur, err := Get[userInfoData](ctx, c, "/open/user/info", nil)
	if err != nil {
		logCloud("获取用户信息", err, dur)
		return nil, err
	}
	info := &UserInfo{
		UserName:   data.UserName,
		UsedSize:   data.RtSpaceInfo.AllUse.SizeFormat,
		TotalSize:  data.RtSpaceInfo.AllTotal.SizeFormat,
		RemainSize: data.RtSpaceInfo.AllRemain.SizeFormat,
	}
	// 成功：补充云端返回的用户名与空间概况
	logCloud("获取用户信息", nil, dur, "账户", info.String())
	return info, nil
}

// UserInfo 是 /open/user/info 的精简展示模型（仅保留打印所需的字段）。
type UserInfo struct {
	UserName   string
	UsedSize   string // 已用空间（格式化）
	TotalSize  string // 总空间（格式化）
	RemainSize string // 剩余空间（格式化）
}

// String 账户概况单行（验证通过后打印）。
func (u *UserInfo) String() string {
	return fmt.Sprintf("%s  空间=%s/%s(剩%s)", u.UserName, u.UsedSize, u.TotalSize, u.RemainSize)
}

// ──── /open/user/info 原始响应（data 段，仅取打印所需字段）────

type userInfoData struct {
	UserName    string      `json:"user_name"`
	RtSpaceInfo rtSpaceInfo `json:"rt_space_info"`
}

type rtSpaceInfo struct {
	AllTotal  spaceSize `json:"all_total"`
	AllRemain spaceSize `json:"all_remain"`
	AllUse    spaceSize `json:"all_use"`
}

type spaceSize struct {
	SizeFormat string `json:"size_format"`
}
