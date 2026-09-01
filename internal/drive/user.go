package drive

import (
	"context"
	"fmt"
)

// UserInfo 账户概况（验证通过后打印）。
type UserInfo struct {
	UserName   string
	UsedSize   string
	TotalSize  string
	RemainSize string
}

// String 账户概况单行。
func (u *UserInfo) String() string {
	return fmt.Sprintf("%s  空间=%s/%s(剩%s)", u.UserName, u.UsedSize, u.TotalSize, u.RemainSize)
}

// GetUserInfo 获取用户空间信息（GET /open/user/info，需 Bearer 鉴权）。
func (c *Client) GetUserInfo(ctx context.Context) (*UserInfo, error) {
	data, dur, err := Get[userInfoData](ctx, c, "/open/user/info", nil)
	if err != nil {
		logCall(ctx, "获取用户信息", err, dur)
		return nil, err
	}
	info := &UserInfo{
		UserName:   data.UserName,
		UsedSize:   data.RtSpaceInfo.AllUse.SizeFormat,
		TotalSize:  data.RtSpaceInfo.AllTotal.SizeFormat,
		RemainSize: data.RtSpaceInfo.AllRemain.SizeFormat,
	}
	logCall(ctx, "获取用户信息", nil, dur, "账户", info.String())
	return info, nil
}

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
