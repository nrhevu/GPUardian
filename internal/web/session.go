package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName            = "gpuardian_session"
	sessionTTL                   = 24 * time.Hour
	loginWindow                  = time.Minute
	loginFailureLimit            = 5
	loginIPFailureLimit          = 50
	loginUserFailureLimit        = 10
	registrationIPLimit          = 5
	registrationGlobalLimit      = 20
	passwordChangeLimit          = 5
	maxLoginAttemptKeys          = 4096
	registrationGlobalAttemptKey = "registration:global"
)

type loginAttempt struct {
	Failures int
	ResetAt  time.Time
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type registrationRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type sessionPayload struct {
	User              string `json:"user"`
	Role              string `json:"role"`
	CredentialVersion int64  `json:"credential_version"`
	Expires           int64  `json:"expires"`
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
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now()
	ipKey := s.loginIPAttemptKey(r)
	loginKey := loginAttemptKeyForIP(ipKey, req.Username)
	userKey := loginUsernameAttemptKey(req.Username)
	allowed, retryAfter := s.reserveLoginAttempt(loginKey, ipKey, userKey, now)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Round(time.Second)/time.Second))))
		writeJSONError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}
	user, err := s.Users.AuthenticateContext(r.Context(), req.Username, req.Password)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.releaseLoginReservation(loginKey, ipKey, userKey)
		writeJSONError(w, http.StatusRequestTimeout, "authentication request canceled")
		return
	}
	if errors.Is(err, errPasswordWorkBusy) {
		s.releaseLoginReservation(loginKey, ipKey, userKey)
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusTooManyRequests, "authentication is temporarily busy; try again")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "Invalid username or password")
		return
	}
	s.releaseLoginReservation(loginKey, ipKey, userKey)
	expires := time.Now().Add(sessionTTL)
	http.SetCookie(w, s.sessionCookie(r, s.signSession(user.Username, user.Role, expires), expires))
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          user.Username,
		"role":          user.Role,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.WebAllowRegistration {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req registrationRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := normalizeUsername(req.Username); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePassword(req.Password); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ipKey := registrationAttemptKeyForIP(s.loginIPAttemptKey(r))
	allowed, retryAfter := s.reserveRegistrationAttempt(ipKey, time.Now())
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Round(time.Second)/time.Second))))
		writeJSONError(w, http.StatusTooManyRequests, "too many account creation attempts; try again later")
		return
	}
	user, err := s.Users.Create(req.Username, req.Password, RoleUser)
	if errors.Is(err, errPasswordWorkBusy) {
		s.releaseLoginReservation(ipKey, registrationGlobalAttemptKey)
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusTooManyRequests, "account creation is temporarily busy; try again")
		return
	}
	if errors.Is(err, errUsernameInUse) {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, errUserLimitReached) {
		writeJSONError(w, http.StatusServiceUnavailable, "account registration is unavailable")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "unable to create account")
		return
	}
	s.requestManagedKeySync()
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
		"authenticated":        ok,
		"user":                 session.User,
		"role":                 session.Role,
		"registration_enabled": s.Cfg.WebAllowRegistration,
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
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	passwordKey := passwordChangeAttemptKey(session.User)
	allowed, retryAfter := s.reserveSingleAttempt(passwordKey, time.Now(), passwordChangeLimit)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Round(time.Second)/time.Second))))
		writeJSONError(w, http.StatusTooManyRequests, "too many password change attempts; try again later")
		return
	}
	user, err := s.Users.ChangePassword(session.User, req.CurrentPassword, req.NewPassword)
	if err != nil {
		if errors.Is(err, errPasswordWorkBusy) {
			s.releaseLoginReservation(passwordKey)
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "password service is temporarily busy; try again")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	expires := time.Now().Add(sessionTTL)
	http.SetCookie(w, s.sessionCookie(r, s.signSession(user.Username, user.Role, expires), expires))
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
		release, ok := s.acquireAuthenticatedRequest(session)
		if !ok {
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "too many concurrent requests; retry shortly")
			return
		}
		defer release()
		next(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, session)))
	}
}

