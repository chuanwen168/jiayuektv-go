// Package admin 实现「曲库管理」管理员鉴权：
// 首次使用时设置密码（sha256 哈希存 settings 表），之后每次登录签发内存
// session token 并通过 httpOnly cookie 下发，与 Node.js 版逻辑完全一致。
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"

	"ktvhome/internal/db"
	"ktvhome/internal/logger"
)

const (
	AdminPasswordKey   = "admin_password_hash"
	AdminSessionCookie = "ktv_admin_session"
	sessionMaxAgeSecs  = 7 * 24 * 60 * 60
	minPasswordLen     = 4
)

// Admin 持有内存会话集合，方法并发安全。
type Admin struct {
	mu       sync.Mutex
	sessions map[string]struct{}
}

func New() *Admin {
	return &Admin{sessions: make(map[string]struct{})}
}

func sha256Hex(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// hashesMatch 常数时间比较两个 64 位十六进制哈希，避免时序侧信道。
func hashesMatch(a, b string) bool {
	if len(a) != 64 || len(b) != 64 {
		return false
	}
	// 统一补齐为相同长度，防止长度差异泄露。
	pad := func(s string) []byte {
		buf := make([]byte, 64)
		copy(buf, []byte(s))
		return buf
	}
	return subtle.ConstantTimeCompare(pad(a), pad(b)) == 1
}

func (a *Admin) getPasswordHash() (string, bool) {
	v, ok, err := db.GetSetting(AdminPasswordKey)
	if err != nil {
		logger.Error("ADMIN", "读取管理员密码哈希失败: "+err.Error())
		return "", false
	}
	return v, ok
}

func (a *Admin) setPasswordHash(hash string) error {
	return db.SetSetting(AdminPasswordKey, hash)
}

// TokenFromRequest 从请求 Cookie 中取出会话 token。
func (a *Admin) TokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(AdminSessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// IsAuthed 判断当前请求是否已登录。
func (a *Admin) IsAuthed(r *http.Request) bool {
	token := a.TokenFromRequest(r)
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.sessions[token]
	return ok
}

// RequireAuth 是保护管理操作（编辑/删除歌曲、改密码）的中间件。
func (a *Admin) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.IsAuthed(r) {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请先登录管理员账号"})
	}
}

func (a *Admin) startSession(w http.ResponseWriter) string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	a.mu.Lock()
	a.sessions[token] = struct{}{}
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     AdminSessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   sessionMaxAgeSecs,
	})
	return token
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     AdminSessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// EnsureInitialPassword 对应 1.2.0 的 ADMIN_PASSWORD 预置密码：
// 仅当管理员密码从未设置过时，用环境变量指定的明文初始化；
// 已设置过则忽略（避免覆盖管理员已改过的密码）。返回是否本次设置了新密码。
func (a *Admin) EnsureInitialPassword(plain string) bool {
	if plain == "" {
		return false
	}
	if _, set := a.getPasswordHash(); set {
		logger.Warn("ADMIN", "管理员密码已设置过，忽略 ADMIN_PASSWORD 环境变量")
		return false
	}
	if len(plain) < minPasswordLen {
		logger.Warn("ADMIN", "ADMIN_PASSWORD 太短（至少 4 位），已忽略，请改用首次设置流程")
		return false
	}
	if err := a.setPasswordHash(sha256Hex(plain)); err != nil {
		logger.Error("ADMIN", "用 ADMIN_PASSWORD 初始化管理员密码失败: "+err.Error())
		return false
	}
	logger.Info("ADMIN", "已用 ADMIN_PASSWORD 环境变量初始化管理员密码")
	return true
}

// SessionStatus 对应 GET /api/admin/session。
func (a *Admin) SessionStatus(w http.ResponseWriter, r *http.Request) {
	_, passwordSet := a.getPasswordHash()
	writeJSON(w, http.StatusOK, map[string]bool{
		"authed":      a.IsAuthed(r),
		"passwordSet": passwordSet,
	})
}

// Setup 对应 POST /api/admin/setup（首次设置密码，仅一次）。
func (a *Admin) Setup(w http.ResponseWriter, r *http.Request) {
	if _, set := a.getPasswordHash(); set {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "管理员密码已设置过，请使用登录"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if len(body.Password) < minPasswordLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "密码至少 4 位"})
		return
	}
	if err := a.setPasswordHash(sha256Hex(body.Password)); err != nil {
		logger.Error("ADMIN", "保存管理员密码失败: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败"})
		return
	}
	a.startSession(w)
	logger.Info("ADMIN", "首次设置曲库管理密码成功")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Login 对应 POST /api/admin/login。
func (a *Admin) Login(w http.ResponseWriter, r *http.Request) {
	stored, ok := a.getPasswordHash()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "尚未设置管理员密码，请先设置"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	inputHash := sha256Hex(body.Password)
	if !hashesMatch(inputHash, stored) {
		logger.Warn("ADMIN", "曲库管理登录失败：密码错误")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误"})
		return
	}
	a.startSession(w)
	logger.Info("ADMIN", "曲库管理登录成功")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Logout 对应 POST /api/admin/logout。
func (a *Admin) Logout(w http.ResponseWriter, r *http.Request) {
	token := a.TokenFromRequest(r)
	if token != "" {
		a.mu.Lock()
		delete(a.sessions, token)
		a.mu.Unlock()
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ChangePassword 对应 POST /api/admin/change-password（需登录）。
func (a *Admin) ChangePassword(w http.ResponseWriter, r *http.Request) {
	stored, ok := a.getPasswordHash()
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "当前密码不正确"})
		return
	}
	var body struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	oldHash := sha256Hex(body.OldPassword)
	if !hashesMatch(oldHash, stored) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "当前密码不正确"})
		return
	}
	if len(body.NewPassword) < minPasswordLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新密码至少 4 位"})
		return
	}
	if err := a.setPasswordHash(sha256Hex(body.NewPassword)); err != nil {
		logger.Error("ADMIN", "保存新密码失败: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败"})
		return
	}

	// 安全起见，使其它所有已登录 session 失效，仅保留当前会话。
	token := a.TokenFromRequest(r)
	a.mu.Lock()
	a.sessions = make(map[string]struct{})
	if token != "" {
		a.sessions[token] = struct{}{}
	}
	a.mu.Unlock()

	logger.Info("ADMIN", "曲库管理密码已修改")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeJSON 便捷 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
