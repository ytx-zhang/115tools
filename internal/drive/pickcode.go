package drive

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件实现 115 pickcode 的本地解码：直接从提取码解出文件/目录 FID，
// 免去秒传命中后按 pickcode 网络补查（files/get_info）。
//
// 算法来源：chenyanggao/p115client（modules/p115pickcode，GPLv3）。
// pickcode 结构 = 前缀 + 中缀 + 后缀(4位)：
//   - 前缀唯一确定一张替换表（文件 a-e 取 1 位，目录 f 开头取 2 位 fa-fe）；
//   - 中缀 = FID 的 36 进制表示 + 替换表简单替换加密；
//   - 后缀 4 位是用户级"不动点"（校验用），解码时去掉。
//
// 本实现为纯本地计算（无网络），已在 2026-08-12 用真实数据验证：
// e51yswi02p66rzs8w → 3494080219994654680、bio48d7jfimox8wc3 → 3484422535041254636。

// pickcodeTrans 前缀 → 替换表（明文 enc[i] → 密文 alphabet[i]，与 p115pickcode 一致）。
// 解码时取反：密文 alphabet[i] → 明文 enc[i]。
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

// PickcodeToID 从 115 pickcode 本地解码出 FID（文件/目录通用），纯本地无网络。
// 解码失败（非法字符/未知前缀/位数不足）返回错误。
func PickcodeToID(pickcode string) (string, error) {
	if pickcode == "" {
		return "", fmt.Errorf("pickcode 为空")
	}
	var prefix, cipher string
	if pickcode[0] == 'f' && len(pickcode) >= 7 {
		prefix = pickcode[:2]
		cipher = pickcode[2 : len(pickcode)-4]
	} else if strings.ContainsRune("abcde", rune(pickcode[0])) && len(pickcode) >= 6 {
		prefix = pickcode[:1]
		cipher = pickcode[1 : len(pickcode)-4]
	} else {
		return "", fmt.Errorf("pickcode 前缀或长度异常: %q", pickcode)
	}
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

// pickcodeAlphabetIndex 返回字符在 36 进制表中的下标，非法字符返回 -1。
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
