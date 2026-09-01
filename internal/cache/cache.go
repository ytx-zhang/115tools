// Package cache 是透传本地缓存层：上传完成的视频按 pickcode 分目录暂存
// （<dir>/<pickcode>/<原名>），供 /download 透传在保留期内直读本地、跳过 115 上游回源。
//
// 缓存本质是「副本」：移动失败不影响云端已存视频与 .strm。保留期由配置控制，到期由清理协程回收。
package cache

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// SweepInterval 清理扫描周期（默认 1 小时）。
const SweepInterval = time.Hour

// Cache 本地透传缓存层（dir 固定，retention 可热更新）。
type Cache struct {
	dir       string
	mu        sync.RWMutex
	retention time.Duration
}

// New 构造缓存层。dir 为缓存根目录（组合根负责 MkdirAll）。
func New(dir string, retention time.Duration) *Cache {
	return &Cache{dir: dir, retention: retention}
}

// SetRetention 热更新保留期（并发安全）。
func (c *Cache) SetRetention(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retention = d
}

// SetDir 热更新缓存根目录（全局设置 cache_dir 变更后由 webui 调用），并确保目录存在。
func (c *Cache) SetDir(dir string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	c.dir = dir
	return nil
}

// cacheDir 返回缓存根目录（读锁收口，SetDir 热更新时并发安全）。
func (c *Cache) cacheDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dir
}

func (c *Cache) retentionLocked() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.retention
}

// LocalPath 返回 pickcode 对应的本地缓存文件路径。命中返回 (path, true)。
func (c *Cache) LocalPath(pickCode string) (string, bool) {
	if !validPickCode(pickCode) {
		return "", false
	}
	dir := filepath.Join(c.cacheDir(), pickCode)
	name, _, ok := firstFileEntry(dir)
	if !ok {
		return "", false
	}
	return filepath.Join(dir, name), true
}

// Item 缓存条目（供 WebUI 展示）。
type Item struct {
	PickCode  string    `json:"pickcode"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CachedAt  time.Time `json:"cached_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// List 枚举全部缓存项（按文件名升序）。
func (c *Cache) List() []Item {
	var items []Item
	dir := c.cacheDir()
	if dir == "" {
		return items
	}
	retention := c.retentionLocked()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return items
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pcDir := filepath.Join(dir, e.Name())
		name, size, ok := firstFileEntry(pcDir)
		if !ok {
			continue
		}
		cachedAt := time.Now()
		if info, serr := e.Info(); serr == nil {
			cachedAt = info.ModTime()
		}
		items = append(items, Item{
			PickCode:  e.Name(),
			Name:      name,
			Size:      size,
			CachedAt:  cachedAt,
			ExpiresAt: cachedAt.Add(retention),
		})
	}
	slices.SortFunc(items, func(a, b Item) int { return strings.Compare(a.Name, b.Name) })
	return items
}

// Move 把已上传的视频原件移入缓存（<dir>/<pickcode>/<原名>），返回落盘路径。
// 优先同盘原子 rename；跨设备（EXDEV）退化为流式拷贝后删除原件。
func (c *Cache) Move(srcPath, pickCode string) (string, error) {
	if !validPickCode(pickCode) {
		return "", os.ErrInvalid
	}
	dstDir := filepath.Join(c.cacheDir(), pickCode)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dstDir, filepath.Base(srcPath))
	if err := os.Rename(srcPath, dst); err == nil {
		slog.InfoContext(context.Background(), "视频移入本地缓存", "缓存路径", dst)
		return dst, nil
	}
	if err := copyFile(srcPath, dst); err != nil {
		return "", err
	}
	if rerr := os.Remove(srcPath); rerr != nil && !os.IsNotExist(rerr) {
		slog.WarnContext(context.Background(), "缓存拷贝成功但删除原件失败", "路径", srcPath, "错误", rerr)
	}
	slog.InfoContext(context.Background(), "视频移入本地缓存(跨设备拷贝)", "缓存路径", dst)
	return dst, nil
}

// Delete 批量删除指定 pickcode 的缓存项，返回实际删除数。
func (c *Cache) Delete(pickCodes []string) int {
	deleted := 0
	for _, pc := range pickCodes {
		if !validPickCode(pc) {
			continue
		}
		dir := filepath.Join(c.cacheDir(), pc)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.WarnContext(context.Background(), "手动删除缓存失败", "pickcode", pc, "错误", err)
			continue
		}
		deleted++
	}
	return deleted
}

// StartCleaner 周期清理超过 retention 的缓存目录（绑定 ctx 生命周期）。
func (c *Cache) StartCleaner(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	slog.InfoContext(ctx, "本地缓存清理协程启动", "缓存路径", c.cacheDir(), "保留期", c.retentionLocked().Round(time.Minute).String())
	c.sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweep()
		}
	}
}

// sweep 删除所有过期缓存项。
func (c *Cache) sweep() {
	dir := c.cacheDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-c.retentionLocked())
	for _, e := range entries {
		pcDir := filepath.Join(dir, e.Name())
		if !e.IsDir() {
			if rerr := os.Remove(pcDir); rerr != nil && !os.IsNotExist(rerr) {
				slog.DebugContext(context.Background(), "清理缓存散落文件失败", "路径", pcDir, "错误", rerr)
			}
			continue
		}
		info, serr := e.Info()
		if serr != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if rerr := os.RemoveAll(pcDir); rerr != nil && !os.IsNotExist(rerr) {
				slog.WarnContext(context.Background(), "清理过期缓存目录失败", "路径", pcDir, "错误", rerr)
			}
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); cerr != nil {
			slog.DebugContext(context.Background(), "关闭源文件失败", "错误", cerr)
		}
	}()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil {
			slog.DebugContext(context.Background(), "关闭目标文件失败", "错误", cerr)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		if rerr := os.Remove(dst); rerr != nil && !os.IsNotExist(rerr) {
			slog.DebugContext(context.Background(), "清理拷贝失败残留失败", "路径", dst, "错误", rerr)
		}
		return err
	}
	return nil
}

// firstFileEntry 返回目录内第一个非目录文件（名称 + 大小）；无则 ok=false。
func firstFileEntry(dir string) (name string, size int64, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, serr := e.Info(); serr == nil {
			size = info.Size()
		}
		return e.Name(), size, true
	}
	return "", 0, false
}

// validPickCode 校验 pickcode 可作缓存目录名（防路径穿越）。
func validPickCode(pickCode string) bool {
	if pickCode == "" || pickCode == "." || pickCode == ".." {
		return false
	}
	return !strings.ContainsAny(pickCode, "/\\")
}
