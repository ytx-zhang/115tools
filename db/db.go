package db

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

var (
	boltDB     *bbolt.DB
	bucketName = []byte("FileIndex")
	dBPath     = `/app/data/files.db`
)

// Init 初始化数据库连接并创建 Bucket
func Init() {
	var err error
	boltDB, err = bbolt.Open(dBPath, 0600, nil)
	if err != nil {
		slog.Error("[数据库] 初始化失败", "错误信息", err)
		return
	}
	// 优化高频写入性能
	boltDB.MaxBatchDelay = 100 * time.Millisecond
	boltDB.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
}

// Close 关闭数据库
func Close() {
	if boltDB != nil {
		if err := boltDB.Close(); err != nil {
			slog.Error("[数据库] 关闭失败", "错误信息", err)
		}
	}
}

// GetInfo 获取单个路径的信息
func GetInfo(localPath string) (fid string, size int64) {
	size = -2
	boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		v := b.Get([]byte(localPath))
		if v == nil {
			return nil
		}
		s := string(v)
		if before, after, ok := strings.Cut(s, "|"); ok {
			fid = before
			size, _ = strconv.ParseInt(after, 10, 64)
		}
		return nil
	})
	return
}

// GetFid 快捷获取文件 FID
func GetFid(localPath string) (fid string) {
	fid, _ = GetInfo(localPath)
	return
}

// SaveRecord 使用 Batch 批量保存记录
func SaveRecord(localPath string, fid string, size int64) {
	val := fid + "|" + strconv.FormatInt(size, 10)
	boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Put([]byte(localPath), []byte(val))
	})
}

func BatchClearPaths(fPaths []string) {
	if len(fPaths) == 0 {
		return
	}
	err := boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}
		for _, fPath := range fPaths {
			selfBytes := []byte(fPath)
			val := b.Get(selfBytes)
			if val != nil {
				idx := bytes.LastIndexByte(val, '|')
				isDir := false
				if idx != -1 {
					sizeBytes := val[idx+1:]
					if len(sizeBytes) == 2 && sizeBytes[0] == '-' && sizeBytes[1] == '1' {
						isDir = true
					}
				}
				if !isDir {
					if err := b.Delete(selfBytes); err != nil {
						return err
					}
					continue
				}
			}
			c := b.Cursor()
			childPrefix := make([]byte, len(selfBytes)+1)
			copy(childPrefix, selfBytes)
			childPrefix[len(selfBytes)] = '/'

			for k, _ := c.Seek(selfBytes); k != nil; {
				if bytes.Equal(k, selfBytes) || bytes.HasPrefix(k, childPrefix) {
					if err := c.Delete(); err != nil {
						return err
					}
					k, _ = c.Seek(k)
				} else {
					break
				}
			}
		}
		return nil
	})

	if err != nil {
		slog.Error("[数据库] 批量清理失败", "数量", len(fPaths), "错误信息", err)
	}
}

// GetTotalCount 统计子项总数 (仅限统计，不建议在高频逻辑中使用)
func GetTotalCount(parentPath string) (count int64) {
	prefix := parentPath
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	prefixBytes := []byte(prefix)
	boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}
		cursor := b.Cursor()
		for k, _ := cursor.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, _ = cursor.Next() {
			count++
		}
		return nil
	})
	return
}

func ScanChildren(ctx context.Context, parentPath string, handler func(name string, valStr string)) {
	if err := ctx.Err(); err != nil {
		return
	}
	prefix := parentPath
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	prefixBytes := []byte(prefix)

	boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}

		c := b.Cursor()
		// 初始定位到前缀起始位置
		k, v := c.Seek(prefixBytes)
		for k != nil && bytes.HasPrefix(k, prefixBytes) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			// 计算相对路径的字节切片（零内存分配）
			relBytes := k[len(prefixBytes):]

			// 查找是否存在子目录分隔符 '/' (byte 47)
			if slashIdx := bytes.IndexByte(relBytes, '/'); slashIdx != -1 {
				// 发现非直属子条目（属于更深层目录）
				// 计算该子目录的完整字节前缀：父目录 + 第一层子目录名 + '/'
				subDirPrefixLen := len(prefixBytes) + slashIdx + 1
				subDirPrefix := k[:subDirPrefixLen]

				// 构造跳转目标：subDirPrefix + 0xFF
				// 这里必须 copy 一份，因为 k 的内存由 bbolt 事务管理，不可直接修改
				jumpTarget := make([]byte, len(subDirPrefix)+1)
				copy(jumpTarget, subDirPrefix)
				jumpTarget[len(subDirPrefix)] = 0xff

				// 性能飞跃点：直接跳过子目录内所有文件
				k, v = c.Seek(jumpTarget)
				continue
			}

			// 如果是直属条目，则转换为 handler 需要的字符串
			handler(string(relBytes), string(v))

			// 继续看下一个条目
			k, v = c.Next()
		}
		return nil
	})
}
