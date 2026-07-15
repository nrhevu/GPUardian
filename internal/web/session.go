package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
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

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type sessionPayload struct {
	User    string `json:"user"`
	Role    string `json:"role"`
	Expires int64  `json:"expires"`
}

type sessionInfo struct {
	User string
	Role string
}

type sessionContextKey struct{}

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
	user, err := s.Users.Authenticate(req.Username, req.Password)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "Invalid username or password")
		return
	}
	expires := time.Now().Add(sessionTTL)
	http.SetCookie(w, s.sessionCookie(r, s.signSession(user.Username, user.Role, expires), expires))
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          user.Username,
		"role":          user.Role,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := s.Users.Create(req.Username, req.Password, RoleUser)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	expires := time.Now().Add(sessionTTL)
	http.SetCookie(w, s.sessionCookie(r, s.signSession(user.Username, user.Role, expires), expires))
	writeJSON(w, http.StatusCreated, map[string]any{
		"authenticated": true,
		"user":          user.Username,
		"role":          user.Role,
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	session, ok := s.sessionUser(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": ok,
		"user":          session.User,
		"role":          session.Role,
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

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	session, _ := currentSession(r)
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := s.Users.ChangePassword(session.User, req.CurrentPassword, req.NewPassword)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"user": user.Username,
		"role": user.Role,
	})
}

func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := s.sessionUser(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, session)))
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		session, _ := currentSession(r)
		if session.Role != RoleAdmin {
			writeJSONError(w, http.StatusForbidden, "admin access required")
			return
		}
		next(w, r)
	})
}

func currentSession(r *http.Request) (sessionInfo, bool) {
	session, ok := r.Context().Value(sessionContextKey{}).(sessionInfo)
	return session, ok
}

func (s *Server) sessionUser(r *http.Request) (sessionInfo, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return sessionInfo{}, false
	}
	payload, ok := s.verifySession(cookie.Value)
	if !ok || time.Now().Unix() > payload.Expires {
		return sessionInfo{}, false
	}
	user, found, err := s.Users.Get(payload.User)
	if err != nil || !found {
		return sessionInfo{}, false
	}
	return sessionInfo{User: user.Username, Role: user.Role}, true
}

func (s *Server) signSession(user, role string, expires time.Time) string {
	payload := sessionPayload{User: user, Role: role, Expires: expires.Unix()}
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
