package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// 鉴权实现说明（依据 API-DETAILS.md §3）：
//   - token 存 auth_config 表，key='token_hash'，值为 SHA-256 十六进制（无盐无迭代）
//   - enabled 存 auth_config 表，key='enabled'，'1' 表示开启
//   - cookie 名 neko-session，值为明文 token（未签名，与上游一致）
//   - 但 WS 侧可能拿到 URL 编码过的值，需要 decodeURIComponent 重试

const sessionCookie = "neko-session"

// sha256Hex 计算 SHA-256 十六进制摘要。
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// authEnabled 读取访问控制开关。
func (s *Server) authEnabled() (bool, bool) {
	var v string
	err := s.db.SQL().QueryRow(`SELECT value FROM auth_config WHERE key = 'enabled'`).Scan(&v)
	if err != nil {
		return false, false
	}
	return v == "1", true
}

// tokenHash 读取已存的 token 摘要。
func (s *Server) tokenHash() string {
	var v string
	_ = s.db.SQL().QueryRow(`SELECT value FROM auth_config WHERE key = 'token_hash'`).Scan(&v)
	return v
}

// verifyToken 校验 token 是否与已存摘要匹配。
func (s *Server) verifyToken(tok string) bool {
	if tok == "" {
		return false
	}
	stored := s.tokenHash()
	if stored == "" {
		return false
	}
	if sha256Hex(tok) == stored {
		return true
	}
	// 兼容 URL 编码过的值（WS 鉴权读到原始 Cookie 头时会带 %xx）
	if dec := urlDecode(tok); dec != tok && sha256Hex(dec) == stored {
		return true
	}
	return false
}

// urlDecode 做一次 URL 解码；失败则原样返回。
func urlDecode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := fromHex(s[i+1])
			lo, ok2 := fromHex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		if s[i] == '+' {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func fromHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// validToken 校验新 token 规则：≥6 字符且同时含字母和数字。
func validToken(tok string) bool {
	if len(tok) < 6 {
		return false
	}
	var hasLetter, hasDigit bool
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}

// setAuthConfig 写一条 auth_config。
func (s *Server) setAuthConfig(key, value string) error {
	_, err := s.db.SQL().Exec(`
		INSERT INTO auth_config (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		key, value)
	return err
}

// ---------------------------------------------------------------- 路由

func (s *Server) registerAuth() {
	s.mux.HandleFunc("GET /api/auth/state", s.handleAuthState)
	s.mux.HandleFunc("POST /api/auth/enable", s.handleAuthEnable)
	s.mux.HandleFunc("POST /api/auth/disable", s.handleAuthDisable)
	s.mux.HandleFunc("POST /api/auth/verify", s.handleAuthVerify)
	s.mux.HandleFunc("PUT /api/auth/token", s.handleAuthToken)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
}

// handleAuthState 返回 { enabled, hasToken }。
func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	enabled := false
	if v, ok := s.authEnabled(); ok {
		enabled = v
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  enabled,
		"hasToken": s.tokenHash() != "",
	})
}

type tokenReq struct {
	Token string `json:"token"`
}

// handleAuthEnable 开启访问控制。首次开启需同时设置 token。
func (s *Server) handleAuthEnable(w http.ResponseWriter, r *http.Request) {
	var req tokenReq
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.Token != "" {
		if !validToken(req.Token) {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"message": "Token must be at least 6 characters and contain both letters and numbers",
			})
			return
		}
		if err := s.setAuthConfig("token_hash", sha256Hex(req.Token)); err != nil {
			s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": err.Error()})
			return
		}
	} else if s.tokenHash() == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"message": "A token is required to enable access control",
		})
		return
	}

	if err := s.setAuthConfig("enabled", "1"); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": err.Error()})
		return
	}

	if req.Token != "" {
		s.setSessionCookie(w, req.Token)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Access control enabled",
	})
}

// handleAuthDisable 关闭访问控制。
func (s *Server) handleAuthDisable(w http.ResponseWriter, r *http.Request) {
	// 已开启时必须带正确 token
	if enabled, ok := s.authEnabled(); ok && enabled {
		var req tokenReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !s.verifyToken(req.Token) {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"message": "Invalid token",
			})
			return
		}
	}
	if err := s.setAuthConfig("enabled", "0"); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": err.Error()})
		return
	}
	s.clearSessionCookie(w)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Access control disabled",
	})
}

// handleAuthVerify 校验 token 是否有效。
func (s *Server) handleAuthVerify(w http.ResponseWriter, r *http.Request) {
	var req tokenReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	if s.verifyToken(req.Token) {
		s.setSessionCookie(w, req.Token)
		s.writeJSON(w, http.StatusOK, map[string]any{"valid": true})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"valid":   false,
		"message": "Invalid token",
	})
}

type rotateReq struct {
	CurrentToken string `json:"currentToken"`
	NewToken     string `json:"newToken"`
}

// handleAuthToken 更换 token。
func (s *Server) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	var req rotateReq
	_ = json.NewDecoder(r.Body).Decode(&req)

	if enabled, ok := s.authEnabled(); ok && enabled {
		if !s.verifyToken(req.CurrentToken) {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"success": false,
				"message": "Invalid current token",
			})
			return
		}
	}
	if !validToken(req.NewToken) {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"message": "Token must be at least 6 characters and contain both letters and numbers",
		})
		return
	}
	if err := s.setAuthConfig("token_hash", sha256Hex(req.NewToken)); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": err.Error()})
		return
	}
	s.setSessionCookie(w, req.NewToken)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Token updated",
	})
}

// handleAuthLogout 清除会话 cookie。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Logged out",
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   30 * 24 * 3600,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// authMiddleware 对 /api/* 施加访问控制（auth 与 health 除外）。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// 放行白名单
		if s.cfg.ForceAccessControlOff ||
			path == "/health" ||
			path == "/ws" ||
			strings.HasPrefix(path, "/api/auth/") {
			next.ServeHTTP(w, r)
			return
		}

		enabled, ok := s.authEnabled()
		if !ok || !enabled {
			next.ServeHTTP(w, r)
			return
		}

		// 从 cookie 或 Bearer 取 token
		tok := ""
		if ck, err := r.Cookie(sessionCookie); err == nil {
			tok = ck.Value
		}
		if tok == "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				tok = strings.TrimPrefix(h, "Bearer ")
			}
		}
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}

		if !s.verifyToken(tok) {
			s.writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
