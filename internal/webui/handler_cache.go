package webui

import (
	"net/http"

	"github.com/ytx-zhang/115tools/internal/cache"
)

// handleCacheList 返回全部缓存条目 + 汇总。
func (s *Server) handleCacheList(w http.ResponseWriter, _ *http.Request) {
	var items []cache.Item
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

// handleCacheDelete 批量删除缓存项。
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
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}
