package web

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuardian/internal/config"
)

func TestSessionLoginProtectsAPIWithoutBasicPopup(t *testing.T) {
	server := New(config.Config{
		WebUser:     "admin",
		WebPassword: "test-password-strong",
		WebRegistry: filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:    filepath.Join(t.TempDir(), "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "test-password-strong"); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/servers", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	if header := unauthorized.Header().Get("WWW-Authenticate"); header != "" {
		t.Fatalf("WWW-Authenticate header = %q, want empty", header)
	}

	login := httptest.NewRecorder()
	body := strings.NewReader(`{"username":"admin","password":"test-password-strong"}`)
	loginReq := httptest.NewRequest(http.MethodPost, "/api/login", body)
	loginReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(login, loginReq)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
		t.Fatalf("login cookies = %+v, want %s", cookies, sessionCookieName)
	}

	session := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	req.AddCookie(cookies[0])
	handler.ServeHTTP(session, req)
	if session.Code != http.StatusOK ||
		!bytes.Contains(session.Body.Bytes(), []byte(`"authenticated":true`)) ||
		!bytes.Contains(session.Body.Bytes(), []byte(`"role":"admin"`)) {
		t.Fatalf("session response = %d %s", session.Code, session.Body.String())
	}

	authorized := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	req.AddCookie(cookies[0])
	handler.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body=%s", authorized.Code, authorized.Body.String())
	}
}

func TestSessionRegistrationIsNotPublic(t *testing.T) {
	server := New(config.Config{
		WebUser:     "admin",
		WebPassword: "test-password-strong",
		WebRegistry: filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:    filepath.Join(t.TempDir(), "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "test-password-strong"); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()

	registered := httptest.NewRecorder()
	body := strings.NewReader(`{"username":"alice","password":"alice-secret","role":"admin"}`)
	registerReq := httptest.NewRequest(http.MethodPost, "/api/register", body)
	registerReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(registered, registerReq)
	if registered.Code != http.StatusNotFound {
		t.Fatalf("register response = %d %s", registered.Code, registered.Body.String())
	}
	if cookies := registered.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("disabled registration cookies = %+v", cookies)
	}
	if _, found, err := server.Users.Get("alice"); err != nil || found {
		t.Fatalf("public registration created user: found=%v, err=%v", found, err)
	}
}

func TestSessionAdvertisesRegistrationSetting(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			server := New(config.Config{WebAllowRegistration: enabled})
			response := httptest.NewRecorder()
			server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/session", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("session status = %d body=%s", response.Code, response.Body.String())
			}
			want := fmt.Sprintf(`"registration_enabled":%t`, enabled)
			if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
				t.Fatalf("session body = %s, want %s", response.Body.String(), want)
			}
		})
	}
}

