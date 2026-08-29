package pan

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// 极简 bencode 解析器：仅用于 .torrent 种子文件，提取 info dict 的原始字节区间（SHA1 即 info hash）
// 与 name 字段。开放平台不支持云端解析 .torrent，统一本地解析种子转磁链。

// torrentInfo 解析结果。
type torrentInfo struct {
	InfoHash string
	Name     string
}

// ParseTorrentInfo 解析 .torrent 字节，返回 info hash（大写 hex）与种子名。
func ParseTorrentInfo(data []byte) (infoHash, name string, err error) {
	info, err := parseTorrent(data)
	if err != nil {
		return "", "", err
	}
	return info.InfoHash, info.Name, nil
}

// parseTorrent 定位顶层 dict 的 info 键，取 value 原始区间，并读取 name。
func parseTorrent(data []byte) (*torrentInfo, error) {
	pos := 0
	if pos >= len(data) || data[pos] != 'd' {
		return nil, fmt.Errorf("种子格式错误: 顶层非字典")
	}
	pos++

	infoStart, infoEnd := -1, -1
	name := ""

	for pos < len(data) {
		if data[pos] == 'e' {
			break
		}
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
	infoHash := sha1Hex(data[infoStart:infoEnd])
	if n := parseInfoName(data[infoStart:infoEnd]); n != "" {
		name = n
	}
	return &torrentInfo{InfoHash: infoHash, Name: name}, nil
}

// parseInfoName 在 info dict 原始字节内找 name 键。
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

// readBString 读取 bencode 字符串：返回内容与下一位置。
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

// skipValue 跳过任意 bencode 值，返回下一位置。
// dict/list 内必须逐个解析元素而不能逐字节深度计数：字符串内容可能含 'd'/'l'/'e' 字节，
// 逐字节统计会把它们误判为结构符导致结构错乱。
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
		pos++
		for {
			if pos >= len(data) {
				return len(data)
			}
			switch data[pos] {
			case 'e':
				return pos + 1
			case 'i':
				end := bytes.IndexByte(data[pos:], 'e')
				if end < 0 {
					return len(data)
				}
				pos += end + 1
			case 'l', 'd':
				pos = skipValue(data, pos)
			default:
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
