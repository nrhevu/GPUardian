package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuardian/internal/config"
	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
)

type cacheTestNodeClient struct {
	mu        sync.Mutex
	snapshots int
	started   chan struct{}
	block     chan struct{}
	snapshot  func(int) model.NodeSnapshot
}

func TestWebRunRejectsPlaintextWithoutExplicitOptIn(t *testing.T) {
	server := &Server{Cfg: config.Config{}, sessionKey: make([]byte, sessionKeyBytes)}
	err := server.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "GPUARDIAN_WEB_ALLOW_INSECURE") {
		t.Fatalf("Run error = %v, want explicit plaintext opt-in error", err)
	}
}

func TestNewWiresInsecureNodeOptIn(t *testing.T) {
	dir := t.TempDir()
	server := New(config.Config{
		WebAllowInsecureNodes: true,
		WebSessionKey:         filepath.Join(dir, "session.key"),
		WebRegistry:           filepath.Join(dir, "servers.json"),
		WebUsers:              filepath.Join(dir, "users.json"),
	})
	if _, err := server.Registry.Upsert(ServerRecord{Name: "node", Endpoint: "http://127.0.0.1:8192", RootKey: "rk_test"}); err != nil {
		t.Fatalf("registry opt-in was not wired: %v", err)
	}
	client, ok := server.Client.(NodeClient)
	if !ok || !client.AllowInsecureNodes {
		t.Fatalf("node client opt-in was not wired: %#v", server.Client)
	}
}

func TestInsecureNodeOptInActivatesLegacyRecordOnlyWhenEnabled(t *testing.T) {
	var hits int
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer node.Close()

	dir := t.TempDir()
	registryPath := filepath.Join(dir, "servers.json")
	record := ServerRecord{ID: "srv_legacy", Name: "legacy", Endpoint: node.URL, RootKey: "rk_test"}
	data, err := json.Marshal([]ServerRecord{record})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	baseConfig := config.Config{
		WebSessionKey: filepath.Join(dir, "session.key"),
		WebRegistry:   registryPath,
		WebUsers:      filepath.Join(dir, "users.json"),
	}

	blocked := New(baseConfig)
	records, err := blocked.Registry.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("load legacy record: records=%+v error=%v", records, err)
	}
	if err := blocked.Client.Health(context.Background(), records[0], records[0].RootKey); err == nil {
		t.Fatal("default gateway contacted a legacy plaintext node")
	}
	if hits != 0 {
		t.Fatalf("default gateway node hits = %d, want 0", hits)
	}

	baseConfig.WebAllowInsecureNodes = true
	allowed := New(baseConfig)
	records, err = allowed.Registry.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("reload opted-in legacy record: records=%+v error=%v", records, err)
	}
	if err := allowed.Client.Health(context.Background(), records[0], records[0].RootKey); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("opted-in gateway node hits = %d, want 1", hits)
	}
}

func (c *cacheTestNodeClient) Health(context.Context, ServerRecord, string) error {
	return nil
}

func (c *cacheTestNodeClient) Snapshot(context.Context, ServerRecord) (model.NodeSnapshot, error) {
	c.mu.Lock()
	c.snapshots++
	count := c.snapshots
	c.mu.Unlock()
	if c.started != nil {
		select {
		case c.started <- struct{}{}:
		default:
		}
	}
	if c.block != nil {
		<-c.block
	}
	if c.snapshot != nil {
		return c.snapshot(count), nil
	}
	return model.NodeSnapshot{
		Now:      time.Unix(int64(count), 0).UTC(),
		Hostname: "node-a",
	}, nil
}

func TestFleetSnapshotCanceledLeaderDoesNotPoisonRefresh(t *testing.T) {
	client := &cacheTestNodeClient{started: make(chan struct{}, 1), block: make(chan struct{})}
	server := New(config.Config{WebRegistry: filepath.Join(t.TempDir(), "servers.json")})
	server.Client = client
	if _, err := server.Registry.Upsert(ServerRecord{Name: "node-a", Endpoint: "https://node-a:8443", RootKey: "rk_test"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := server.cachedFleetSnapshot(ctx)
		firstDone <- err
	}()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("upstream snapshot did not start")
	}
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller error = %v, want context canceled", err)
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := server.cachedFleetSnapshot(context.Background())
		secondDone <- err
	}()
	close(client.block)
	if err := <-secondDone; err != nil {
		t.Fatalf("shared refresh was poisoned by canceled leader: %v", err)
	}
	if got := client.snapshotCount(); got != 1 {
		t.Fatalf("upstream snapshots = %d, want 1 shared refresh", got)
	}
}