func TestSessionRegistrationCreatesRegularUserAndSession(t *testing.T) {
	dir := t.TempDir()
	server := New(config.Config{
		WebAllowRegistration: true,
		WebSessionKey:        filepath.Join(dir, "session.key"),
		WebRegistry:          filepath.Join(dir, "servers.json"),
		WebUsers:             filepath.Join(dir, "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "bootstrap-password"); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()
	registered := requestJSON(handler, http.MethodPost, "/api/register", `{"username":"Alice","password":"alice-password"}`, nil)
	if registered.Code != http.StatusCreated {
		t.Fatalf("register response = %d %s", registered.Code, registered.Body.String())
	}
	cookies := registered.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
		t.Fatalf("register cookies = %+v", cookies)
	}
	record, found, err := server.Users.Get("alice")
	if err != nil || !found || record.Role != RoleUser {
		t.Fatalf("registered user = %+v found=%v err=%v", record, found, err)
	}
	if !bytes.Contains(registered.Body.Bytes(), []byte(`"role":"user"`)) {
		t.Fatalf("register body = %s", registered.Body.String())
	}
	session := requestJSON(handler, http.MethodGet, "/api/session", "", cookies[0])
	if session.Code != http.StatusOK ||
		!bytes.Contains(session.Body.Bytes(), []byte(`"authenticated":true`)) ||
		!bytes.Contains(session.Body.Bytes(), []byte(`"user":"alice"`)) {
		t.Fatalf("registered session = %d %s", session.Code, session.Body.String())
	}
	users := requestJSON(handler, http.MethodGet, "/api/users", "", cookies[0])
	if users.Code != http.StatusForbidden {
		t.Fatalf("registered user accessed admin endpoint: %d %s", users.Code, users.Body.String())
	}
}

func TestSessionRegistrationRejectsRoleInjection(t *testing.T) {
	server := New(config.Config{
		WebAllowRegistration: true,
		WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:             filepath.Join(t.TempDir(), "users.json"),
	})
	registered := requestJSON(server.routes(), http.MethodPost, "/api/register", `{"username":"alice","password":"alice-password","role":"admin"}`, nil)
	if registered.Code != http.StatusBadRequest {
		t.Fatalf("role injection response = %d %s", registered.Code, registered.Body.String())
	}
	if cookies := registered.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("role injection cookies = %+v", cookies)
	}
	if _, found, err := server.Users.Get("alice"); err != nil || found {
		t.Fatalf("role injection created user: found=%v err=%v", found, err)
	}
}

func TestSessionRegistrationRejectsInvalidPasswordsBeforeHashing(t *testing.T) {
	server := New(config.Config{
		WebAllowRegistration: true,
		WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:             filepath.Join(t.TempDir(), "users.json"),
	})
	for name, password := range map[string]string{
		"empty":      "",
		"too-short":  "elevenbytes",
		"too-long":   strings.Repeat("x", maxPasswordBytes+1),
		"whitespace": strings.Repeat(" ", minPasswordBytes),
	} {
		t.Run(name, func(t *testing.T) {
			body := fmt.Sprintf(`{"username":"alice","password":%q}`, password)
			registered := requestJSON(server.routes(), http.MethodPost, "/api/register", body, nil)
			if registered.Code != http.StatusBadRequest {
				t.Fatalf("invalid password response = %d %s", registered.Code, registered.Body.String())
			}
			if len(server.Users.passwordWork) != 0 {
				t.Fatal("invalid password acquired a password worker")
			}
		})
	}
}

func TestSessionRegistrationRequiresSameOriginJSON(t *testing.T) {
	server := New(config.Config{
		WebAllowRegistration: true,
		WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:             filepath.Join(t.TempDir(), "users.json"),
	})
	handler := server.routes()
	body := `{"username":"alice","password":"alice-password"}`

	missingType := httptest.NewRecorder()
	handler.ServeHTTP(missingType, httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(body)))
	if missingType.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type response = %d %s", missingType.Code, missingType.Body.String())
	}

	crossOriginRequest := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(body))
	crossOriginRequest.Header.Set("Content-Type", "application/json")
	crossOriginRequest.Header.Set("Origin", "https://attacker.example")
	crossOrigin := httptest.NewRecorder()
	handler.ServeHTTP(crossOrigin, crossOriginRequest)
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin response = %d %s", crossOrigin.Code, crossOrigin.Body.String())
	}
	if _, found, err := server.Users.Get("alice"); err != nil || found {
		t.Fatalf("unsafe registration created user: found=%v err=%v", found, err)
	}
}

func TestSessionRegistrationRejectsCanonicalDuplicate(t *testing.T) {
	server := New(config.Config{
		WebAllowRegistration: true,
		WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:             filepath.Join(t.TempDir(), "users.json"),
	})
	if _, err := server.Users.Create("alice", "original-password", RoleUser); err != nil {
		t.Fatal(err)
	}
	registered := requestJSON(server.routes(), http.MethodPost, "/api/register", `{"username":" Alice ","password":"replacement-password"}`, nil)
	if registered.Code != http.StatusConflict {
		t.Fatalf("duplicate registration = %d %s", registered.Code, registered.Body.String())
	}
	if _, err := server.Users.Authenticate("alice", "original-password"); err != nil {
		t.Fatalf("duplicate registration changed original password: %v", err)
	}
	if _, err := server.Users.Authenticate("alice", "replacement-password"); err == nil {
		t.Fatal("duplicate registration replaced original password")
	}
}

