// 仅用于前端截图测试的假后端：服务真实静态文件 + 返回示例数据，绝不连接 115。
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const cookieName = "tools115_session"

var (
	mu    sync.Mutex
	tasks = []map[string]any{
		{
			"id": "t1", "name": "媒体库", "enabled": true,
			"local_dir": "/strm媒体库", "cloud_dir": "/媒体库",
			"upload": true, "download": false,
			"running": true, "initializing": false, "queued": false,
			"completed": 87, "total": 120, "current": "/strm媒体库/电影/示例影片.mp4",
			"last_run":  time.Now().Add(-2 * time.Hour).UnixMilli(),
			"next_cron": time.Now().Add(10 * time.Hour).UnixMilli(),
		},
		{
			"id": "t2", "name": "待整理", "enabled": true,
			"local_dir": "/待整理", "cloud_dir": "/待整理",
			"upload": true, "download": true,
			"running": false, "initializing": false, "queued": false,
			"completed": 0, "total": 0, "current": "",
			"last_run":  time.Now().Add(-26 * time.Hour).UnixMilli(),
			"next_cron": time.Now().Add(6 * time.Hour).UnixMilli(),
		},
		{
			"id": "t3", "name": "未开定时", "enabled": false,
			"local_dir": "/未开定时", "cloud_dir": "/未开定时",
			"upload": true, "download": false,
			"running": false, "initializing": false, "queued": false,
			"completed": 0, "total": 0, "current": "",
			"last_run": time.Now().Add(-72 * time.Hour).UnixMilli(),
		},
	}
)

