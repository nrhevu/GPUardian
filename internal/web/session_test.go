package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"rocguardd/internal/config"
)

func TestSessionLoginProtectsAPIWithoutBasicPopup(t *testing.T) {
	server := New(config.Config{
		WebUser:     "admin",
		WebPassword: "secret",
		WebRegistry: filepath.Join(t.TempDir(), "servers.json"),
	})
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
	body := strings.NewReader(`{"username":"admin","password":"secret"}`)
	handler.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/login", body))
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
	if session.Code != http.StatusOK || !bytes.Contains(session.Body.Bytes(), []byte(`"authenticated":true`)) {
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
