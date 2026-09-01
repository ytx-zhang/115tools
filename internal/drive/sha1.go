package drive

import (
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"sync"
)

// ──── SHA1 工具（大写 hex，115 秒传与二次校验强制格式） ────

// bufPool 复用 SHA1 计算用的读缓冲（大文件多次上传时不重复分配）。
var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

// FileSHA1WithPreid 单次遍历文件，同时计算全量 SHA1 与前 128KB 的 SHA1（preid）。
func FileSHA1WithPreid(filePath string) (full, pre string, err error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // 只读文件，关闭失败无补救动作

	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	buf := *bufPtr

	hFull := sha1.New()
	hPre := sha1.New()

	head := io.LimitReader(f, 128*1024)
	if _, err := io.CopyBuffer(io.MultiWriter(hFull, hPre), head, buf); err != nil {
		return "", "", err
	}
	pre = fmt.Sprintf("%X", hPre.Sum(nil))

	if _, err := io.CopyBuffer(hFull, f, buf); err != nil {
		return "", "", err
	}
	full = fmt.Sprintf("%X", hFull.Sum(nil))
	return full, pre, nil
}

// FileSHA1Partial 计算文件 [start, end] 闭区间字节的 SHA1（秒传二次校验用）。
func FileSHA1Partial(filePath string, start, end int64) string {
	f, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // 只读文件，关闭失败无补救动作
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	h := sha1.New()
	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	buf := *bufPtr
	if _, err := io.CopyBuffer(h, io.LimitReader(f, end-start+1), buf); err != nil {
		return ""
	}
	return fmt.Sprintf("%X", h.Sum(nil))
}