func j(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	staticDir := flag.String("static", "internal/webui/static", "前端静态目录")
	addr := flag.String("addr", ":18080", "监听地址")
	flag.Parse()

	mux := http.NewServeMux()

	idxFile := filepath.Join(*staticDir, "index.html")
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		idx, _ := os.ReadFile(idxFile)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(idx)
	})
	fs := http.FileServer(http.Dir(*staticDir))
	mux.Handle("GET /static/", http.StripPrefix("/static/", fs))

	// 公开
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Username == "root" && req.Password == "9483531436" {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "mock", Path: "/", MaxAge: 7 * 86400, HttpOnly: true})
			j(w, 200, map[string]bool{"ok": true})
			return
		}
		time.Sleep(300 * time.Millisecond)
		j(w, 401, map[string]string{"error": "账号或密码错误"})
	})
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie(cookieName)
		logged := c != nil && c.Value != ""
		j(w, 200, map[string]any{"auth_required": true, "logged_in": logged})
	})
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]string{"version": "0.4.1-mock"})
	})

	// 受保护（仅校验 cookie 存在）
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, _ := r.Cookie(cookieName)
			if c == nil || c.Value == "" {
				j(w, 401, map[string]string{"error": "未登录或会话已过期"})
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /api/overview", guard(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		t := append([]map[string]any{}, tasks...)
		mu.Unlock()
		j(w, 200, map[string]any{"config_ready": true, "missing": []string{}, "init_error": "", "tasks": t})
	}))
	mux.HandleFunc("GET /api/events", guard(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		mu.Lock()
		t := append([]map[string]any{}, tasks...)
		mu.Unlock()
		send := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
		send(map[string]any{"type": "overview", "config_ready": true, "missing": []string{}, "init_error": "", "tasks": t})
		send(map[string]any{"type": "logs", "full": true, "seq": 2, "logs": sampleLogs()})
		hb := time.NewTicker(15 * time.Second)
		defer hb.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-hb.C:
				_, _ = w.Write([]byte(": ping\n\n"))
				flusher.Flush()
			}
		}
	}))
	mux.HandleFunc("GET /api/settings", guard(func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{
			"strm_url": "http://localhost:8080/download", "temp_dir": "/Temp", "cache_dir": "/mnt/cache",
			"cache_retention_days": 1, "offline_dir": "/影视/下载",
			"video_exts": []string{".mp4", ".mkv", ".ts"}, "upload_exclude": []string{".part", ".crdownload"},
			"auth_username": "root", "has_refresh_token": true,
		})
	}))
	mux.HandleFunc("GET /api/tasks", guard(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		t := append([]map[string]any{}, tasks...)
		mu.Unlock()
		j(w, 200, map[string]any{"tasks": t})
	}))
	mux.HandleFunc("POST /api/tasks", guard(func(w http.ResponseWriter, r *http.Request) {
		var t map[string]any
		_ = json.NewDecoder(r.Body).Decode(&t)
		mu.Lock()
		t["id"] = "t" + strconv.Itoa(len(tasks)+1)
		tasks = append(tasks, t)
		mu.Unlock()
		j(w, 201, t)
	}))
	mux.HandleFunc("GET /api/offline/tasks", guard(func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{
			"tasks": []map[string]any{
				{"name": "示例影片.1080p.BluRay.mkv", "size": 5368709120, "percentDone": 63.5, "status": "1", "info_hash": "abc123"},
				{"name": "纪录片.2160p.WEB-DL.mp4", "size": 3221225472, "percentDone": 100, "status": "2", "info_hash": "def456"},
				{"name": "剧集.S01E01.mkv", "size": 1073741824, "percentDone": 12.3, "status": "1", "info_hash": "ghi789"},
				{"name": "旧任务种子", "size": 2147483648, "percentDone": 0, "status": "-1", "info_hash": "jkl000"},
			},
			"page": 0, "page_count": 3, "count": 24,
		})
	}))
	mux.HandleFunc("GET /api/offline/quota", guard(func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{"used": 512, "count": 1000, "surplus": 488})
	}))
	mux.HandleFunc("GET /api/cache", guard(func(w http.ResponseWriter, r *http.Request) {
		items := []map[string]any{
			{"name": "示例影片.mp4", "pickcode": "pc-001", "size": 5368709120, "expires_at": time.Now().Add(20 * 24 * time.Hour).UnixMilli()},
			{"name": "纪录片.mp4", "pickcode": "pc-002", "size": 3221225472, "expires_at": time.Now().Add(1 * 24 * time.Hour).UnixMilli()},
			{"name": "演唱会.ts", "pickcode": "pc-003", "size": 8589934592, "expires_at": time.Now().Add(3 * 24 * time.Hour).UnixMilli()},
		}
		var total int64
		for _, it := range items {
			total += int64(it["size"].(int))
		}
		j(w, 200, map[string]any{"items": items, "count": len(items), "total_size": total})
	}))
	mux.HandleFunc("GET /api/activity", guard(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		j(w, 200, map[string]any{"events": []map[string]any{
			{"state": "success", "scope": "upload", "trigger": "cron", "time": now.Add(-2 * time.Hour).UnixMilli(), "duration_ms": 145000,
				"stats": map[string]any{"uploaded": 8, "strm_generated": 8, "deleted": 0, "failed": 0, "dirs": []string{"/媒体库/电影"}}},
			{"state": "running", "scope": "upload", "trigger": "watch", "time": now.Add(-30 * time.Minute).UnixMilli(), "duration_ms": 0,
				"stats": map[string]any{"uploaded": 2, "strm_generated": 2}},
			{"state": "failed", "scope": "download", "trigger": "manual", "time": now.Add(-5 * time.Hour).UnixMilli(), "duration_ms": 32000,
				"stats": map[string]any{"downloaded": 0, "failed": 1}, "error": "云端目录不存在: /媒体库/缺失"},
		}})
	}))
	mux.HandleFunc("GET /api/logs", guard(func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{"logs": sampleLogs(), "seq": 2})
	}))
	mux.HandleFunc("GET /api/fs", guard(func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{"path": "/", "parent": "", "dirs": []map[string]any{
			{"name": "strm媒体库"}, {"name": "待整理"}, {"name": "影视"}, {"name": "downloads"},
		}})
	}))
	mux.HandleFunc("GET /api/tasks/{id}/dry-run", guard(func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{
			"groups": []map[string]any{
				{"op": 1, "label": "上传本地新增", "count": 3, "danger": false},
				{"op": 4, "label": "生成 STRM", "count": 3, "danger": false},
				{"op": 7, "label": "归档原视频到回收目录", "count": 3, "danger": true},
			},
			"danger": 3,
			"ops": []map[string]any{
				{"label": "上传", "path": "/strm媒体库/电影/示例影片.mp4"},
				{"label": "生成STRM", "path": "/strm媒体库/电影/示例影片.strm"},
				{"label": "归档", "path": "/strm媒体库/电影/示例影片.mp4 → /Temp/示例影片.mp4", "danger": true, "reason": "移入回收目录，可找回"},
			},
		})
	}))

	log.Printf("mock 后端监听 %s（静态目录 %s）", *addr, *staticDir)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func sampleLogs() []map[string]any {
	now := time.Now()
	return []map[string]any{
		{"level": "WARN", "time": now.Add(-40 * time.Minute).UnixMilli(), "msg": "云端目录 '/媒体库/缺失' 不存在，已跳过下载",
			"attrs": []map[string]any{{"key": "task", "value": "媒体库"}}},
		{"level": "ERROR", "time": now.Add(-5 * time.Hour).UnixMilli(), "msg": "下载任务失败",
			"attrs": []map[string]any{{"key": "task", "value": "待整理"}, {"key": "err", "value": "网络超时"}}},
	}
}
