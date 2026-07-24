package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "tools115_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// sessionStore 内存会话表：token → 过期时间。重启后需重新登录。
type sessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

func newSessionStore() sessionStore {
	return sessionStore{tokens: make(map[string]time.Time)}
}

// create 生成随机 token 并顺带清理过期会话。
func (s *sessionStore) create() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	maps.DeleteFunc(s.tokens, func(_ string, exp time.Time) bool { return now.After(exp) })
	s.tokens[token] = now.Add(sessionTTL)
	return token
}

func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.tokens, token)
		return false
	}
	return true
}

func (s *sessionStore) remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

// authRequired 是否启用了登录验证（配置了用户名即启用）。
func (s *Server) authRequired() bool {
	user, _ := s.Cfg.GetAuth()
	return user != ""
}

// loggedIn 当前请求是否持有效会话。
func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.sessions.valid(c.Value)
}

// protect 鉴权中间件：未启用验证时直接放行；否则要求有效会话。
// /download 不经过本中间件（在 main 中直接注册，供 Emby 使用）。
func (s *Server) protect(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authRequired() && !s.loggedIn(r) {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next(w, r)
	})
}

// handleMe 会话探测：前端据此决定显示登录页还是主界面。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"auth_required": s.authRequired(),
		"logged_in":     !s.authRequired() || s.loggedIn(r),
	})
}

// handleLogin 账号密码登录，成功后签发会话 Cookie。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	user, passHash := s.Cfg.GetAuth()
	if user == "" { // 未启用验证
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(user)) == 1
	// 无论用户名是否匹配，都对存储的哈希执行一次 bcrypt 比对，
	// 避免「用户名是否存在」被时间差侧信道探测到。
	passOK := bcrypt.CompareHashAndPassword([]byte(passHash), []byte(req.Password)) == nil
	if !userOK || !passOK {
		time.Sleep(500 * time.Millisecond) // 减缓暴力破解
		slog.Warn("[WEB] 登录失败", "用户名", req.Username, "来源", clientIP(r))
		writeErr(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.sessions.create(),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	slog.Info("[WEB] 登录成功", "用户名", req.Username, "来源", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLogout 注销当前会话。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.remove(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