func TestConcurrentRegistrationCreatesOneCanonicalUser(t *testing.T) {
	server := New(config.Config{
		WebAllowRegistration: true,
		WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:             filepath.Join(t.TempDir(), "users.json"),
	})
	handler := server.routes()
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, username := range []string{"Alice", " alice "} {
		go func() {
			<-start
			results <- requestJSON(handler, http.MethodPost, "/api/register", fmt.Sprintf(`{"username":%q,"password":"alice-password"}`, username), nil)
		}()
	}
	close(start)
	codes := map[int]int{}
	cookies := 0
	for range 2 {
		response := <-results
		codes[response.Code]++
		cookies += len(response.Result().Cookies())
	}
	if codes[http.StatusCreated] != 1 || codes[http.StatusConflict] != 1 {
		t.Fatalf("concurrent registration codes = %+v", codes)
	}
	if cookies != 1 {
		t.Fatalf("concurrent registration cookies = %d, want 1", cookies)
	}
	users, err := server.Users.List()
	if err != nil || len(users) != 1 || users[0].Username != "alice" || users[0].Role != RoleUser {
		t.Fatalf("concurrent registered users = %+v err=%v", users, err)
	}
}

func TestSessionRegistrationReturnsBusyWithoutConsumingRateLimit(t *testing.T) {
	server := New(config.Config{
		WebAllowRegistration: true,
		WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:             filepath.Join(t.TempDir(), "users.json"),
	})
	for range maxConcurrentPasswordWork {
		server.Users.passwordWork <- struct{}{}
	}
	defer func() {
		for range maxConcurrentPasswordWork {
			<-server.Users.passwordWork
		}
	}()
	registered := requestJSON(server.routes(), http.MethodPost, "/api/register", `{"username":"alice","password":"alice-password"}`, nil)
	if registered.Code != http.StatusTooManyRequests || registered.Header().Get("Retry-After") == "" {
		t.Fatalf("busy registration = %d headers=%v body=%s", registered.Code, registered.Header(), registered.Body.String())
	}
	server.loginMu.Lock()
	defer server.loginMu.Unlock()
	if len(server.loginAttempts) != 0 {
		t.Fatalf("busy registration consumed rate limit: %+v", server.loginAttempts)
	}
}

func TestSessionRegistrationRateLimitRunsBeforePasswordHash(t *testing.T) {
	server := New(config.Config{
		WebAllowRegistration: true,
		WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:             filepath.Join(t.TempDir(), "users.json"),
	})
	server.loginAttempts[registrationGlobalAttemptKey] = loginAttempt{Failures: registrationGlobalLimit, ResetAt: time.Now().Add(time.Minute)}
	registered := requestJSON(server.routes(), http.MethodPost, "/api/register", `{"username":"alice","password":"alice-password"}`, nil)
	if registered.Code != http.StatusTooManyRequests || registered.Header().Get("Retry-After") == "" {
		t.Fatalf("limited registration = %d headers=%v body=%s", registered.Code, registered.Header(), registered.Body.String())
	}
	if len(server.Users.passwordWork) != 0 {
		t.Fatal("rate-limited registration acquired a password worker")
	}
}

func TestSessionRegistrationOutcomesConsumeRateWithoutChangingLoginCounters(t *testing.T) {
	server := New(config.Config{
		WebAllowRegistration: true,
		WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:             filepath.Join(t.TempDir(), "users.json"),
	})
	loginKey := loginUsernameAttemptKey("victim")
	server.loginAttempts[loginKey] = loginAttempt{Failures: 3, ResetAt: time.Now().Add(time.Minute)}
	handler := server.routes()
	created := requestJSON(handler, http.MethodPost, "/api/register", `{"username":"alice","password":"alice-password"}`, nil)
	duplicate := requestJSON(handler, http.MethodPost, "/api/register", `{"username":"Alice","password":"other-password"}`, nil)
	if created.Code != http.StatusCreated || duplicate.Code != http.StatusConflict {
		t.Fatalf("registration outcomes = %d/%d bodies=%s / %s", created.Code, duplicate.Code, created.Body.String(), duplicate.Body.String())
	}
	ipKey := registrationAttemptKeyForIP("ip:192.0.2.1")
	server.loginMu.Lock()
	defer server.loginMu.Unlock()
	if got := server.loginAttempts[ipKey].Failures; got != 2 {
		t.Fatalf("registration IP attempts = %d, want 2", got)
	}
	if got := server.loginAttempts[registrationGlobalAttemptKey].Failures; got != 2 {
		t.Fatalf("global registration attempts = %d, want 2", got)
	}
	if got := server.loginAttempts[loginKey].Failures; got != 3 {
		t.Fatalf("registration changed login attempts to %d", got)
	}
}

