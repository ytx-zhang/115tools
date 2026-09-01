package webui

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// handleFS 列出容器内某个目录下的子目录（本地目录输入框的「浏览」按钮用）。
//
// 只列目录、不列文件，也不读任何文件内容；受登录保护，因此暴露文件系统目录结构是可接受的。
func (s *Server) handleFS(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = "/"
	}
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		writeErr(w, http.StatusBadRequest, "必须是绝对路径: %s", p)
		return
	}
	if _, err := os.Stat(clean); err != nil {
		writeErr(w, http.StatusBadRequest, "目录不存在: %v", err)
		return
	}

	entries, err := os.ReadDir(clean)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取目录失败: %v", err)
		return
	}
	type dir struct {
		Name string `json:"name"`
	}
	out := make([]dir, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, dir{Name: e.Name()})
	}
	slices.SortFunc(out, func(a, b dir) int { return strings.Compare(a.Name, b.Name) })

	parent := filepath.Dir(clean)
	if parent == clean {
		parent = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":   clean,
		"parent": parent,
		"dirs":   out,
	})
}
