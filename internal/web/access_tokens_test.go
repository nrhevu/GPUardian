package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuardian/internal/config"
)

func TestAccessTokenLifecycleStoresOnlyHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	store := NewUserStore(path)
	if err := store.BootstrapAdmin("admin", "test-password-strong"); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateAccessToken(
		"admin", "training SDK", []string{accessTokenScopeHistoryRead, accessTokenScopeNodesRead, accessTokenScopeHistoryRead}, time.Now().Add(24*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Secret, accessTokenPrefix) || !strings.HasPrefix(created.ID, "at_") {
		t.Fatalf("created token = %+v", created)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), created.Secret) {
		t.Fatal("users file contains plaintext access token")
	}
	authenticated, ok := store.AuthenticateAccessToken(created.Secret)
	if !ok || authenticated.User != "admin" || authenticated.ID != created.ID {
		t.Fatalf("authenticated = %+v, ok=%v", authenticated, ok)
	}
	if len(authenticated.Scopes) != 2 || authenticated.Scopes[0] != accessTokenScopeHistoryRead || authenticated.Scopes[1] != accessTokenScopeNodesRead {
		t.Fatalf("scopes = %v", authenticated.Scopes)
	}
	if err := store.RevokeAccessToken("admin", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.AuthenticateAccessToken(created.Secret); ok {
		t.Fatal("revoked token authenticated")
	}
}

func TestLegacyAccessTokenFieldMigratesWithoutInvalidatingSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	store := NewUserStore(path)
	if err := store.BootstrapAdmin("admin", "test-password-strong"); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateAccessToken("admin", "legacy", []string{accessTokenScopeNodesRead}, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	legacyData := strings.Replace(string(data), `"access_tokens"`, `"mcp_access_tokens"`, 1)
	if err := os.WriteFile(path, []byte(legacyData), 0600); err != nil {
		t.Fatal(err)
	}

	reloaded := NewUserStore(path)
	authenticated, ok := reloaded.AuthenticateAccessToken(created.Secret)
	if !ok || authenticated.ID != created.ID {
		t.Fatalf("legacy token authenticated = %+v, ok=%v", authenticated, ok)
	}
	if err := reloaded.RevokeAccessToken("admin", created.ID); err != nil {
		t.Fatal(err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(migrated), `"mcp_access_tokens"`) || !strings.Contains(string(migrated), `"access_tokens"`) {
		t.Fatalf("legacy token field was not migrated: %s", migrated)
	}
}

func TestAccessTokenAPIAndScopeEnforcement(t *testing.T) {
	dir := t.TempDir()
	server := New(config.Config{
		WebRegistry: filepath.Join(dir, "servers.json"),
		WebUsers:    filepath.Join(dir, "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "test-password-strong"); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()
	cookie := testSessionCookie(t, server, "admin", RoleAdmin)

	createdResponse := requestJSON(
		handler, http.MethodPost, "/api/access-tokens",
		`{"name":"history reader","expiry_days":30,"scopes":["history:read"]}`,
		cookie,
	)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created CreatedAccessToken
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	historyRequest := httptest.NewRequest(http.MethodGet, "/api/history/summary", nil)
	historyRequest.Header.Set("Authorization", "Bearer "+created.Secret)
	historyResponse := httptest.NewRecorder()
	handler.ServeHTTP(historyResponse, historyRequest)
	if historyResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("history status = %d, want service unavailable after successful auth; body=%s", historyResponse.Code, historyResponse.Body.String())
	}

	serversRequest := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	serversRequest.Header.Set("Authorization", "Bearer "+created.Secret)
	serversResponse := httptest.NewRecorder()
	handler.ServeHTTP(serversResponse, serversRequest)
	if serversResponse.Code != http.StatusForbidden || !strings.Contains(serversResponse.Body.String(), accessTokenScopeNodesRead) {
		t.Fatalf("servers response = %d %s", serversResponse.Code, serversResponse.Body.String())
	}

	manageRequest := httptest.NewRequest(http.MethodGet, "/api/access-tokens", nil)
	manageRequest.Header.Set("Authorization", "Bearer "+created.Secret)
	manageResponse := httptest.NewRecorder()
	handler.ServeHTTP(manageResponse, manageRequest)
	if manageResponse.Code != http.StatusUnauthorized {
		t.Fatalf("token management response = %d %s", manageResponse.Code, manageResponse.Body.String())
	}

	revokeResponse := requestJSON(handler, http.MethodDelete, "/api/access-tokens/"+created.ID, "", cookie)
	if revokeResponse.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body=%s", revokeResponse.Code, revokeResponse.Body.String())
	}
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	sessionRequest.Header.Set("Authorization", "Bearer "+created.Secret)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	if !strings.Contains(sessionResponse.Body.String(), `"authenticated":false`) {
		t.Fatalf("revoked session response = %d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
}
