package web

import (
	"net/http"

	"github.com/ytx-zhang/115tools/internal/version"
)

// handleVersion 暴露当前版本号（公开探针，无需鉴权），供前端展示与运维探活。
func handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version.Version})
}
