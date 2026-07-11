package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	sessionCookieName = "rocguard_session"
	sessionTTL        = 24 * time.Hour
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type sessionPayload struct {
	User    string `json:"user"`
	Expires int64  `json:"expires"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !constantEqual(strings.TrimSpace(req.Username), s.Cfg.WebUser) || !constantEqual(req.Password, s.Cfg.WebPassword) {
		writeJSONError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	expires := time.Now().Add(sessionTTL)
	http.SetCookie(w, s.sessionCookie(r, s.signSession(s.Cfg.WebUser, expires), expires))
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          s.Cfg.WebUser,
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, ok := s.sessionUser(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": ok,
		"user":          user,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	http.SetCookie(w, s.clearSessionCookie(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.sessionUser(r); !ok {
			writeJSONError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

func (s *Server) sessionUser(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	payload, ok := s.verifySession(cookie.Value)
	if !ok || payload.User != s.Cfg.WebUser || time.Now().Unix() > payload.Expires {
		return "", false
	}
	return payload.User, true
}

func (s *Server) signSession(user string, expires time.Time) string {
	payload := sessionPayload{User: user, Expires: expires.Unix()}
	data, _ := json.Marshal(payload)
	signature := s.sessionSignature(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (s *Server) verifySession(value string) (sessionPayload, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return sessionPayload{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return sessionPayload{}, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return sessionPayload{}, false
	}
	if !hmac.Equal(signature, s.sessionSignature(data)) {
		return sessionPayload{}, false
	}
	var payload sessionPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return sessionPayload{}, false
	}
	return payload, true
}

func (s *Server) sessionSignature(data []byte) []byte {
	mac := hmac.New(sha256.New, []byte(s.Cfg.WebUser+"\x00"+s.Cfg.WebPassword))
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func (s *Server) sessionCookie(r *http.Request, value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	}
}

func (s *Server) clearSessionCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	}
}

func constantEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