func TestFleetSnapshotConcurrentRefreshIsShared(t *testing.T) {
	client := &cacheTestNodeClient{started: make(chan struct{}, 1), block: make(chan struct{})}
	server := New(config.Config{WebRegistry: filepath.Join(t.TempDir(), "servers.json")})
	server.Client = client
	if _, err := server.Registry.Upsert(ServerRecord{Name: "node-a", Endpoint: "https://node-a:8443", RootKey: "rk_test"}); err != nil {
		t.Fatal(err)
	}

	const callers = 12
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, err := server.cachedFleetSnapshot(context.Background())
			errs <- err
		}()
	}
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("upstream snapshot did not start")
	}
	time.Sleep(20 * time.Millisecond)
	if got := client.snapshotCount(); got != 1 {
		t.Fatalf("concurrent upstream snapshots = %d, want 1", got)
	}
	close(client.block)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestFleetSnapshotServesTwentyConcurrentUsersWithOneRefresh(t *testing.T) {
	const concurrentUsers = 20
	userRecords := make([]UserRecord, 0, concurrentUsers)
	tokens := make([]model.TokenView, 0, concurrentUsers)
	updatedAt := time.Now().UTC()
	for index := range concurrentUsers {
		username := fmt.Sprintf("user-%02d", index)
		userRecords = append(userRecords, UserRecord{
			Username:  username,
			Role:      RoleUser,
			CreatedAt: updatedAt,
			UpdatedAt: updatedAt,
		})
		tokens = append(tokens, model.TokenView{ID: fmt.Sprintf("token-%02d", index), Name: username, Mode: model.TokenModeClaimed})
	}
	processes := make([]model.GPUProcess, 256)
	for index := range processes {
		processes[index] = model.GPUProcess{GPU: 0, PID: index + 1, Name: "hidden-process"}
	}
	client := &cacheTestNodeClient{
		started: make(chan struct{}, 1),
		block:   make(chan struct{}),
		snapshot: func(int) model.NodeSnapshot {
			return model.NodeSnapshot{
				GPUs:   []model.GPUSnapshot{{ID: 0, State: "available", Processes: processes}},
				Tokens: tokens,
			}
		},
	}
	dir := t.TempDir()
	server := New(config.Config{
		WebSessionKey: filepath.Join(dir, "session.key"),
		WebRegistry:   filepath.Join(dir, "servers.json"),
		WebUsers:      filepath.Join(dir, "users.json"),
	})
	server.Client = client
	server.Users.mu.Lock()
	server.Users.loaded = true
	server.Users.users = userRecords
	server.Users.mu.Unlock()
	if _, err := server.Registry.Upsert(ServerRecord{Name: "node", Endpoint: "https://node:8192", RootKey: "rk_test"}); err != nil {
		t.Fatal(err)
	}

	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(client.block) }) }
	t.Cleanup(releaseRefresh)
	type result struct {
		username string
		code     int
		body     []byte
	}
	start := make(chan struct{})
	results := make(chan result, concurrentUsers)
	handler := server.routes()
	for _, user := range userRecords {
		username := user.Username
		cookie := server.signSession(username, RoleUser, time.Now().Add(time.Hour))
		go func() {
			<-start
			request := httptest.NewRequest(http.MethodGet, "/api/fleet/snapshot", nil)
			request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			results <- result{username: username, code: response.Code, body: append([]byte(nil), response.Body.Bytes()...)}
		}()
	}
	close(start)
	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("fleet refresh did not start")
	}
	activeDeadline := time.Now().Add(2 * time.Second)
	for {
		server.requestMu.Lock()
		activeTotal := server.activeTotal
		activeUsers := len(server.activeUsers)
		server.requestMu.Unlock()
		if activeTotal == concurrentUsers && activeUsers == concurrentUsers {
			break
		}
		if time.Now().After(activeDeadline) {
			t.Fatalf("simultaneous admission = %d requests across %d users, want %d", activeTotal, activeUsers, concurrentUsers)
		}
		time.Sleep(time.Millisecond)
	}
	if got := client.snapshotCount(); got != 1 {
		t.Fatalf("upstream snapshot calls while users waited = %d, want 1", got)
	}
	releaseRefresh()
	for range concurrentUsers {
		select {
		case got := <-results:
			if got.code != http.StatusOK {
				t.Fatalf("%s response = %d body=%s", got.username, got.code, got.body)
			}
			var snapshot fleetSnapshot
			if err := json.Unmarshal(got.body, &snapshot); err != nil {
				t.Fatalf("decode %s response: %v", got.username, err)
			}
			if len(snapshot.Servers) != 1 || snapshot.Servers[0].Snapshot == nil {
				t.Fatalf("%s snapshot = %+v", got.username, snapshot)
			}
			visible := snapshot.Servers[0].Snapshot
			if len(visible.Tokens) != 1 || visible.Tokens[0].Name != got.username {
				t.Fatalf("%s visible tokens = %+v", got.username, visible.Tokens)
			}
			if len(visible.GPUs) != 1 || len(visible.GPUs[0].Processes) != 0 {
				t.Fatalf("%s received hidden process data", got.username)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent fleet requests did not finish")
		}
	}
	server.requestMu.Lock()
	activeTotal := server.activeTotal
	activeUsers := len(server.activeUsers)
	server.requestMu.Unlock()
	if activeTotal != 0 || activeUsers != 0 {
		t.Fatalf("admission counters did not drain: total=%d users=%d", activeTotal, activeUsers)
	}
	if got := client.snapshotCount(); got != 1 {
		t.Fatalf("upstream snapshot calls = %d, want 1", got)
	}
}

