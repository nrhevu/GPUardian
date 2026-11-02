package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gpuardian/internal/config"
	"gpuardian/internal/history"
	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
)

func TestStaticAssetsSetCacheControl(t *testing.T) {
	uiDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(uiDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"index.html":             "<!doctype html><title>GPUardian</title>",
		"assets/index-abc123.js": "console.log('gpuardian');",
		"theme-init.js":          "void 0;",
	} {
		if err := os.WriteFile(filepath.Join(uiDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	server := New(config.Config{WebRegistry: filepath.Join(t.TempDir(), "servers.json"), WebUIDir: uiDir})
	handler := server.routes()
	tests := []struct {
		path string
		want string
	}{
		{path: "/assets/index-abc123.js", want: "public, max-age=31536000, immutable"},
		{path: "/", want: "no-cache"},
		{path: "/theme-init.js", want: "no-cache"},
		{path: "/assets/missing-123.js", want: "no-cache"},
		{path: "/schedule", want: "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d", recorder.Code)
			}
			if got := recorder.Header().Get("Cache-Control"); got != tt.want {
				t.Fatalf("Cache-Control = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFleetSnapshotCompactDropsUnusedDetailWithoutTouchingCache(t *testing.T) {
	server, client, _ := newAuthzServer(t)
	client.snapshot.PS = []model.PSRow{{ID: "ps_1", GPU: "0", User: "alice", Command: "python train.py"}}
	client.snapshot.Leases = []model.Lease{{ID: "lease_1", Mode: model.ModeUser, Holder: "alice"}}
	handler := server.routes()
	adminCookie := testSessionCookie(t, server, "admin", RoleAdmin)

	decode := func(response *httptest.ResponseRecorder) model.NodeSnapshot {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		var out fleetSnapshot
		if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Servers) != 1 || out.Servers[0].Snapshot == nil {
			t.Fatalf("fleet = %+v", out)
		}
		return *out.Servers[0].Snapshot
	}
	compact := decode(requestJSON(handler, http.MethodGet, "/api/fleet/snapshot?compact=1", "", adminCookie))
	if len(compact.Tokens) != 0 || len(compact.PS) != 0 || len(compact.Leases) != 0 {
		t.Fatalf("compact snapshot kept unused detail: %+v", compact)
	}
	if len(compact.Reservations) != 2 {
		t.Fatalf("compact snapshot reservations = %+v", compact.Reservations)
	}
	full := decode(requestJSON(handler, http.MethodGet, "/api/fleet/snapshot", "", adminCookie))
	if len(full.Tokens) != 2 || len(full.PS) != 1 || len(full.Leases) != 1 {
		t.Fatalf("compact request modified the shared cache: %+v", full)
	}
}

func TestFixedKeysIncludeNodeSyncOnlyOnRequest(t *testing.T) {
	server, _, _ := newAuthzServer(t)
	if err := server.Users.InitializeFixedKeys(bytes.Repeat([]byte{0x44}, 32)); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()
	cookie := testSessionCookie(t, server, "alice", RoleUser)
	tests := []struct {
		path string
		want bool
	}{
		{path: "/api/keys", want: false},
		{path: "/api/keys?include=node_sync", want: true},
		{path: "/api/keys?include=other,node_sync", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			response := requestJSON(handler, http.MethodGet, tt.path, "", cookie)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			var keys []FixedUserKey
			if err := json.Unmarshal(response.Body.Bytes(), &keys); err != nil {
				t.Fatal(err)
			}
			if len(keys) != 1 {
				t.Fatalf("keys = %+v", keys)
			}
			if got := len(keys[0].NodeSync) > 0; got != tt.want {
				t.Fatalf("node_sync present = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFleetSnapshotServesRecentDataWhileRefreshIsSlow(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	var clockMu sync.Mutex
	client := &cacheTestNodeClient{}
	server := New(config.Config{WebRegistry: filepath.Join(t.TempDir(), "servers.json")})
	server.Client = client
	server.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}
	if _, err := server.Registry.Upsert(ServerRecord{Name: "node-a", Endpoint: "https://node-a:8443", RootKey: "rk_test"}); err != nil {
		t.Fatal(err)
	}
	first, err := server.cachedFleetSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	client.block = make(chan struct{})
	advance(2 * time.Second)
	started := time.Now()
	stale, err := server.cachedFleetSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > fleetStaleWait+time.Second {
		t.Fatalf("stale snapshot took %v", elapsed)
	}
	if !stale.Servers[0].Snapshot.Now.Equal(first.Servers[0].Snapshot.Now) {
		t.Fatal("slow refresh did not fall back to the recent snapshot")
	}
	close(client.block)

	// A mutation must never be answered with pre-mutation data.
	server.clearFleetCache()
	advance(2 * time.Second)
	fresh, err := server.cachedFleetSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Servers[0].Snapshot.Now.After(first.Servers[0].Snapshot.Now) {
		t.Fatal("snapshot after invalidation was served from stale data")
	}
}

type countingManagedNodeClient struct {
	*managedAuthzNodeClient
	syncs int
}

func (c *countingManagedNodeClient) SyncManagedUserKeys(ctx context.Context, record ServerRecord, snapshot protocol.ManagedUserKeySnapshot) (protocol.ManagedUserKeySyncResult, error) {
	c.mu.Lock()
	c.syncs++
	c.mu.Unlock()
	return c.managedAuthzNodeClient.SyncManagedUserKeys(ctx, record, snapshot)
}

func (c *countingManagedNodeClient) syncCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.syncs
}

func TestEnsureManagedKeysSkipsNodesHoldingCurrentSnapshot(t *testing.T) {
	server, legacy, serverID := newAuthzServer(t)
	if err := server.Users.InitializeFixedKeys(bytes.Repeat([]byte{0x55}, 32)); err != nil {
		t.Fatal(err)
	}
	client := &countingManagedNodeClient{managedAuthzNodeClient: &managedAuthzNodeClient{authzNodeClient: legacy}}
	server.Client = client
	record, ok, err := server.Registry.Get(serverID)
	if err != nil || !ok {
		t.Fatalf("load server: found=%v err=%v", ok, err)
	}

	steps := []struct {
		name      string
		before    func()
		wantSyncs int
	}{
		{name: "first use syncs", wantSyncs: 1},
		{name: "current snapshot is reused", wantSyncs: 1},
		{name: "key set change resyncs", before: func() {
			if _, err := server.Users.Create("bob", "test-password-strong", RoleUser); err != nil {
				t.Fatal(err)
			}
		}, wantSyncs: 2},
		{name: "failed sync is retried", before: func() {
			server.recordManagedKeySync(record.ID, "", context.DeadlineExceeded)
		}, wantSyncs: 3},
	}
	for _, step := range steps {
		if step.before != nil {
			step.before()
		}
		if err := server.ensureManagedKeysOnNode(t.Context(), record); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := client.syncCount(); got != step.wantSyncs {
			t.Fatalf("%s: syncs = %d, want %d", step.name, got, step.wantSyncs)
		}
	}
}

func TestHistorySessionTimelineLimitValidation(t *testing.T) {
	server, _, _ := newAuthzServer(t)
	store, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server.History = store
	start := time.Now().UTC().Add(-time.Hour)
	if err := store.PrepareSession(t.Context(), "sess_limit", "node-a", "server-a", "GPU node", "alice", "training", start, start.Add(30*time.Minute), []int{0}); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmSession(t.Context(), "sess_limit", "group-limit", []string{"reservation-limit"}, []int{0}); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()
	cookie := testSessionCookie(t, server, "alice", RoleUser)
	tests := []struct {
		query string
		want  int
	}{
		{query: "", want: http.StatusOK},
		{query: "?timeline_limit=180", want: http.StatusOK},
		{query: "?timeline_limit=1000000", want: http.StatusOK},
		{query: "?timeline_limit=0", want: http.StatusBadRequest},
		{query: "?timeline_limit=-5", want: http.StatusBadRequest},
		{query: "?timeline_limit=all", want: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			response := requestJSON(handler, http.MethodGet, "/api/history/sessions/sess_limit"+tt.query, "", cookie)
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d body=%s", response.Code, tt.want, response.Body.String())
			}
		})
	}
}