func TestSessionRegistrationUserLimitAndStorageErrorsAreGeneric(t *testing.T) {
	t.Run("user-limit", func(t *testing.T) {
		server := New(config.Config{
			WebAllowRegistration: true,
			WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
			WebUsers:             filepath.Join(t.TempDir(), "users.json"),
		})
		server.Users.mu.Lock()
		server.Users.loaded = true
		server.Users.users = make([]UserRecord, maxUsers)
		server.Users.mu.Unlock()
		response := requestJSON(server.routes(), http.MethodPost, "/api/register", `{"username":"alice","password":"alice-password"}`, nil)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "registration is unavailable") {
			t.Fatalf("full registration response = %d %s", response.Code, response.Body.String())
		}
	})

	t.Run("storage-error", func(t *testing.T) {
		usersPath := t.TempDir()
		server := New(config.Config{
			WebAllowRegistration: true,
			WebRegistry:          filepath.Join(t.TempDir(), "servers.json"),
			WebUsers:             usersPath,
		})
		response := requestJSON(server.routes(), http.MethodPost, "/api/register", `{"username":"alice","password":"alice-password"}`, nil)
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "unable to create account") {
			t.Fatalf("storage registration response = %d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), usersPath) {
			t.Fatalf("storage registration leaked path: %s", response.Body.String())
		}
	})
}

func TestRegistrationPerIPLimitCannotBeBypassedByGlobalHeadroom(t *testing.T) {
	server := New(config.Config{})
	now := time.Now()
	ipKey := registrationAttemptKeyForIP("ip:192.0.2.10")
	server.loginAttempts[ipKey] = loginAttempt{Failures: registrationIPLimit, ResetAt: now.Add(time.Minute)}
	server.loginAttempts[registrationGlobalAttemptKey] = loginAttempt{Failures: 1, ResetAt: now.Add(time.Minute)}
	if allowed, retryAfter := server.reserveRegistrationAttempt(ipKey, now); allowed || retryAfter <= 0 {
		t.Fatalf("per-IP registration limit allowed=%v retry=%v", allowed, retryAfter)
	}
}

func TestRegistrationPerIPLimitLeavesCapacityForAnotherIP(t *testing.T) {
	server := New(config.Config{})
	now := time.Now()
	firstIP := registrationAttemptKeyForIP("ip:192.0.2.10")
	for attempt := 0; attempt < registrationIPLimit; attempt++ {
		if allowed, _ := server.reserveRegistrationAttempt(firstIP, now); !allowed {
			t.Fatalf("first IP attempt %d rejected before its limit", attempt)
		}
	}
	if allowed, retryAfter := server.reserveRegistrationAttempt(firstIP, now); allowed || retryAfter <= 0 {
		t.Fatalf("first IP exceeded limit: allowed=%v retry=%v", allowed, retryAfter)
	}
	secondIP := registrationAttemptKeyForIP("ip:198.51.100.20")
	if allowed, retryAfter := server.reserveRegistrationAttempt(secondIP, now); !allowed || retryAfter != 0 {
		t.Fatalf("second IP was blocked by first IP quota: allowed=%v retry=%v", allowed, retryAfter)
	}
	server.loginMu.Lock()
	defer server.loginMu.Unlock()
	if got := server.loginAttempts[registrationGlobalAttemptKey].Failures; got != registrationIPLimit+1 {
		t.Fatalf("global attempts = %d, want %d", got, registrationIPLimit+1)
	}
}

func TestRegistrationReservationsAreAtomic(t *testing.T) {
	server := New(config.Config{})
	const attempts = 32
	start := make(chan struct{})
	results := make(chan bool, attempts)
	now := time.Now()
	for index := range attempts {
		ipKey := registrationAttemptKeyForIP(fmt.Sprintf("ip:192.0.2.%d", index))
		go func() {
			<-start
			allowed, _ := server.reserveRegistrationAttempt(ipKey, now)
			results <- allowed
		}()
	}
	close(start)
	allowed := 0
	for range attempts {
		if <-results {
			allowed++
		}
	}
	if allowed != registrationGlobalLimit {
		t.Fatalf("concurrent registrations allowed = %d, want %d", allowed, registrationGlobalLimit)
	}
}

