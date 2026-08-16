package drive

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// 本文件实现极简 bencode 解析器：仅用于 .torrent 种子文件，
// 提取 info dict 的原始字节区间（SHA1 即 BT info hash）与 name 字段。
// 开放平台不支持云端解析 .torrent，统一本地解析种子转磁链。
// 不引入完整 bencode 依赖，符合项目少依赖风格。
//
// bencode 编码要点（只支持种子所需子集）：
//   - 字符串：<十进制长度>:<字节>
//   - 整数：i<十进制>e
//   - 列表：l...e
//   - 字典：d<key><value>...e（key 为 bencode 字符串）

// torrentInfo 解析结果。
type torrentInfo struct {
	InfoHash string // info dict 的 SHA1，大写 hex（= BT info hash）
	Name     string // 种子名（info.name）
}

// ParseTorrentInfo 解析 .torrent 字节，返回 info hash（大写 hex）与种子名。
func ParseTorrentInfo(data []byte) (infoHash, name string, err error) {
	info, err := parseTorrent(data)
	if err != nil {
		return "", "", err
	}
	return info.InfoHash, info.Name, nil
}

// parseTorrent 解析种子：定位顶层 dict 的 info 键，取 value 原始区间，并读取 name。
func parseTorrent(data []byte) (*torrentInfo, error) {
	pos := 0
	if pos >= len(data) || data[pos] != 'd' {
		return nil, fmt.Errorf("种子格式错误: 顶层非字典")
	}
	pos++ // 跳过 'd'

	infoStart, infoEnd := -1, -1
	name := ""

	for pos < len(data) {
		// 读到 'e' 表示顶层 dict 结束
		if data[pos] == 'e' {
			break
		}
		// 读 key
		key, next, err := readBString(data, pos)
		if err != nil {
			return nil, fmt.Errorf("读取键失败: %w", err)
		}
		pos = next
		if key == "info" {
			infoStart = pos
			infoEnd = skipValue(data, pos)
			pos = infoEnd
		} else if key == "name" && name == "" {
			// 顶层 name（极少数种子放外面），兜底用
			if v, np, e := readBString(data, pos); e == nil {
				name = v
				pos = np
			} else {
				pos = skipValue(data, pos)
			}
		} else {
			pos = skipValue(data, pos)
		}
	}

	if infoStart < 0 || infoEnd <= infoStart {
		return nil, fmt.Errorf("种子格式错误: 缺少 info 字典")
	}

	// info hash = info dict 原始字节的 SHA1（大写 hex）
	infoHash := sha1Hex(data[infoStart:infoEnd])

	// 读取 info dict 内的 name（通常 UTF-8）
	if n := parseInfoName(data[infoStart:infoEnd]); n != "" {
		name = n
	}
	return &torrentInfo{InfoHash: infoHash, Name: name}, nil
}

// parseInfoName 在 info dict 原始字节内找 name 键（跳过 "name" 前缀的 <len>: 编码）。
func parseInfoName(infoData []byte) string {
	pos := 0
	if len(infoData) == 0 || infoData[0] != 'd' {
		return ""
	}
	pos++
	for pos < len(infoData) {
		key, next, err := readBString(infoData, pos)
		if err != nil {
			return ""
		}
		pos = next
		if key == "name" {
			v, _, e := readBString(infoData, pos)
			if e == nil {
				return v
			}
			return ""
		}
		pos = skipValue(infoData, pos)
		if pos >= len(infoData) || infoData[pos] == 'e' {
			return ""
		}
	}
	return ""
}

// readBString 读取 bencode 字符串：返回内容与下一个位置。
func readBString(data []byte, pos int) (string, int, error) {
	colon := bytes.IndexByte(data[pos:], ':')
	if colon < 0 {
		return "", pos, fmt.Errorf("字符串缺少冒号")
	}
	colon += pos
	length, err := strconv.Atoi(string(data[pos:colon]))
	if err != nil || length < 0 {
		return "", pos, fmt.Errorf("字符串长度非法")
	}
	end := colon + 1 + length
	if end > len(data) {
		return "", pos, fmt.Errorf("字符串越界")
	}
	return string(data[colon+1 : end]), end, nil
}

// skipValue 跳过任意 bencode 值，返回下一个位置。
// ⚠️ dict/list 内必须逐个解析元素而不能逐字节深度计数：字符串内容可能含
// 'd'/'l'/'e' 字节（如 key "length" 里的 e），逐字节统计会把它们误判为
// 结构符导致 dict 提前结束、后续 key 解析错乱（"字符串长度非法"）。
func skipValue(data []byte, pos int) int {
	if pos >= len(data) {
		return pos
	}
	switch data[pos] {
	case 'i':
		end := bytes.IndexByte(data[pos:], 'e')
		if end < 0 {
			return len(data)
		}
		return pos + end + 1
	case 'l', 'd':
		pos++ // 跳过容器开头的 'd'/'l'，从首个元素开始
		for {
			if pos >= len(data) {
				return len(data)
			}
			switch data[pos] {
			case 'e':
				return pos + 1 // dict/list 结束符
			case 'i':
				end := bytes.IndexByte(data[pos:], 'e')
				if end < 0 {
					return len(data)
				}
				pos += end + 1
			case 'l', 'd':
				pos = skipValue(data, pos) // 嵌套 dict/list，递归跳过
			default:
				// 字符串：按 "长度:内容" 整体跳过，不逐个字节统计
				_, next, err := readBString(data, pos)
				if err != nil {
					return len(data)
				}
				pos = next
			}
		}
	default:
		_, next, err := readBString(data, pos)
		if err != nil {
			return len(data)
		}
		return next
	}
}

// sha1Hex 计算整段字节的 SHA1（大写 hex）。
func sha1Hex(b []byte) string {
	h := sha1.Sum(b)
	return strings.ToUpper(hex.EncodeToString(h[:]))
}
