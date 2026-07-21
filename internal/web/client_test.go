package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNodeClientReusesConnections(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()

	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	client := NodeClient{Timeout: time.Second, clients: clients}
	record := testNodeRecord(server.URL)
	for i := 0; i < 2; i++ {
		if err := client.Health(context.Background(), record, record.RootKey); err != nil {
			t.Fatal(err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("connections = %d, want 1", got)
	}
}

func TestNodeClientAllowsHTTPOnlyWithExplicitOptIn(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer rk_test" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	record := testNodeRecord(server.URL)
	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	blocked := NodeClient{Timeout: time.Second, clients: clients}
	if err := blocked.Health(context.Background(), record, record.RootKey); err == nil || !strings.Contains(err.Error(), "GPUARDIAN_WEB_ALLOW_INSECURE_NODES") {
		t.Fatalf("default HTTP error = %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("blocked client contacted plaintext node %d times", got)
	}

	allowed := NodeClient{Timeout: time.Second, AllowInsecureNodes: true, clients: clients}
	if err := allowed.Health(context.Background(), record, record.RootKey); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("opted-in client hits = %d, want 1", got)
	}
}

func TestNodeClientTransportBoundsConnections(t *testing.T) {
	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	for name, client := range map[string]*http.Client{"verified": clients.secure, "skip-verify": clients.insecure} {
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s transport type = %T", name, client.Transport)
		}
		if transport.MaxIdleConns != maxServerRecords || transport.MaxIdleConnsPerHost != maxNodeConnectionsPerHost || transport.MaxConnsPerHost != maxNodeConnectionsPerHost {
			t.Fatalf("%s transport bounds = idle %d, idle/host %d, max/host %d", name, transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost)
		}
	}
}

func TestNodeClientRejectsRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/healthz", http.StatusFound)
	}))
	defer redirect.Close()

	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	client := NodeClient{Timeout: time.Second, clients: clients}
	err := client.Health(context.Background(), testNodeRecord(redirect.URL), "rk_test")
	if !errors.Is(err, errNodeRedirect) {
		t.Fatalf("error = %v, want redirect rejection", err)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0", got)
	}
}

func TestNodeClientRejectsHTTPRedirectsWithOptIn(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/healthz", http.StatusFound)
	}))
	defer redirect.Close()

	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	client := NodeClient{Timeout: time.Second, AllowInsecureNodes: true, clients: clients}
	err := client.Health(context.Background(), testNodeRecord(redirect.URL), "rk_test")
	if !errors.Is(err, errNodeRedirect) {
		t.Fatalf("error = %v, want redirect rejection", err)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("HTTP redirect target hits = %d, want 0", got)
	}
}

func TestNodeClientCapsResponseBodies(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"padding":"` + strings.Repeat("x", 128) + `"}`))
		}))
		defer server.Close()

		clients := newNodeHTTPClients()
		defer clients.closeIdleConnections()
		client := NodeClient{Timeout: time.Second, clients: clients, responseLimit: 64}
		err := client.Health(context.Background(), testNodeRecord(server.URL), "rk_test")
		if err == nil || !strings.Contains(err.Error(), "response body exceeds 64 bytes") {
			t.Fatalf("error = %v, want oversized response error", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + strings.Repeat("x", 128) + `"}`))
		}))
		defer server.Close()

		clients := newNodeHTTPClients()
		defer clients.closeIdleConnections()
		client := NodeClient{Timeout: time.Second, clients: clients, errorResponseLimit: 64}
		err := client.Health(context.Background(), testNodeRecord(server.URL), "rk_test")
		if err == nil || !strings.Contains(err.Error(), "response body exceeds 64 bytes") {
			t.Fatalf("error = %v, want oversized error response", err)
		}
	})
}

func TestNodeClientPrevalidatesSnapshotCollectionBounds(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "top-level authorizations",
			body:    `{"authorizations":[` + repeatJSONElement(`{}`, maxNodeSnapshotRecords+1) + `]}`,
			wantErr: "node snapshot exceeds 4096 records",
		},
		{
			name:    "nested gpu processes",
			body:    `{"gpus":[{"processes":[` + repeatJSONElement(`{}`, maxNodeSnapshotProcesses+1) + `]}]}`,
			wantErr: "node snapshot exceeds 2048 processes",
		},
		{
			name:    "nested authorization command",
			body:    `{"authorizations":[{"command":[` + repeatJSONElement(`""`, maxNodeSnapshotRecords) + `]}]}`,
			wantErr: "node snapshot exceeds 4096 records",
		},
		{
			name:    "nested lease command",
			body:    `{"leases":[{"command":[` + repeatJSONElement(`""`, maxNodeSnapshotRecords) + `]}]}`,
			wantErr: "node snapshot exceeds 4096 records",
		},
		{
			name:    "unicode case-folding field",
			body:    `{"gpus":[{"proce\u017f\u017fe\u017f":[{}]}]}`,
			wantErr: "node snapshot field names must be ASCII",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			clients := newNodeHTTPClients()
			defer clients.closeIdleConnections()
			client := NodeClient{Timeout: time.Second, clients: clients}
			_, err := client.Snapshot(context.Background(), testNodeRecord(server.URL))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Snapshot error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestNodeClientDecodesBoundedSnapshot(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hostname":"node-a","gpus":[{"id":0,"processes":[{"pid":42}]}],"authorizations":[{"command":["echo","ok"]}]}`))
	}))
	defer server.Close()

	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	client := NodeClient{Timeout: time.Second, clients: clients}
	snapshot, err := client.Snapshot(context.Background(), testNodeRecord(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Hostname != "node-a" || len(snapshot.GPUs) != 1 || len(snapshot.GPUs[0].Processes) != 1 ||
		len(snapshot.Authorizations) != 1 || len(snapshot.Authorizations[0].Command) != 2 {
		t.Fatalf("decoded snapshot = %+v", snapshot)
	}
}

func TestNodeClientPrevalidatesKeyStatusCollectionBounds(t *testing.T) {
	for _, field := range []string{"tokens", "reservations", "authorizations", "bypasses"} {
		t.Run("top-level "+field, func(t *testing.T) {
			body := `{"` + field + `":[` + repeatJSONElement(`{}`, maxNodeSnapshotRecords+1) + `]}`
			assertShowKeysError(t, body, "node key status exceeds 4096 records")
		})
	}
	for _, field := range []string{"authorizations", "leases"} {
		t.Run("nested "+field+" command", func(t *testing.T) {
			body := `{"` + field + `":[{"command":[` + repeatJSONElement(`""`, maxNodeSnapshotRecords) + `]}]}`
			assertShowKeysError(t, body, "node key status exceeds 4096 records")
		})
	}
}

func TestNodeClientDecodesBoundedKeyStatus(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tokens":[{"id":"tok_1"}],"reservations":[{"id":"res_1"}],"authorizations":[{"id":"auth_1","command":["echo"]}],"bypasses":[{"id":"bypass_1"}]}`))
	}))
	defer server.Close()

	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	client := NodeClient{Timeout: time.Second, clients: clients}
	status, err := client.ShowKeys(context.Background(), testNodeRecord(server.URL), "rk_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Tokens) != 1 || len(status.Reservations) != 1 || len(status.Authorizations) != 1 ||
		len(status.Authorizations[0].Command) != 1 || len(status.Bypasses) != 1 {
		t.Fatalf("decoded key status = %+v", status)
	}
}