func TestFilterFleetSnapshotDoesNotMutateCachedAdminData(t *testing.T) {
	reservation := &model.ReservationView{ID: "res_bob", GroupID: "tok_bob", Holder: "bob"}
	claim := &model.SoftClaimView{ID: "claim_bob", Holder: "bob"}
	input := fleetSnapshot{Servers: []serverSnapshot{{Snapshot: &model.NodeSnapshot{
		GPUs: []model.GPUSnapshot{{
			ID:          0,
			Processes:   []model.GPUProcess{{GPU: 0, PID: 123, Name: "private-command"}},
			Reservation: reservation,
			Claim:       claim,
		}},
		PS: []model.PSRow{{ID: "auth_bob", User: "bob", Command: "private-command"}},
	}}}}

	filtered := filterFleetSnapshot(input, sessionInfo{User: "alice", Role: RoleUser})
	if len(filtered.Servers[0].Snapshot.GPUs[0].Processes) != 0 || len(filtered.Servers[0].Snapshot.PS) != 0 {
		t.Fatal("non-admin snapshot exposed process details")
	}
	if filtered.Servers[0].Snapshot.GPUs[0].Claim != nil {
		t.Fatal("non-admin snapshot exposed another user's claim")
	}
	if got := input.Servers[0].Snapshot.GPUs[0]; len(got.Processes) != 1 || got.Claim == nil || got.Reservation.GroupID != "tok_bob" {
		t.Fatalf("cached admin snapshot was mutated: %+v", got)
	}
}

func (c *cacheTestNodeClient) CreateReservation(context.Context, ServerRecord, protocol.RegisterArgs) (model.RegisterResult, error) {
	return model.RegisterResult{}, nil
}

func (c *cacheTestNodeClient) CreateClaimKey(context.Context, ServerRecord, protocol.RegisterArgs) (model.RegisterResult, error) {
	return model.RegisterResult{}, nil
}

func (c *cacheTestNodeClient) ShowKeys(context.Context, ServerRecord, string) (model.KeyStatus, error) {
	return model.KeyStatus{}, nil
}

func (c *cacheTestNodeClient) Allow(context.Context, ServerRecord, protocol.AllowArgs) (model.AllowResult, error) {
	return model.AllowResult{}, nil
}

func (c *cacheTestNodeClient) Revoke(context.Context, ServerRecord, protocol.RevokeArgs) (map[string]string, error) {
	return nil, nil
}

func (c *cacheTestNodeClient) snapshotCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshots
}

