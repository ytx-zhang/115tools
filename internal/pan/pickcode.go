package pan

import (
	"fmt"
	"strconv"
	"strings"
)

// pickcode 本地解码：从提取码直接解出文件/目录 FID，免去秒传命中后的网络补查。
// 算法来源：chenyanggao/p115client（modules/p115pickcode，GPLv3）。
// pickcode = 前缀 + 中缀 + 后缀(4 位)：前缀确定替换表，中缀为 FID 的 36 进制 + 替换加密，后缀为校验位。

// pickcodeTrans 前缀 → 替换表（明文 enc[i] → 密文 alphabet[i]）。
var pickcodeTrans = map[string]string{
	"a":  "fuln1ytpj3smg8d5a094qh7cxkbi62zvewro",
	"b":  "sk721n9a0emlfpcrzbqdw3gjh6ty5xui48vo",
	"c":  "ywcz3hite6f1j0guoakvdb2ns7p8qr9ml5x4",
	"d":  "rq2vl5o7wsken9u8tp4jg3zbyc6xmhifd01a",
	"e":  "ljm9eqbcfhw7ktv3x1dgp5ua8y6s4znr2io0",
	"fa": "fumk0ytpj3sng8d5a194qh7cxlbi62zvewro",
	"fb": "sk732o9a1enmfpcrzbqdw4gjh6ty5xui08vl",
	"fc": "ywcz6hite9f4j3gup2kvdb5osal0qr1nm8x7",
	"fd": "on6vl0r2wpkeq9u3ts8jg7zbyc1xmhifd45a",
	"fe": "ljm0es2cfhwakqv6x4dgp8r1by9u7znt5io3",
}

// PickcodeToID 从 pickcode 本地解码出 FID（纯本地无网络）。
func PickcodeToID(pickcode string) (string, error) {
	if pickcode == "" {
		return "", fmt.Errorf("pickcode 为空")
	}
	// 前缀长度：'f' 开头为 2 位（fa~fe），其余为 1 位（a~e）。
	// 剩余结构统一为「前缀 + 中缀 + 4 位校验」，最小长度 = 前缀 + 1 位中缀 + 4。
	prefixLen := 1
	if pickcode[0] == 'f' {
		prefixLen = 2
	}
	if len(pickcode) < prefixLen+5 || !strings.ContainsRune("abcdef", rune(pickcode[0])) {
		return "", fmt.Errorf("pickcode 前缀或长度异常: %q", pickcode)
	}
	prefix := pickcode[:prefixLen]
	cipher := pickcode[prefixLen : len(pickcode)-4]
	dec, ok := pickcodeTrans[prefix]
	if !ok {
		return "", fmt.Errorf("未知 pickcode 前缀: %s", prefix)
	}
	var sb strings.Builder
	sb.Grow(len(cipher))
	for i := range cipher {
		idx := pickcodeAlphabetIndex(cipher[i])
		if idx < 0 {
			return "", fmt.Errorf("pickcode 含非法字符: %q", cipher)
		}
		sb.WriteByte(dec[idx])
	}
	id, err := strconv.ParseUint(sb.String(), 36, 64)
	if err != nil {
		return "", fmt.Errorf("pickcode 解码失败: %w", err)
	}
	return strconv.FormatUint(id, 10), nil
}

// pickcodeAlphabetIndex 返回字符在 36 进制表中的下标，非法返回 -1。
func pickcodeAlphabetIndex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 10
	default:
		return -1
	}
}