func (s *Server) acquireAuthenticatedRequest(session sessionInfo) (func(), bool) {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	if s.activeUsers == nil {
		s.activeUsers = make(map[string]int)
	}
	user := strings.ToLower(strings.TrimSpace(session.User))
	if s.activeTotal >= maxAuthenticatedRequests || s.activeUsers[user] >= maxAuthenticatedRequestsUser {
		return nil, false
	}
	if session.Role != RoleAdmin && s.activeNonAdmin >= maxAuthenticatedRequests-reservedAdminRequests {
		return nil, false
	}
	s.activeUsers[user]++
	s.activeTotal++
	if session.Role != RoleAdmin {
		s.activeNonAdmin++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.requestMu.Lock()
			defer s.requestMu.Unlock()
			s.activeUsers[user]--
			if s.activeUsers[user] == 0 {
				delete(s.activeUsers, user)
			}
			s.activeTotal--
			if session.Role != RoleAdmin {
				s.activeNonAdmin--
			}
		})
	}, true
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
	if !ok || payload.Expires <= 0 || time.Now().Unix() >= payload.Expires {
		return sessionInfo{}, false
	}
	user, found, err := s.Users.Get(payload.User)
	if err != nil || !found {
		return sessionInfo{}, false
	}
	if payload.CredentialVersion == 0 || payload.CredentialVersion != user.UpdatedAt.UnixNano() {
		return sessionInfo{}, false
	}
	return sessionInfo{User: user.Username, Role: user.Role}, true
}

func (s *Server) signSession(user, role string, expires time.Time) string {
	credentialVersion := int64(0)
	if record, found, err := s.Users.Get(user); err == nil && found {
		credentialVersion = record.UpdatedAt.UnixNano()
	}
	payload := sessionPayload{User: user, Role: role, CredentialVersion: credentialVersion, Expires: expires.Unix()}
	data, _ := json.Marshal(payload)
	signature := s.sessionSignature(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (s *Server) verifySession(value string) (sessionPayload, bool) {
	if len(value) > 2048 {
		return sessionPayload{}, false
	}
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
	mac := hmac.New(sha256.New, s.sessionKey)
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
		SameSite: http.SameSiteStrictMode,
		Secure:   s.Cfg.WebSecureCookies || r.TLS != nil,
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
		SameSite: http.SameSiteStrictMode,
		Secure:   s.Cfg.WebSecureCookies || r.TLS != nil,
	}
}

func loginIPAttemptKey(r *http.Request) string {
	return loginIPAttemptKeyWithTrust(r, false)
}

func (s *Server) loginIPAttemptKey(r *http.Request) string {
	return loginIPAttemptKeyWithTrust(r, s.Cfg.WebTrustProxy)
}

func loginIPAttemptKeyWithTrust(r *http.Request, trustLoopbackProxy bool) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	remoteIP := net.ParseIP(strings.Trim(host, "[]"))
	if trustLoopbackProxy && remoteIP != nil && remoteIP.IsLoopback() {
		forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
		if forwardedIP := net.ParseIP(strings.TrimSpace(forwarded)); forwardedIP != nil {
			host = forwardedIP.String()
		}
	}
	return "ip:" + strings.ToLower(host)
}

func loginAttemptKey(r *http.Request, username string) string {
	return loginAttemptKeyForIP(loginIPAttemptKey(r), username)
}

func loginAttemptKeyForIP(ipKey, username string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(username))))
	return ipKey + ":user:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func loginUsernameAttemptKey(username string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(username))))
	return "login:user:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func registrationAttemptKeyForIP(ipKey string) string {
	return "registration:" + ipKey
}

func passwordChangeAttemptKey(username string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(username))))
	return "password:user:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Server) reserveLoginAttempt(userKey, ipKey, accountKey string, now time.Time) (bool, time.Duration) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	userAttempt := s.loginAttempts[userKey]
	if !userAttempt.ResetAt.IsZero() && !now.Before(userAttempt.ResetAt) {
		delete(s.loginAttempts, userKey)
		userAttempt = loginAttempt{}
	}
	ipAttempt := s.loginAttempts[ipKey]
	if !ipAttempt.ResetAt.IsZero() && !now.Before(ipAttempt.ResetAt) {
		delete(s.loginAttempts, ipKey)
		ipAttempt = loginAttempt{}
	}
	accountAttempt := s.loginAttempts[accountKey]
	if !accountAttempt.ResetAt.IsZero() && !now.Before(accountAttempt.ResetAt) {
		delete(s.loginAttempts, accountKey)
		accountAttempt = loginAttempt{}
	}
	var retryAfter time.Duration
	if userAttempt.Failures >= loginFailureLimit {
		retryAfter = userAttempt.ResetAt.Sub(now)
	}
	if ipAttempt.Failures >= loginIPFailureLimit {
		retryAfter = max(retryAfter, ipAttempt.ResetAt.Sub(now))
	}
	if accountAttempt.Failures >= loginUserFailureLimit {
		retryAfter = max(retryAfter, accountAttempt.ResetAt.Sub(now))
	}
	if retryAfter > 0 {
		return false, retryAfter
	}

	newKeys := 0
	if _, ok := s.loginAttempts[userKey]; !ok {
		newKeys++
	}
	if _, ok := s.loginAttempts[ipKey]; !ok {
		newKeys++
	}
	if _, ok := s.loginAttempts[accountKey]; !ok {
		newKeys++
	}
	if !s.ensureAttemptCapacityLocked(now, newKeys) {
		return false, loginWindow
	}

	for _, key := range []string{userKey, ipKey, accountKey} {
		attempt := s.loginAttempts[key]
		if attempt.ResetAt.IsZero() {
			attempt.ResetAt = now.Add(loginWindow)
		}
		attempt.Failures++
		s.loginAttempts[key] = attempt
	}
	return true, 0
}

