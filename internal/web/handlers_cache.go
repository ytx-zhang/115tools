package web

import (
	"net/http"

	"github.com/ytx-zhang/115tools/internal/cache"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// ──── 透传本地缓存管理 ────

// handleCacheList 返回全部缓存条目（文件名升序）+ 汇总（条目数、总占用）。
// Cache 为 nil（未启用本地缓存）时返回空列表，前端照常渲染空态。
func (s *Server) handleCacheList(w http.ResponseWriter, r *http.Request) {
	items := []cache.Item{}
	total := int64(0)
	if s.Cache != nil {
		items = s.Cache.List()
		for _, it := range items {
			total += it.Size
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"count":      len(items),
		"total_size": total,
	})
}

// handleCacheDelete 批量删除指定 pickcode 的缓存项，返回实际删除数。
func (s *Server) handleCacheDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PickCodes []string `json:"pickcodes"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if len(req.PickCodes) == 0 {
		writeErr(w, http.StatusBadRequest, "未指定要删除的缓存项")
		return
	}
	deleted := 0
	if s.Cache != nil {
		deleted = s.Cache.Delete(req.PickCodes)
	}
	logs.Info(logs.ModuleSystem, "手动删除缓存完成", "请求", len(req.PickCodes), "删除", deleted)
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}
