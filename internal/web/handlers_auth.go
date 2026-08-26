package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
	"golang.org/x/crypto/bcrypt"
)

// ──── 会话管理（HTTP 层特有）────

const (
	sessionCookie = "tools115_session"
	sessionTTL    = 7 * 24 * time.Hour
)

type sessionStore struct {
	// sync.Map：token → time.Time（过期时刻）。登录写入低频、校验读取高频，
	// 正是 sync.Map 目标场景（读多写少、key 不相交），无需加锁。
	tokens sync.Map
}

func (s *sessionStore) create() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 几乎不会失败（内部自动兜底）；真失败时 token 退化为零值，
		// 生成的会话不可用即被拒绝，此处仅告警。
		logs.Error(logs.ModuleSystem, "生成会话令牌失败", "错误", err)
	}
	token := hex.EncodeToString(buf)
	now := time.Now()
	// 惰性清理过期会话（登录低频，Range 全扫成本可忽略）
	s.tokens.Range(func(k, v any) bool {
		if now.After(v.(time.Time)) {
			s.tokens.Delete(k)
		}
		return true
	})
	s.tokens.Store(token, now.Add(sessionTTL))
	return token
}

func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	v, ok := s.tokens.Load(token)
	if !ok {
		return false
	}
	if time.Now().After(v.(time.Time)) {
		s.tokens.Delete(token)
		return false
	}
	return true
}

func (s *sessionStore) remove(token string) {
	s.tokens.Delete(token)
}

func (s *Server) authRequired() bool {
	return s.App.AuthRequired()
}

func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.sessions.valid(c.Value)
}

func (s *Server) protect(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authRequired() && !s.loggedIn(r) {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next(w, r)
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"auth_required": s.authRequired(),
		"logged_in":     !s.authRequired() || s.loggedIn(r),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	user, passHash := s.App.GetAuth()
	if user == "" {
		writeOK(w, http.StatusOK)
		return
	}

	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(user)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(passHash), []byte(req.Password)) == nil
	if !userOK || !passOK {
		time.Sleep(500 * time.Millisecond)
		logs.Warn(logs.ModuleSystem, "登录失败", "用户名", req.Username, "来源", clientIP(r))
		writeErr(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.sessions.create(), Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	logs.Info(logs.ModuleSystem, "登录成功", "用户名", req.Username, "来源", clientIP(r))
	writeOK(w, http.StatusOK)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.remove(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	writeOK(w, http.StatusOK)
}
