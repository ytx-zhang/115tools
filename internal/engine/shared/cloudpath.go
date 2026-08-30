package shared

import (
	"context"
	"fmt"
	"strings"

	"github.com/ytx-zhang/115tools/internal/pan"
)

// EnsureCloudDir 逐级确保云端绝对路径存在（每级 CreateFolder，同名自动复用），返回末级 FID。
// 供 engine（回收目录）与 push.CloudOps（任务云端根）共用，避免重复实现逐级创建逻辑。
func EnsureCloudDir(ctx context.Context, api *pan.Client, path string) (string, error) {
	parentFid := "0"
	cur := ""
	for seg := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		cur += "/" + seg
		fid, err := api.CreateFolder(ctx, parentFid, seg, cur)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", cur, err)
		}
		parentFid = fid
	}
	return parentFid, nil
}
