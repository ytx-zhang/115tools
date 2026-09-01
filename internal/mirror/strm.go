package mirror

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ──── .strm 路径与内容（纯函数，无副作用）─────

// IsStrmPath 判断路径是否为 .strm 索引文件（大小写不敏感）。
func IsStrmPath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".strm")
}

// VideoToStrmPath 把视频路径改为同名 .strm 路径（去掉原扩展名）。
func VideoToStrmPath(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".strm"
}

// StrmContent 构造 .strm 文件内容（统一格式：strmURL 去尾斜杠 + /download?pickcode=）。
func StrmContent(strmURL, pickCode string) string {
	return fmt.Sprintf("%s/download?pickcode=%s", strings.TrimRight(strmURL, "/"), pickCode)
}

// trimBOM 去掉 UTF-8 BOM 与首尾空白（.strm 可能被外部工具带 BOM 写出）。
func trimBOM(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(s, "\xEF\xBB\xBF"))
}

// ReadStrmFile 读取 .strm 的原始内容（去 BOM 与首尾空白）；读取失败返回空串。
func ReadStrmFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return trimBOM(string(data))
}

// ParsePickCode 从 .strm 内容里解析出 pickcode；解析失败返回空串。
func ParsePickCode(content string) string {
	u, err := url.Parse(content)
	if err != nil {
		return ""
	}
	return u.Query().Get("pickcode")
}

// ParseStrmFile 读取 .strm 并解析出 pickcode。
func ParseStrmFile(path string) string {
	return ParsePickCode(ReadStrmFile(path))
}

// WriteStrmFile 写出 .strm 文件（一行直链）。
//
// 注意：这里**不**保留原 mtime。一致性判定改比 pickcode 之后，mtime 不再承担版本号职责，
// 因此也就不需要「覆写后把 mtime 改回去」这类补救动作。
func WriteStrmFile(strmURL, pickCode, path string) error {
	return os.WriteFile(path, []byte(StrmContent(strmURL, pickCode)), 0o644)
}

// StrmNeedsFix 判断 .strm 是否需要重写：pickcode 未变但链接格式不是本程序当前的输出格式
// （常见于 strm_url 改了 host 或端口）。
func StrmNeedsFix(strmURL, pickCode, content string) bool {
	if pickCode == "" {
		return false
	}
	return content != StrmContent(strmURL, pickCode)
}
