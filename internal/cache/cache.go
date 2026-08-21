// Package cache 实现透传本地缓存层：视频上传完成后不再删除原件，而是按 pickcode 分目录
// 暂存于 <dir>/<pickcode>/<原名>，供 /download 透传在保留期内直读本地、跳过 115 上游回源。
//
// 设计要点：
//   - 缓存目录以 pickcode 命名（用户需求），目录内为上传时的原文件名；透传命中即取该文件。
//   - 保留期由配置 cache_retention_days（默认 1 天）控制，到期后由 Cleaner 周期清理（按 pickcode
//     目录 mtime 判定，该时间即「移入缓存」时刻：MkdirAll + Rename 都会刷新目录 mtime，与原文件内容无关）。
//   - retention 经 SetRetention 支持热更新（配置保存后由 app.ApplyConfig 调用），用 mu 保护并发读写。
//   - 缓存本质是「副本」：即使移动失败（跨设备拷贝失败）也不影响云端已存的视频与 .strm，
//     仅退化为旧行为保留/删除原件，不会丢失数据。
//   - 本包零业务依赖（仅 logs），可被 web（透传）与 sync（上传）共同复用，无循环依赖。
package cache

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// SweepInterval 清理扫描周期（默认 1 小时）：保留期到期最迟延迟一个周期清理。
const SweepInterval = time.Hour

// Cache 本地透传缓存层实例（dir 固定，retention 可经 SetRetention 热更新）。
type Cache struct {
	dir string
	mu  sync.RWMutex
	// retention 保留期：读（sweep）走 RLock，写（SetRetention）走 Lock，避免热更新与清理竞态。
	retention time.Duration
}

// New 构造缓存层。dir 为缓存根目录（由组合根负责 MkdirAll），retention 为保留期。
func New(dir string, retention time.Duration) *Cache {
	return &Cache{dir: dir, retention: retention}
}

// SetRetention 热更新缓存保留期（配置保存后由 app.ApplyConfig 调用），并发安全。
func (c *Cache) SetRetention(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retention = d
}

// retentionLocked 在已持读锁时读取保留期（sweep 调用）；独立成方法避免读锁范围蔓延。
func (c *Cache) retentionLocked() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.retention
}

// Dir 返回缓存根目录（供调试/日志）。
func (c *Cache) Dir() string { return c.dir }

// LocalPath 返回 pickcode 对应的本地缓存文件路径（缓存目录以 pickcode 命名，内含原文件名）。
// 命中返回 (path, true)；目录不存在/为空/越界/出错返回 ("", false)，调用方应回退上游回源。
func (c *Cache) LocalPath(pickCode string) (string, bool) {
	if !validPickCode(pickCode) {
		return "", false
	}
	dir := filepath.Join(c.dir, pickCode)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		return filepath.Join(dir, e.Name()), true
	}
	return "", false
}

// Item 缓存条目（供 WebUI 展示与管理）。
type Item struct {
	PickCode  string    `json:"pickcode"`
	Name      string    `json:"name"`       // 缓存内原文件名（可读性优先，pickcode 无含义）
	Size      int64     `json:"size"`       // 文件字节数
	CachedAt  time.Time `json:"cached_at"`  // 移入缓存时刻（目录 mtime，与清理判定一致）
	ExpiresAt time.Time `json:"expires_at"` // 过期时刻 = CachedAt + 当前保留期（与清理 cutoff 公式一致）
}

// List 枚举全部缓存项，按文件名升序返回（WebUI 默认按文件名排序）。
// 与清理协程共用目录 mtime + 保留期判定，页面显示的过期时间与清理行为一致。
// 单项读取失败跳过不中断；散落的非目录文件不展示（sweep 会直接清理）。
func (c *Cache) List() []Item {
	items := []Item{}
	if c.dir == "" {
		return items
	}
	retention := c.retentionLocked()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		logs.Debug(logs.ModuleSystem, "读取缓存目录失败", "路径", c.dir, "错误", err)
		return items
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pcDir := filepath.Join(c.dir, e.Name())
		info, serr := os.Stat(pcDir)
		if serr != nil {
			continue
		}
		// 目录内首个文件即缓存原件（布局 <dir>/<pickcode>/<原名>），顺带取大小
		name, size := firstFile(pcDir)
		cachedAt := info.ModTime()
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
	dstDir := filepath.Join(c.dir, pickCode)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return "", err
	}
	dst := filepath.Join(dstDir, filepath.Base(srcPath))
	// 同盘：原子 rename（SyncPath 与缓存同文件系统时的常见路径）。
	if err := os.Rename(srcPath, dst); err == nil {
		logs.Info(logs.ModuleSystem, "视频移入本地缓存", "缓存路径", dst)
		return dst, nil
	}
	// 跨设备：拷贝后删原件（缓存本质是副本，失败不会丢云端数据）。
	if err := copyFile(srcPath, dst); err != nil {
		return "", err
	}
	if rerr := os.Remove(srcPath); rerr != nil && !os.IsNotExist(rerr) {
		// 拷贝成功但删原件失败：缓存已有完整副本，仅告警（原件残留会在下次 strm 比对时被忽略）
		logs.Warn(logs.ModuleSystem, "缓存拷贝成功但删除原件失败", "路径", srcPath, "错误", rerr)
	}
	logs.Info(logs.ModuleSystem, "视频移入本地缓存(跨设备拷贝)", "缓存路径", dst)
	return dst, nil
}