func TestNodeClientCapsGenericJSONTokens(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"values":[` + repeatJSONElement(`""`, maxGenericNodeJSONTokens) + `]}`))
	}))
	defer server.Close()

	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	client := NodeClient{Timeout: time.Second, clients: clients}
	err := client.Health(context.Background(), testNodeRecord(server.URL), "rk_test")
	if err == nil || !strings.Contains(err.Error(), "node response exceeds 65536 JSON tokens") {
		t.Fatalf("Health error = %v, want generic JSON token bound", err)
	}
}

func TestValidateGenericNodeJSONDepth(t *testing.T) {
	withinLimit := strings.Repeat("[", maxNodeSnapshotJSONDepth) + strings.Repeat("]", maxNodeSnapshotJSONDepth)
	if err := validateGenericNodeJSON([]byte(withinLimit)); err != nil {
		t.Fatalf("JSON at depth limit rejected: %v", err)
	}
	overLimit := "[" + withinLimit + "]"
	if err := validateGenericNodeJSON([]byte(overLimit)); err == nil || !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("JSON over depth limit error = %v, want nesting depth error", err)
	}
}

func assertShowKeysError(t *testing.T, body, want string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	client := NodeClient{Timeout: time.Second, clients: clients}
	_, err := client.ShowKeys(context.Background(), testNodeRecord(server.URL), "rk_test")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ShowKeys error = %v, want %q", err, want)
	}
}

func repeatJSONElement(element string, count int) string {
	if count <= 0 {
		return ""
	}
	return strings.Repeat(element+",", count-1) + element
}

func TestNodeClientAppliesTimeoutThroughContext(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer server.Close()

	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	client := NodeClient{Timeout: 20 * time.Millisecond, clients: clients}
	err := client.Health(context.Background(), testNodeRecord(server.URL), "rk_test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
}

func TestNodeClientDoesNotUseEnvironmentProxies(t *testing.T) {
	clients := newNodeHTTPClients()
	defer clients.closeIdleConnections()
	for name, client := range map[string]*http.Client{"secure": clients.secure, "insecure": clients.insecure} {
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s transport type = %T", name, client.Transport)
		}
		if transport.Proxy != nil {
			t.Fatalf("%s node transport honors environment proxy settings", name)
		}
	}
}

func TestJoinURLRequiresHTTPS(t *testing.T) {
	if _, err := joinURL("https://node.example:8192", "/healthz", false); err != nil {
		t.Fatalf("HTTPS endpoint rejected: %v", err)
	}
	for _, endpoint := range []string{
		"http://localhost:8192",
		"http://127.0.0.1:8192",
		"http://[::1]:8192",
		"http://node.example:8192",
		"http://10.0.0.5:8192",
		"http://127.0.0.1.example:8192",
	} {
		if _, err := joinURL(endpoint, "/healthz", false); err == nil || !strings.Contains(err.Error(), "use HTTPS") {
			t.Errorf("joinURL(%q) error = %v, want HTTPS requirement", endpoint, err)
		}
	}
}

func TestJoinURLAllowsHTTPWithExplicitOptIn(t *testing.T) {
	if _, err := joinURL("https://node.example:8192", "/healthz", true); err != nil {
		t.Fatalf("HTTPS endpoint rejected while HTTP opt-in was enabled: %v", err)
	}
	got, err := joinURL("http://node.example:8192", "/healthz", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://node.example:8192/healthz" {
		t.Fatalf("URL = %q", got)
	}
	for _, endpoint := range []string{
		"ftp://node.example:8192",
		"http://user:password@node.example:8192",
		"http://node.example:8192?target=other",
		"http://node.example:8192#fragment",
		"http:///missing-host",
	} {
		if _, err := joinURL(endpoint, "/healthz", true); err == nil {
			t.Errorf("opted-in joinURL(%q) unexpectedly succeeded", endpoint)
		}
	}
}

func testNodeRecord(endpoint string) ServerRecord {
	return ServerRecord{Name: "node-a", Endpoint: endpoint, RootKey: "rk_test", TLSSkipVerify: true}
}
