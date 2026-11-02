package daemon

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuardian/internal/telemetry"
)

func telemetryTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	server.Telemetry, err = telemetry.Open(filepath.Join(dir, "node.id"), filepath.Join(dir, "outbox"), "boot-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Telemetry.Close() })
	return server, key
}

func getTelemetryPage(t *testing.T, server *Server, key, query string) (*httptest.ResponseRecorder, telemetry.Page) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/telemetry?"+query, nil)
	request.Header.Set("Authorization", "Bearer "+key)
	response := httptest.NewRecorder()
	server.nodeHTTPHandler().ServeHTTP(response, request)
	var page telemetry.Page
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
	}
	return response, page
}

func TestNodeTelemetryWaitHoldsEmptyPageUntilAnEvent(t *testing.T) {
	server, key := telemetryTestServer(t)
	if _, err := server.Telemetry.Append(telemetry.EventDaemonStarted, map[string]bool{"ok": true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, first := getTelemetryPage(t, server, key, "limit=10")
	if len(first.Events) != 1 {
		t.Fatalf("first page events = %d, want 1", len(first.Events))
	}
	if !strings.Contains(fmt.Sprint(server.Telemetry.Info().Capabilities), telemetry.CapabilityTelemetryWait) {
		t.Fatalf("info capabilities = %v, want %s", server.Telemetry.Info().Capabilities, telemetry.CapabilityTelemetryWait)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = server.Telemetry.Append(telemetry.EventJobStarted, map[string]string{"job": "a"}, time.Now())
	}()
	started := time.Now()
	response, page := getTelemetryPage(t, server, key, "limit=10&wait=5s&cursor="+url.QueryEscape(first.NextCursor))
	if response.Code != http.StatusOK || len(page.Events) != 1 || page.Events[0].Type != telemetry.EventJobStarted {
		t.Fatalf("held page = %d %s", response.Code, response.Body.String())
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("held page returned after %s, want shortly after the append", elapsed)
	}

	started = time.Now()
	response, page = getTelemetryPage(t, server, key, "limit=10&wait=150ms&cursor="+url.QueryEscape(page.NextCursor))
	if response.Code != http.StatusOK || len(page.Events) != 0 {
		t.Fatalf("idle page = %d %s", response.Code, response.Body.String())
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("idle page returned after %s, want the full wait", elapsed)
	}
}

func TestNodeTelemetryWaitRejectsOutOfRangeValues(t *testing.T) {
	server, key := telemetryTestServer(t)
	for _, wait := range []string{"-1s", "11s", "soon"} {
		if response, _ := getTelemetryPage(t, server, key, "wait="+wait); response.Code != http.StatusBadRequest {
			t.Fatalf("wait=%s status = %d, want 400", wait, response.Code)
		}
	}
}

func TestNodeHTTPCompressesLargeResponsesForGzipClients(t *testing.T) {
	server, key := telemetryTestServer(t)
	for i := range 40 {
		if _, err := server.Telemetry.Append(telemetry.EventJobUpdated, map[string]string{"job": fmt.Sprintf("job-%02d", i), "command": strings.Repeat("python train.py ", 8)}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/telemetry?limit=100", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	server.nodeHTTPHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status=%d encoding=%q", response.Code, response.Header().Get("Content-Encoding"))
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var page telemetry.Page
	if err := json.Unmarshal(body, &page); err != nil || len(page.Events) != 40 {
		t.Fatalf("decoded page events = %d (%v)", len(page.Events), err)
	}
}