// Delete 批量删除指定 pickcode 的缓存项，返回实际删除的数量。
// 与清理协程并发时以 IsNotExist 容错；正在透传播放的文件句柄不受 RemoveAll 影响（Unix），
// 其后续切片会退化为回源，行为可接受，无需额外加锁。
func (c *Cache) Delete(pickCodes []string) int {
	deleted := 0
	for _, pc := range pickCodes {
		if !validPickCode(pc) {
			continue
		}
		dir := filepath.Join(c.dir, pc)
		if _, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				continue // 已不存在（可能刚被清理协程删除），跳过不计数
			}
			logs.Warn(logs.ModuleSystem, "检查待删缓存失败", "pickcode", pc, "错误", err)
			continue
		}
		name := firstFileName(dir)
		if err := os.RemoveAll(dir); err != nil {
			logs.Warn(logs.ModuleSystem, "手动删除缓存失败", "文件名", name, "pickcode", pc, "错误", err)
			continue
		}
		logs.Info(logs.ModuleSystem, "手动删除缓存", "文件名", name)
		deleted++
	}
	return deleted
}

// StartCleaner 周期清理超过 retention 的缓存目录（绑定 ctx 生命周期，ctx 取消即退出）。
func (c *Cache) StartCleaner(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// 启动日志：展示缓存根目录、保留期与当前占用，便于核对清理策略与磁盘影响。
	logs.Info(logs.ModuleSystem, "本地缓存清理协程启动",
		"缓存路径", c.dir,
		"保留期", c.retentionLocked().Round(time.Minute).String(),
		"缓存大小", formatBytes(dirSize(c.dir)))
	c.sweep() // 启动立即扫一次，避免重启后积压过期残留
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweep()
		}
	}
}

// sweep 删除所有过期缓存项：pickcode 目录整体过期则 RemoveAll；散落的非目录文件直接删。
func (c *Cache) sweep() {
	if c.dir == "" {
		return
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		logs.Debug(logs.ModuleSystem, "读取缓存目录失败", "路径", c.dir, "错误", err)
		return
	}
	cutoff := time.Now().Add(-c.retentionLocked())
	removed := 0
	for _, e := range entries {
		pcDir := filepath.Join(c.dir, e.Name())
		if !e.IsDir() {
			// 意外散落文件（非 pickcode 目录）：直接删，避免无限堆积
			if rerr := os.Remove(pcDir); rerr == nil {
				removed++
			} else if !os.IsNotExist(rerr) {
				logs.Debug(logs.ModuleSystem, "清理缓存散落文件失败", "路径", pcDir, "错误", rerr)
			}
			continue
		}
		info, serr := os.Stat(pcDir)
		if serr != nil {
			continue
		}
		// 目录 mtime = 移入缓存时刻（MkdirAll/Rename 刷新），按保留期判定
		if info.ModTime().Before(cutoff) {
			// 展示缓存内原文件名而非 pickcode（pickcode 无可读性，文件名一眼可辨）
			name := firstFileName(pcDir)
			if rerr := os.RemoveAll(pcDir); rerr != nil && !os.IsNotExist(rerr) {
				logs.Warn(logs.ModuleSystem, "清理过期缓存目录失败", "路径", pcDir, "错误", rerr)
			} else {
				logs.Info(logs.ModuleSystem, "清理过期缓存", "文件名", name)
				removed++
			}
		}
	}
	// 每次清理末尾打汇总：清了多少项 + 清理后缓存目录占用（周期性可见，便于观测缓存是否异常膨胀）。
	logs.Info(logs.ModuleSystem, "缓存清理完成", "清理项", removed, "缓存大小", formatBytes(dirSize(c.dir)))
}

// copyFile 流式拷贝 src → dst（用于跨设备移动兜底）。失败清理半成品目标文件。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); cerr != nil {
			logs.Debug(logs.ModuleSystem, "关闭源文件失败", "错误", cerr)
		}
	}()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil {
			logs.Debug(logs.ModuleSystem, "关闭目标文件失败", "错误", cerr)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		if rerr := os.Remove(dst); rerr != nil && !os.IsNotExist(rerr) {
			logs.Debug(logs.ModuleSystem, "清理拷贝失败残留失败", "路径", dst, "错误", rerr)
		}
		return err
	}
	return nil
}

// dirSize 递归统计目录总字节数（供启动/清理日志展示缓存占用）。
// 单项读取失败不中断统计（返回 nil 继续走）；根级失败返回 0。
func dirSize(root string) int64 {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单项不可读不阻塞整体统计
		}
		if d.IsDir() {
			return nil
		}
		if info, serr := d.Info(); serr == nil {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		logs.Debug(logs.ModuleSystem, "统计缓存目录大小失败", "路径", root, "错误", err)
		return 0
	}
	return total
}

// firstFileName 读取缓存目录内第一个文件名（缓存布局 <dir>/<pickcode>/<原文件名>，
// 目录为空或读取失败返回 ""），供清理日志展示可读文件名。
func firstFileName(dir string) string {
	name, _ := firstFile(dir)
	return name
}

// firstFile 返回缓存目录内第一个文件名及其大小（目录为空或读取失败返回 ("", 0)），
// 供 List 枚举缓存条目时复用同一次 ReadDir 取到文件名与大小。
func firstFile(dir string) (string, int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		size := int64(0)
		if info, serr := e.Info(); serr == nil {
			size = info.Size()
		}
		return e.Name(), size
	}
	return "", 0
}

// formatBytes 人类可读字节数（B/KB/MB/GB，1024 进制），供日志展示。
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// validPickCode 校验 pickcode 可作缓存目录名（防路径穿越）：非空、不含路径分隔符、非 "."/".."。
func validPickCode(pickCode string) bool {
	if pickCode == "" {
		return false
	}
	if strings.ContainsAny(pickCode, "/\\") {
		return false
	}
	if pickCode == "." || pickCode == ".." {
		return false
	}
	return true
}