func TestSessionChangePassword(t *testing.T) {
	server := New(config.Config{
		WebUser:     "admin",
		WebPassword: "test-password-strong",
		WebRegistry: filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:    filepath.Join(t.TempDir(), "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "test-password-strong"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Users.Create("alice", "old-password-strong", RoleUser); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()
	cookie := testSessionCookie(t, server, "alice", RoleUser)

	wrong := requestJSON(handler, http.MethodPost, "/api/password", `{"current_password":"wrong","new_password":"new-password-strong"}`, cookie)
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong current password status = %d, body=%s", wrong.Code, wrong.Body.String())
	}

	changed := requestJSON(handler, http.MethodPost, "/api/password", `{"current_password":"old-password-strong","new_password":"new-password-strong"}`, cookie)
	if changed.Code != http.StatusOK {
		t.Fatalf("change password status = %d, body=%s", changed.Code, changed.Body.String())
	}
	staleSession := httptest.NewRecorder()
	staleReq := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	staleReq.AddCookie(cookie)
	handler.ServeHTTP(staleSession, staleReq)
	if staleSession.Code != http.StatusUnauthorized {
		t.Fatalf("pre-change cookie status = %d, want unauthorized", staleSession.Code)
	}
	changedCookies := changed.Result().Cookies()
	if len(changedCookies) != 1 {
		t.Fatalf("change password cookies = %+v", changedCookies)
	}
	freshSession := httptest.NewRecorder()
	freshReq := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	freshReq.AddCookie(changedCookies[0])
	handler.ServeHTTP(freshSession, freshReq)
	if freshSession.Code != http.StatusOK {
		t.Fatalf("replacement cookie status = %d, body=%s", freshSession.Code, freshSession.Body.String())
	}

	oldLogin := httptest.NewRecorder()
	oldLoginReq := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"alice","password":"old-password-strong"}`))
	oldLoginReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(oldLogin, oldLoginReq)
	if oldLogin.Code != http.StatusUnauthorized {
		t.Fatalf("old login status = %d, want unauthorized", oldLogin.Code)
	}
	newLogin := httptest.NewRecorder()
	newLoginReq := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"alice","password":"new-password-strong"}`))
	newLoginReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(newLogin, newLoginReq)
	if newLogin.Code != http.StatusOK {
		t.Fatalf("new login status = %d, body=%s", newLogin.Code, newLogin.Body.String())
	}
}

