package web

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuardian/internal/config"
	"gpuardian/internal/history"
	"gpuardian/internal/telemetry"
)

func TestTelemetryCapabilityRequiresExplicitAdvertisement(t *testing.T) {
	legacy := telemetry.Info{NodeID: "node", TelemetrySchema: 1}
	if telemetryCapability(legacy, "reservation_external_session_id") {
		t.Fatal("legacy node unexpectedly accepts external_session_id")
	}
	current := telemetry.Info{NodeID: "node", TelemetrySchema: 1, Capabilities: []string{"telemetry_v1", "reservation_external_session_id"}}
	if !telemetryCapability(current, "reservation_external_session_id") {
		t.Fatal("current node capability was not detected")
	}
}

func TestHistorySearchCursorRoundTrip(t *testing.T) {
	number := 42.5
	original := history.SearchCursor{Field: "average_utilization_percent", Direction: "desc", ID: "session-a", Number: &number}
	decoded, ok := decodeHistorySearchCursor(encodeHistorySearchCursor(original))
	if !ok || decoded.Field != original.Field || decoded.Direction != original.Direction || decoded.ID != original.ID || decoded.Number == nil || *decoded.Number != number {
		t.Fatalf("cursor round trip = %+v ok=%v", decoded, ok)
	}
	if _, ok := decodeHistorySearchCursor("not-a-cursor"); ok {
		t.Fatal("invalid search cursor was accepted")
	}
}

func TestUnsupportedExternalSessionError(t *testing.T) {
	if !unsupportedExternalSessionError(errors.New(`node: unknown register argument "external_session_id"`)) {
		t.Fatal("legacy parser error was not detected")
	}
	if unsupportedExternalSessionError(errors.New("reservation overlaps existing session")) {
		t.Fatal("ordinary reservation error would be retried")
	}
}

func TestHistoryAPIReadForUsersAndResultOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	server := New(config.Config{
		WebSessionKey: filepath.Join(dir, "session.key"),
		WebRegistry:   filepath.Join(dir, "servers.json"),
		WebUsers:      filepath.Join(dir, "users.json"),
	})
	store, err := history.Open(filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server.History = store
	for _, user := range []struct{ name, role string }{{"alice", RoleUser}, {"bob", RoleUser}, {"admin", RoleAdmin}} {
		if _, err := server.Users.Create(user.name, "test-password-strong", user.role); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now().UTC().Add(-time.Hour)
	if err := store.PrepareSession(t.Context(), "sess_public", "node-a", "server-a", "GPU node", "alice", "training", start, start.Add(30*time.Minute), []int{0}); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmSession(t.Context(), "sess_public", "group-private", []string{"reservation-private"}, []int{0}); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()

	read := httptest.NewRecorder()
	readRequest := httptest.NewRequest(http.MethodGet, "/api/history/sessions/sess_public", nil)
	readRequest.AddCookie(testSessionCookie(t, server, "bob", RoleUser))
	handler.ServeHTTP(read, readRequest)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"owner":"alice"`) || strings.Contains(read.Body.String(), "group-private") || strings.Contains(read.Body.String(), "reservation-private") {
		t.Fatalf("history read = %d %s", read.Code, read.Body.String())
	}

	adminWrite := httptest.NewRecorder()
	adminRequest := httptest.NewRequest(http.MethodPut, "/api/history/sessions/sess_public/result", strings.NewReader(`{"outcome":"success","note":"admin","artifacts":[],"version":0}`))
	adminRequest.Header.Set("Content-Type", "application/json")
	adminRequest.AddCookie(testSessionCookie(t, server, "admin", RoleAdmin))
	handler.ServeHTTP(adminWrite, adminRequest)
	if adminWrite.Code != http.StatusForbidden {
		t.Fatalf("admin write = %d %s", adminWrite.Code, adminWrite.Body.String())
	}

	ownerWrite := httptest.NewRecorder()
	ownerRequest := httptest.NewRequest(http.MethodPut, "/api/history/sessions/sess_public/result", bytes.NewBufferString(`{"outcome":"success","note":"done <script>alert(1)</script>","artifacts":[{"label":"report","url":"https://example.test/report"}],"version":0}`))
	ownerRequest.Header.Set("Content-Type", "application/json")
	ownerRequest.AddCookie(testSessionCookie(t, server, "alice", RoleUser))
	handler.ServeHTTP(ownerWrite, ownerRequest)
	if ownerWrite.Code != http.StatusOK || !strings.Contains(ownerWrite.Body.String(), `"version":1`) {
		t.Fatalf("owner write = %d %s", ownerWrite.Code, ownerWrite.Body.String())
	}

	search := httptest.NewRecorder()
	searchRequest := httptest.NewRequest(http.MethodPost, "/api/history/search", strings.NewReader(`{"filter":{"groups":[{"rules":[{"field":"purpose","operator":"contains","value":"train"}]}]},"limit":50}`))
	searchRequest.Header.Set("Content-Type", "application/json")
	searchRequest.AddCookie(testSessionCookie(t, server, "bob", RoleUser))
	handler.ServeHTTP(search, searchRequest)
	if search.Code != http.StatusOK || !strings.Contains(search.Body.String(), `"sessions":1`) || !strings.Contains(search.Body.String(), `"purpose":"training"`) {
		t.Fatalf("history search = %d %s", search.Code, search.Body.String())
	}

	invalidSearch := httptest.NewRecorder()
	invalidRequest := httptest.NewRequest(http.MethodPost, "/api/history/search", strings.NewReader(`{"filter":{"groups":[{"rules":[{"field":"private_sql","operator":"equals","value":"x"}]}]}}`))
	invalidRequest.Header.Set("Content-Type", "application/json")
	invalidRequest.AddCookie(testSessionCookie(t, server, "bob", RoleUser))
	handler.ServeHTTP(invalidSearch, invalidRequest)
	if invalidSearch.Code != http.StatusBadRequest {
		t.Fatalf("invalid history search = %d %s", invalidSearch.Code, invalidSearch.Body.String())
	}
}
