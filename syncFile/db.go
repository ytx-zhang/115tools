package syncFile

import (
	"bytes"
	"log"
	"os"
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

func initDB() {
	var err error
	boltDB, err = bbolt.Open(dBPath, 0600, nil)
	if err != nil {
		log.Fatalf("[数据库] 初始化失败: %v", err)
	}
	boltDB.MaxBatchDelay = 100 * time.Millisecond
	boltDB.MaxBatchSize = 2000
	boltDB.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
}

func closeDB() {
	if boltDB != nil {
		log.Printf("[数据库] 正在关闭 bbolt...")
		boltDB.Close()
	}
}

func removeDB() {
	if err := os.Remove(dBPath); err != nil {
		log.Printf("[同步] 清理数据库失败: %v", err)
	} else {
		log.Printf("[同步] 已清理数据库文件")
	}
}

func dbGetFID(localPath string) string {
	var fid string
	boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		v := b.Get([]byte(localPath))
		if v == nil {
			return nil
		}
		s := string(v)
		if before, _, ok := strings.Cut(s, "|"); ok {
			fid = before
		}
		return nil
	})
	return fid
}

func dbSaveRecord(localPath string, fid string, size int64) {
	val := fid + "|" + strconv.FormatInt(size, 10)

	boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Put([]byte(localPath), []byte(val))
	})
}

func dbListChildren(currentLocalPath string, res map[string]string) {
	prefix := currentLocalPath + "/"
	prefixBytes := []byte(prefix)

	boltDB.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketName).Cursor()
		for k, v := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, v = c.Next() {
			remain := k[len(prefixBytes):]
			if !bytes.Contains(remain, []byte("/")) {
				res[string(remain)] = string(v)
			}
		}
		return nil
	})
}

func dbClearPath(fPath string) {
	prefix := fPath + "/"
	prefixBytes := []byte(prefix)

	err := boltDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		b.Delete([]byte(fPath))

		c := b.Cursor()
		for k, _ := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); {
			if err := c.Delete(); err != nil {
				return err
			}
			k, _ = c.Next()
		}
		return nil
	})

	if err != nil {
		log.Printf("[数据库] 清理路径失败 %s: %v", fPath, err)
	}
}

func dbGetTotalCount(parentPath string) int {
	count := 0
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
	return count
}
