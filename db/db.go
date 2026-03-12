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

func Init() {
	var err error
	boltDB, err = bbolt.Open(dBPath, 0600, nil)
	if err != nil {
		slog.Info("[数据库] 初始化失败", "错误信息", err)
	}
	boltDB.MaxBatchDelay = 100 * time.Millisecond
	boltDB.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
}

func Close() {
	if boltDB != nil {
		if err := boltDB.Close(); err != nil {
			slog.Error("[数据库] 关闭失败", "错误信息", err)
		}
	}
}

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
func GetFid(localPath string) (fid string) {
	fid, _ = GetInfo(localPath)
	return
}

func SaveRecord(localPath string, fid string, size int64) {
	val := fid + "|" + strconv.FormatInt(size, 10)

	boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Put([]byte(localPath), []byte(val))
	})
}

func ClearPath(fPath string) {
	selfBytes := []byte(fPath)
	childPrefix := []byte(fPath + "/")

	err := boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		c := b.Cursor()
		for k, _ := c.Seek(selfBytes); k != nil; {
			if bytes.Equal(k, selfBytes) || bytes.HasPrefix(k, childPrefix) {
				if err := c.Delete(); err != nil {
					return err
				}
				k, _ = c.Next()
			} else {
				break
			}
		}
		return nil
	})

	if err != nil {
		slog.Error("[数据库] 清理路径失败", "路径", fPath, "错误信息", err)
	}
}

func GetTotalCount(parentPath string) (count int64) {
	prefix := parentPath + "/"
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
		for k, v := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, v = c.Next() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			fullPath := string(k)
			relPath := strings.TrimPrefix(fullPath, prefix)
			if strings.Contains(relPath, "/") {
				continue
			}

			handler(relPath, string(v))
		}
		return nil
	})
}