func (s *Server) reserveSingleAttempt(key string, now time.Time, limit int) (bool, time.Duration) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	attempt := s.loginAttempts[key]
	if !attempt.ResetAt.IsZero() && !now.Before(attempt.ResetAt) {
		delete(s.loginAttempts, key)
		attempt = loginAttempt{}
	}
	if attempt.Failures >= limit {
		return false, attempt.ResetAt.Sub(now)
	}
	newKeys := 0
	if attempt.ResetAt.IsZero() {
		newKeys = 1
	}
	if !s.ensureAttemptCapacityLocked(now, newKeys) {
		return false, loginWindow
	}
	if attempt.ResetAt.IsZero() {
		attempt.ResetAt = now.Add(loginWindow)
	}
	attempt.Failures++
	s.loginAttempts[key] = attempt
	return true, 0
}

func (s *Server) reserveRegistrationAttempt(ipKey string, now time.Time) (bool, time.Duration) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	ipAttempt := s.loginAttempts[ipKey]
	if !ipAttempt.ResetAt.IsZero() && !now.Before(ipAttempt.ResetAt) {
		delete(s.loginAttempts, ipKey)
		ipAttempt = loginAttempt{}
	}
	globalAttempt := s.loginAttempts[registrationGlobalAttemptKey]
	if !globalAttempt.ResetAt.IsZero() && !now.Before(globalAttempt.ResetAt) {
		delete(s.loginAttempts, registrationGlobalAttemptKey)
		globalAttempt = loginAttempt{}
	}
	var retryAfter time.Duration
	if ipAttempt.Failures >= registrationIPLimit {
		retryAfter = ipAttempt.ResetAt.Sub(now)
	}
	if globalAttempt.Failures >= registrationGlobalLimit {
		retryAfter = max(retryAfter, globalAttempt.ResetAt.Sub(now))
	}
	if retryAfter > 0 {
		return false, retryAfter
	}

	newKeys := 0
	if _, ok := s.loginAttempts[ipKey]; !ok {
		newKeys++
	}
	if _, ok := s.loginAttempts[registrationGlobalAttemptKey]; !ok {
		newKeys++
	}
	if !s.ensureAttemptCapacityLocked(now, newKeys) {
		return false, loginWindow
	}
	for _, key := range []string{ipKey, registrationGlobalAttemptKey} {
		attempt := s.loginAttempts[key]
		if attempt.ResetAt.IsZero() {
			attempt.ResetAt = now.Add(loginWindow)
		}
		attempt.Failures++
		s.loginAttempts[key] = attempt
	}
	return true, 0
}

func (s *Server) ensureAttemptCapacityLocked(now time.Time, newKeys int) bool {
	if len(s.loginAttempts)+newKeys <= maxLoginAttemptKeys {
		return true
	}
	for key, attempt := range s.loginAttempts {
		if !now.Before(attempt.ResetAt) {
			delete(s.loginAttempts, key)
		}
	}
	return len(s.loginAttempts)+newKeys <= maxLoginAttemptKeys
}

func (s *Server) releaseLoginReservation(keys ...string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	for _, key := range keys {
		attempt, ok := s.loginAttempts[key]
		if !ok {
			continue
		}
		attempt.Failures--
		if attempt.Failures <= 0 {
			delete(s.loginAttempts, key)
		} else {
			s.loginAttempts[key] = attempt
		}
	}
}