func TestFleetSnapshotUsesOneSecondCache(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	client := &cacheTestNodeClient{}
	server := New(config.Config{WebRegistry: filepath.Join(t.TempDir(), "servers.json")})
	server.Client = client
	server.now = func() time.Time { return now }

	if _, err := server.Registry.Upsert(ServerRecord{
		Name:     "node-a",
		Endpoint: "https://node-a:8443",
		RootKey:  "rk_test",
	}); err != nil {
		t.Fatal(err)
	}

	first, err := server.cachedFleetSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.cachedFleetSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := client.snapshotCount(); got != 1 {
		t.Fatalf("snapshot calls = %d, want 1", got)
	}
	if !first.Servers[0].Snapshot.Now.Equal(second.Servers[0].Snapshot.Now) {
		t.Fatalf("second snapshot was not served from cache")
	}

	now = now.Add(time.Second)
	third, err := server.cachedFleetSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := client.snapshotCount(); got != 2 {
		t.Fatalf("snapshot calls after cache expiry = %d, want 2", got)
	}
	if !third.Servers[0].Snapshot.Now.After(second.Servers[0].Snapshot.Now) {
		t.Fatalf("third snapshot did not refresh after cache expiry")
	}
}

func TestFleetSnapshotReturnsPartialErrorsWhenProcessBudgetIsExceeded(t *testing.T) {
	client := &cacheTestNodeClient{snapshot: func(int) model.NodeSnapshot {
		return model.NodeSnapshot{GPUs: []model.GPUSnapshot{{
			ID:        0,
			Processes: make([]model.GPUProcess, maxNodeSnapshotProcesses),
		}}}
	}}
	server := New(config.Config{WebRegistry: filepath.Join(t.TempDir(), "servers.json")})
	server.Client = client
	for i := 0; i < 5; i++ {
		if _, err := server.Registry.Upsert(ServerRecord{
			Name:     fmt.Sprintf("node-%d", i),
			Endpoint: fmt.Sprintf("https://node-%d:8443", i),
			RootKey:  "rk_test",
		}); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := server.fetchFleetSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	online, failed := 0, 0
	for _, item := range snapshot.Servers {
		if item.Online && item.Snapshot != nil {
			online++
		} else if strings.Contains(item.Error, "fleet snapshot budget exceeded") {
			failed++
		}
	}
	if online != 4 || failed != 1 {
		t.Fatalf("budgeted fleet results: online=%d failed=%d, want 4 and 1", online, failed)
	}
}

func TestFleetSnapshotBudgetsBoundRecordsAndRetainedMemory(t *testing.T) {
	t.Run("error strings", func(t *testing.T) {
		message := boundedFleetError(errors.New(strings.Repeat("x", maxFleetErrorBytes+1)))
		if len(message) != maxFleetErrorBytes {
			t.Fatalf("bounded error length = %d, want %d", len(message), maxFleetErrorBytes)
		}
	})

	t.Run("node records", func(t *testing.T) {
		_, _, _, err := nodeSnapshotCost(model.NodeSnapshot{PS: make([]model.PSRow, maxNodeSnapshotRecords+1)})
		if err == nil || !strings.Contains(err.Error(), "records") {
			t.Fatalf("oversized node snapshot error = %v", err)
		}
	})

	t.Run("fleet records", func(t *testing.T) {
		budget := &fleetSnapshotBudget{}
		snapshot := model.NodeSnapshot{PS: make([]model.PSRow, maxNodeSnapshotRecords)}
		for i := 0; i < maxFleetSnapshotRecords/maxNodeSnapshotRecords; i++ {
			if err := budget.accept(snapshot); err != nil {
				t.Fatalf("record budget rejected snapshot %d early: %v", i, err)
			}
		}
		if err := budget.accept(snapshot); err == nil {
			t.Fatal("fleet record budget accepted an oversized aggregate")
		}
	})

	t.Run("fleet retained memory", func(t *testing.T) {
		budget := &fleetSnapshotBudget{}
		snapshot := model.NodeSnapshot{Hostname: strings.Repeat("x", 1<<20)}
		var rejected bool
		for range 40 {
			if err := budget.accept(snapshot); err != nil {
				rejected = true
				break
			}
		}
		if !rejected {
			t.Fatal("fleet retained-memory budget accepted an oversized aggregate")
		}
	})
}