func TestPasswordChangeRateLimit(t *testing.T) {
	server := New(config.Config{
		WebRegistry: filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:    filepath.Join(t.TempDir(), "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "bootstrap-password"); err != nil {
		t.Fatal(err)
	}
	key := passwordChangeAttemptKey("admin")
	server.loginAttempts[key] = loginAttempt{Failures: passwordChangeLimit, ResetAt: time.Now().Add(time.Minute)}
	response := requestJSON(
		server.routes(),
		http.MethodPost,
		"/api/password",
		`{"current_password":"bootstrap-password","new_password":"replacement-password"}`,
		testSessionCookie(t, server, "admin", RoleAdmin),
	)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("limited password change = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestSessionsUseIndependentRandomKeys(t *testing.T) {
	newServer := func() *Server {
		server := New(config.Config{
			WebUser:     "admin",
			WebPassword: "shared-bootstrap-password",
			WebRegistry: filepath.Join(t.TempDir(), "servers.json"),
			WebUsers:    filepath.Join(t.TempDir(), "users.json"),
		})
		if err := server.Users.BootstrapAdmin("admin", "shared-bootstrap-password"); err != nil {
			t.Fatal(err)
		}
		return server
	}
	first := newServer()
	second := newServer()
	token := first.signSession("admin", RoleAdmin, time.Now().Add(time.Hour))
	if _, ok := second.verifySession(token); ok {
		t.Fatal("session signed by an independent random key verified")
	}
}

func TestLoginRateLimit(t *testing.T) {
	server := New(config.Config{
		WebUser:     "admin",
		WebPassword: "bootstrap-password",
		WebRegistry: filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:    filepath.Join(t.TempDir(), "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "bootstrap-password"); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()
	keyReq := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
	keyReq.Header.Set("Content-Type", "application/json")
	key := loginAttemptKey(keyReq, "admin")
	server.loginAttempts[key] = loginAttempt{Failures: loginFailureLimit, ResetAt: time.Now().Add(time.Minute)}
	limited := httptest.NewRecorder()
	handler.ServeHTTP(limited, keyReq)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("limited login = %d headers=%v body=%s", limited.Code, limited.Header(), limited.Body.String())
	}
}

func TestAuthenticatedRequestAdmissionReservesAdminCapacity(t *testing.T) {
	server := New(config.Config{})
	var releases []func()
	users := (maxAuthenticatedRequests - reservedAdminRequests) / maxAuthenticatedRequestsUser
	for user := 0; user < users; user++ {
		for range maxAuthenticatedRequestsUser {
			release, ok := server.acquireAuthenticatedRequest(sessionInfo{User: fmt.Sprintf("user-%d", user), Role: RoleUser})
			if !ok {
				t.Fatalf("regular request rejected before non-admin boundary for user %d", user)
			}
			releases = append(releases, release)
		}
	}
	if _, ok := server.acquireAuthenticatedRequest(sessionInfo{User: "extra", Role: RoleUser}); ok {
		t.Fatal("regular request consumed admin-reserved capacity")
	}
	adminRelease, ok := server.acquireAuthenticatedRequest(sessionInfo{User: "admin", Role: RoleAdmin})
	if !ok {
		t.Fatal("admin request could not use reserved capacity")
	}
	adminRelease()
	for _, release := range releases {
		release()
	}
}

func TestLoginRateLimitCannotBeBypassedByUsernameSpray(t *testing.T) {
	server := New(config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	if loginAttemptKey(req, "alice") == loginAttemptKey(req, "bob") {
		t.Fatal("per-account login limiter key does not vary by username")
	}
	now := time.Now()
	ipKey := loginIPAttemptKey(req)
	for i := 0; i < loginIPFailureLimit; i++ {
		username := fmt.Sprintf("user-%d", i)
		allowed, _ := server.reserveLoginAttempt(loginAttemptKey(req, username), ipKey, loginUsernameAttemptKey(username), now)
		if !allowed {
			t.Fatalf("attempt %d was limited before the IP budget was exhausted", i)
		}
	}
	if allowed, retry := server.reserveLoginAttempt(loginAttemptKey(req, "one-more-user"), ipKey, loginUsernameAttemptKey("one-more-user"), now); allowed || retry <= 0 {
		t.Fatalf("username spray allowed=%v retry=%v after IP budget exhaustion", allowed, retry)
	}

	for i := 0; i < maxLoginAttemptKeys; i++ {
		request := httptest.NewRequest(http.MethodPost, "/api/login", nil)
		request.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i)
		server.reserveLoginAttempt(loginAttemptKey(request, "user"), loginIPAttemptKey(request), loginUsernameAttemptKey("user"), now)
	}
	if len(server.loginAttempts) > maxLoginAttemptKeys {
		t.Fatalf("login limiter retained %d entries, want at most %d", len(server.loginAttempts), maxLoginAttemptKeys)
	}
}

func TestTrustedLoopbackProxyUsesForwardedClientIP(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	request.RemoteAddr = "127.0.0.1:4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.7, 127.0.0.1")
	if got := loginIPAttemptKeyWithTrust(request, true); got != "ip:203.0.113.7" {
		t.Fatalf("trusted proxy key = %q", got)
	}
	request.RemoteAddr = "198.51.100.10:4321"
	if got := loginIPAttemptKeyWithTrust(request, true); got != "ip:198.51.100.10" {
		t.Fatalf("untrusted remote supplied forwarded address: %q", got)
	}
}

func TestLoginAccountLimitSurvivesForwardedIPSpoofing(t *testing.T) {
	server := New(config.Config{})
	now := time.Now()
	accountKey := loginUsernameAttemptKey("alice")
	for attempt := 0; attempt < loginUserFailureLimit; attempt++ {
		ipKey := fmt.Sprintf("ip:198.51.100.%d", attempt)
		if allowed, _ := server.reserveLoginAttempt(loginAttemptKeyForIP(ipKey, "alice"), ipKey, accountKey, now); !allowed {
			t.Fatalf("attempt %d rejected before account limit", attempt)
		}
	}
	ipKey := "ip:203.0.113.1"
	if allowed, retry := server.reserveLoginAttempt(loginAttemptKeyForIP(ipKey, "alice"), ipKey, accountKey, now); allowed || retry <= 0 {
		t.Fatalf("spoofed-IP attempt allowed=%v retry=%v after account limit", allowed, retry)
	}
}

func TestLoginLimiterFailsClosedWithoutEvictingActiveLimits(t *testing.T) {
	server := New(config.Config{})
	now := time.Now()
	accountKey := loginUsernameAttemptKey("alice")
	server.loginAttempts[accountKey] = loginAttempt{Failures: loginUserFailureLimit, ResetAt: now.Add(time.Minute)}
	for index := 0; len(server.loginAttempts) < maxLoginAttemptKeys; index++ {
		server.loginAttempts[fmt.Sprintf("filler:%d", index)] = loginAttempt{Failures: 1, ResetAt: now.Add(2 * time.Minute)}
	}

	if allowed, _ := server.reserveLoginAttempt("new:user", "new:ip", "new:account", now); allowed {
		t.Fatal("login limiter admitted new keys after reaching its capacity")
	}
	if _, ok := server.loginAttempts[accountKey]; !ok {
		t.Fatal("capacity pressure evicted an active account limit")
	}
	if allowed, retry := server.reserveLoginAttempt("alice:new-ip", "new-alice-ip", accountKey, now); allowed || retry <= 0 {
		t.Fatalf("active account limit was bypassed after capacity pressure: allowed=%v retry=%v", allowed, retry)
	}
}

func TestLoginReservationsAreAtomic(t *testing.T) {
	server := New(config.Config{})
	request := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	userKey := loginAttemptKey(request, "alice")
	ipKey := loginIPAttemptKey(request)
	accountKey := loginUsernameAttemptKey("alice")
	now := time.Now()
	const attempts = 32
	start := make(chan struct{})
	results := make(chan bool, attempts)
	for range attempts {
		go func() {
			<-start
			allowed, _ := server.reserveLoginAttempt(userKey, ipKey, accountKey, now)
			results <- allowed
		}()
	}
	close(start)
	allowed := 0
	for range attempts {
		if <-results {
			allowed++
		}
	}
	if allowed != loginFailureLimit {
		t.Fatalf("concurrent attempts allowed = %d, want %d", allowed, loginFailureLimit)
	}
}

func TestLoginReturnsTooManyRequestsWhenPasswordWorkersAreBusy(t *testing.T) {
	server := New(config.Config{
		WebRegistry: filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:    filepath.Join(t.TempDir(), "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "bootstrap-password"); err != nil {
		t.Fatal(err)
	}
	for range maxConcurrentPasswordWork {
		server.Users.passwordWork <- struct{}{}
	}
	for range maxQueuedPasswordWork {
		server.Users.passwordQueue <- struct{}{}
	}
	defer func() {
		for range maxConcurrentPasswordWork {
			<-server.Users.passwordWork
		}
		for range maxQueuedPasswordWork {
			<-server.Users.passwordQueue
		}
	}()

	request := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"bootstrap-password"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("busy login = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	server.loginMu.Lock()
	defer server.loginMu.Unlock()
	if len(server.loginAttempts) != 0 {
		t.Fatalf("busy login consumed failure budget: %+v", server.loginAttempts)
	}
}

func TestSuccessfulLoginDoesNotResetIPFailureBudget(t *testing.T) {
	server := New(config.Config{
		WebRegistry: filepath.Join(t.TempDir(), "servers.json"),
		WebUsers:    filepath.Join(t.TempDir(), "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "bootstrap-password"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"bootstrap-password"}`))
	request.Header.Set("Content-Type", "application/json")
	key := loginIPAttemptKey(request)
	server.loginAttempts[key] = loginAttempt{Failures: loginIPFailureLimit - 1, ResetAt: time.Now().Add(time.Minute)}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("valid login status = %d, body=%s", response.Code, response.Body.String())
	}
	if got := server.loginAttempts[key].Failures; got != loginIPFailureLimit-1 {
		t.Fatalf("failure budget reset to %d after valid login", got)
	}
}
