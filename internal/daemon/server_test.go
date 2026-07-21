package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuardian/internal/config"
	"gpuardian/internal/enforce"
	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
	"gpuardian/internal/store"
	"gpuardian/internal/telemetry"
)

type fakeAMD struct {
	processes []model.GPUProcess
}

func (f fakeAMD) Processes(context.Context) ([]model.GPUProcess, error) {
	return f.processes, nil
}

type failingAMD struct {
	err error
}

func (f failingAMD) Processes(context.Context) ([]model.GPUProcess, error) {
	return nil, f.err
}

type countingAMD struct {
	calls int
}

func (f *countingAMD) Processes(context.Context) ([]model.GPUProcess, error) {
	f.calls++
	return nil, nil
}

type daemonFakeProc struct {
	infos map[int]model.ProcInfo
}

func (f daemonFakeProc) Exists(pid int) bool {
	_, ok := f.infos[pid]
	return ok
}

func (f daemonFakeProc) Info(pid int) (model.ProcInfo, error) {
	info, ok := f.infos[pid]
	if !ok {
		return model.ProcInfo{}, errors.New("missing")
	}
	return info, nil
}

type daemonFakeKiller struct {
	killed []int
}

func (f *daemonFakeKiller) Kill(info model.ProcInfo, message string) error {
	f.killed = append(f.killed, info.PID)
	return nil
}

type daemonFakeRuntime struct{}

func (daemonFakeRuntime) ResolveDockerContainer(context.Context, string) (string, error) {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
}

func (daemonFakeRuntime) DockerContainerName(context.Context, string) (string, error) {
	return "trainer", nil
}

func (daemonFakeRuntime) NamespaceForContainer(context.Context, string) (string, error) {
	return "training", nil
}

func TestNodeHTTPRejectsPlaintextWithoutExplicitOptIn(t *testing.T) {
	server := testServer(t)
	server.Cfg.NodeAddr = "127.0.0.1:0"
	closeServer, err := server.startNodeHTTP(context.Background())
	if closeServer != nil {
		closeServer()
	}
	if err == nil || !strings.Contains(err.Error(), "GPUARDIAN_NODE_ALLOW_INSECURE") {
		t.Fatalf("startNodeHTTP error = %v, want explicit plaintext opt-in error", err)
	}
}

type daemonMissingDockerRuntime struct{}

func (daemonMissingDockerRuntime) ResolveDockerContainer(context.Context, string) (string, error) {
	return "", errors.New("container not found")
}

func (daemonMissingDockerRuntime) DockerContainerName(context.Context, string) (string, error) {
	return "future-container", nil
}

func (daemonMissingDockerRuntime) NamespaceForContainer(context.Context, string) (string, error) {
	return "", errors.New("namespace not found")
}

func TestRegisterRPC(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	client, srv := net.Pipe()
	defer client.Close()
	go server.handleConn(context.Background(), srv)

	args, _ := json.Marshal(protocol.RegisterArgs{RootKey: key, Mode: model.TokenModeClaimed, Name: "alice", TTL: "1h"})
	req, _ := json.Marshal(protocol.Request{ID: "1", Method: "register", Args: args})
	if _, err := client.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}
	var resp protocol.Response
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("register failed: %s", resp.Error)
	}
	var result model.RegisterResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Token == "" {
		t.Fatal("empty token")
	}
}

func TestReservationValidationUsesConfiguredInventoryAndOneSample(t *testing.T) {
	server := testServer(t)
	server.Cfg.GPUCount = 2
	provider := &countingAMD{}
	server.GPU = provider
	now := time.Now()
	if err := server.ensureGPUsCanReserveWindow(context.Background(), []int{0, 1}, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 {
		t.Fatalf("amd process samples = %d, want 1", provider.calls)
	}
	if err := server.ensureGPUsCanReserveWindow(context.Background(), []int{2}, now, now.Add(time.Hour)); err == nil {
		t.Fatal("out-of-inventory GPU unexpectedly accepted")
	}
}

func TestNonRootStatusRequiresTokenAndFiltersOtherKeys(t *testing.T) {
	server := testServer(t)
	rootKey, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	aliceSecret, aliceToken, err := server.Store.RegisterSoftToken(rootKey, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, bobToken, err := server.Store.RegisterSoftToken(rootKey, "bob", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []model.Token{aliceToken, bobToken} {
		if err := server.Store.AddAuthorization(model.Authorization{
			ID: store.NewAuthorizationID(), Mode: model.ModeUser, TokenHash: token.Hash,
			TokenMode: token.Mode, Holder: token.Name, UID: 1000, CreatedAt: time.Now(), Active: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := server.dispatch(context.Background(), peer{UID: 1000, GID: 1000}, protocol.Request{Method: "status"}); err == nil {
		t.Fatal("non-root tokenless status unexpectedly succeeded")
	}
	result, err := server.dispatch(context.Background(), peer{UID: 1000, GID: 1000}, protocol.Request{Method: "status", Token: aliceSecret})
	if err != nil {
		t.Fatal(err)
	}
	status := result.(model.Status)
	if len(status.Tokens) != 1 || status.Tokens[0].ID != aliceToken.ID {
		t.Fatalf("status exposed other tokens: %+v", status.Tokens)
	}
	if len(status.Authorizations) != 1 || status.Authorizations[0].TokenID != aliceToken.ID {
		t.Fatalf("status exposed other authorizations: %+v", status.Authorizations)
	}
	if len(status.Bypasses) != 0 || len(status.Leases) != 0 {
		t.Fatalf("status exposed administrative rows: %+v", status)
	}
}

func TestHardRegisterCreatesReservation(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.RegisterArgs{RootKey: key, Mode: model.TokenModeReserved, Name: "alice", GPUs: []int{2}, TTL: "1h"})
	result, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "register", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	register := result.(model.RegisterResult)
	if register.Token == "" || register.TokenID == "" || len(register.ReservationIDs) != 1 || len(register.GPUs) != 1 || register.GPUs[0] != 2 {
		t.Fatalf("unexpected register result: %+v", register)
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Reservations) != 1 || status.Reservations[0].GPU != 2 {
		t.Fatalf("expected reserved reservation, got %+v", status.Reservations)
	}
}

func TestHardRegisterCreatesMultipleReservations(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.RegisterArgs{RootKey: key, Mode: model.TokenModeReserved, Name: "alice", GPUs: []int{0, 1}, TTL: "1h"})
	result, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "register", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	register := result.(model.RegisterResult)
	if register.Token == "" || register.TokenID == "" || len(register.ReservationIDs) != 2 || len(register.GPUs) != 2 || register.GPUs[0] != 0 || register.GPUs[1] != 1 {
		t.Fatalf("unexpected register result: %+v", register)
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Reservations) != 2 || status.Reservations[0].GPU != 0 || status.Reservations[1].GPU != 1 {
		t.Fatalf("expected reserved reservations, got %+v", status.Reservations)
	}
}

func TestHardRegisterBusyGPUFailsWithoutReservation(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	server.GPU = fakeAMD{processes: []model.GPUProcess{{GPU: 2, PID: 10, MemBytes: 1}}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000, Cmdline: []string{"python", "--password", "do-not-leak", "\x1b[31m"}}}}
	args, _ := json.Marshal(protocol.RegisterArgs{RootKey: key, Mode: model.TokenModeReserved, Name: "alice", GPUs: []int{1, 2}, TTL: "1h"})
	if _, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "register", Args: args}); err == nil {
		t.Fatal("expected busy gpu error")
	} else if err.Error() != "gpu 2 is busy" {
		t.Fatalf("busy error exposed process details: %q", err)
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Reservations) != 0 {
		t.Fatalf("busy reserved register should not create reservation: %+v", status.Reservations)
	}
}

func TestHardRegisterIgnoresParentOnlyGPUProcess(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	server.GPU = fakeAMD{processes: []model.GPUProcess{{GPU: 2, PID: 10, MemBytes: 0}}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000, Cmdline: []string{"python", "-m", "launcher"}}}}
	args, _ := json.Marshal(protocol.RegisterArgs{RootKey: key, Mode: model.TokenModeReserved, Name: "alice", GPUs: []int{2}, TTL: "1h"})
	if _, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "register", Args: args}); err != nil {
		t.Fatal(err)
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Reservations) != 1 || status.Reservations[0].GPU != 2 {
		t.Fatalf("expected reservation despite parent-only process: %+v", status.Reservations)
	}
}

func TestSoftRegisterCreatesTokenOnly(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.RegisterArgs{RootKey: key, Mode: model.TokenModeClaimed, Name: "alice"})
	result, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "register", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	register := result.(model.RegisterResult)
	if register.Token == "" || register.Mode != model.TokenModeClaimed || register.ExpiresAt != nil {
		t.Fatalf("unexpected claimed register result: %+v", register)
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Tokens) != 1 || len(status.Reservations) != 0 {
		t.Fatalf("expected token only, got tokens=%+v reservations=%+v", status.Tokens, status.Reservations)
	}
}

func TestReservedTokenOnlyAuthorizesDuringWindow(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	start := now.Add(time.Hour)
	secret, _, _, err := server.Store.RegisterScheduledReservations(key, "alice", "", []int{0}, start, start.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	token, tokenHash, err := server.Store.ValidateToken(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ensureTokenCanAuthorize(tokenHash, token, now); err == nil {
		t.Fatal("reserved token should not authorize before starts_at")
	}
	if err := server.ensureTokenCanAuthorize(tokenHash, token, start.Add(time.Minute)); err != nil {
		t.Fatalf("reserved token should authorize during window: %v", err)
	}
	if err := server.ensureTokenCanAuthorize(tokenHash, token, start.Add(2*time.Hour)); err == nil {
		t.Fatal("reserved token should not authorize after reservation expires")
	}
}

func TestNodeHTTPRequiresBearerRootKey(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	handler := server.nodeHTTPHandler()

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer got %d, want 401", missing.Code)
	}

	badReq := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	badReq.Header.Set("Authorization", "Bearer bad")
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, badReq)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad bearer got %d, want 401", bad.Code)
	}

	goodReq := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	goodReq.Header.Set("Authorization", "Bearer "+key)
	good := httptest.NewRecorder()
	handler.ServeHTTP(good, goodReq)
	if good.Code != http.StatusOK {
		t.Fatalf("valid bearer got %d: %s", good.Code, good.Body.String())
	}
}

func TestNodeHTTPTelemetryInfoAndPage(t *testing.T) {
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
	defer server.Telemetry.Close()
	if _, err := server.Telemetry.Append(telemetry.EventDaemonStarted, map[string]bool{"ok": true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	handler := server.nodeHTTPHandler()
	for _, endpoint := range []string{"/api/v1/info", "/api/v1/telemetry?limit=1"} {
		request := httptest.NewRequest(http.MethodGet, endpoint, nil)
		request.Header.Set("Authorization", "Bearer "+key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", endpoint, response.Code, response.Body.String())
		}
		if endpoint == "/api/v1/info" && (!strings.Contains(response.Body.String(), `"node_id":"node_`) || !strings.Contains(response.Body.String(), `"telemetry_v1"`) || !strings.Contains(response.Body.String(), `"managed_user_keys_v1"`)) {
			t.Fatalf("unexpected info response: %s", response.Body.String())
		}
		if strings.HasPrefix(endpoint, "/api/v1/telemetry") && !strings.Contains(response.Body.String(), `"daemon.started"`) {
			t.Fatalf("unexpected telemetry response: %s", response.Body.String())
		}
	}
}

func TestNodeHTTPManagedKeySyncAndReservation(t *testing.T) {
	server := testServer(t)
	rootKey, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	secret := "rg_" + strings.Repeat("a", 48)
	snapshot := protocol.ManagedUserKeySnapshot{SnapshotID: "sha256:test", Keys: []protocol.ManagedUserKey{{ID: "uk_alice", Owner: "alice", Version: 1, Hash: store.HashToken(secret)}}}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/user-keys/sync", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+rootKey)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.nodeHTTPHandler().ServeHTTP(response, req)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"managed":true`) {
		t.Fatalf("sync status=%d body=%s", response.Code, response.Body.String())
	}

	now := time.Now().UTC()
	startsAt, expiresAt := now.Add(time.Minute), now.Add(time.Hour)
	reservationBody, err := json.Marshal(protocol.RegisterArgs{Name: "mallory", UserKeyID: "uk_alice", Purpose: "training", GPUs: []int{0, 1}, StartsAt: &startsAt, ExpiresAt: &expiresAt})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/reservations", strings.NewReader(string(reservationBody)))
	req.Header.Set("Authorization", "Bearer "+rootKey)
	req.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.nodeHTTPHandler().ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("reservation status=%d body=%s", response.Code, response.Body.String())
	}
	var result model.RegisterResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Token != "" || result.TokenID != "uk_alice" || result.GroupID == "" || len(result.ReservationIDs) != 2 {
		t.Fatalf("unexpected managed reservation result: %+v", result)
	}
	status, err := server.Store.KeyStatus(rootKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Tokens) != 1 || status.Tokens[0].Key != "" || status.Tokens[0].Name != "alice" || !status.Tokens[0].Managed {
		t.Fatalf("managed verifier leaked or owner changed: %+v", status.Tokens)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/claim-keys", strings.NewReader(`{"name":"alice"}`))
	req.Header.Set("Authorization", "Bearer "+rootKey)
	req.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.nodeHTTPHandler().ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("legacy claim key remained enabled: %d %s", response.Code, response.Body.String())
	}
}

func TestObservedTelemetryJobGroupsGPUsAndDebouncesFinish(t *testing.T) {
	server := testServer(t)
	dir := t.TempDir()
	box, err := telemetry.Open(filepath.Join(dir, "node.id"), filepath.Join(dir, "outbox"), "boot-test")
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	server.Telemetry = box
	server.bootID = "boot-test"
	state := model.State{
		Tokens:         []model.Token{{ID: "tok_group", Hash: "hash", Mode: model.TokenModeReserved}},
		Authorizations: []model.Authorization{{ID: "auth_private", TokenHash: "hash", TokenMode: model.TokenModeReserved, Mode: model.ModeDocker, Holder: "alice"}},
	}
	info := model.ProcInfo{PID: 42, StartTime: 99, Cmdline: []string{"python", "train.py"}}
	decisions := []enforce.Decision{
		{Action: "allow", AuthID: "auth_private", Process: model.GPUProcess{PID: 42, GPU: 0}, Info: info},
		{Action: "allow", AuthID: "auth_private", Process: model.GPUProcess{PID: 42, GPU: 1}, Info: info},
	}
	now := time.Now().UTC()
	server.trackObservedTelemetryJobs(state, decisions, now)
	server.trackObservedTelemetryJobs(state, nil, now.Add(time.Second))
	server.trackObservedTelemetryJobs(state, nil, now.Add(2*time.Second))
	page, err := box.Page("", 10)
	if err != nil {
		t.Fatal(err)
	}
	var started, updated, finished telemetry.JobEvent
	for _, event := range page.Events {
		switch event.Type {
		case telemetry.EventJobStarted:
			if err := json.Unmarshal(event.Payload, &started); err != nil {
				t.Fatal(err)
			}
		case telemetry.EventJobUpdated:
			if err := json.Unmarshal(event.Payload, &updated); err != nil {
				t.Fatal(err)
			}
		case telemetry.EventJobFinished:
			if err := json.Unmarshal(event.Payload, &finished); err != nil {
				t.Fatal(err)
			}
		}
	}
	if started.ExecutionID == "" || updated.ExecutionID != started.ExecutionID || finished.ExecutionID != started.ExecutionID || len(updated.GPUs) != 2 || finished.FinishedAt == nil {
		t.Fatalf("unexpected observed lifecycle: started=%+v updated=%+v finished=%+v", started, updated, finished)
	}
}

func TestNodeHTTPReservationPreservesOwner(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	startsAt := now.Add(time.Hour)
	expiresAt := startsAt.Add(time.Hour)
	body, err := json.Marshal(protocol.RegisterArgs{
		Name:      " alice ",
		Purpose:   "training",
		GPUs:      []int{0},
		StartsAt:  &startsAt,
		ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/reservations", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.nodeHTTPHandler().ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("reservation status = %d, body=%s", response.Code, response.Body.String())
	}
	status, err := server.Store.Status(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Reservations) != 1 || status.Reservations[0].Holder != "alice" {
		t.Fatalf("reservations = %+v, want holder alice", status.Reservations)
	}

	missingOwner, err := json.Marshal(protocol.RegisterArgs{
		GPUs:      []int{1},
		StartsAt:  &startsAt,
		ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/reservations", strings.NewReader(string(missingOwner)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.nodeHTTPHandler().ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing owner status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestScheduledReservationRejectsOverlappingLegacyLease(t *testing.T) {
	server := testServer(t)
	now := time.Now().UTC()
	lease := model.Lease{
		ID:        "lease_existing",
		GPU:       0,
		Mode:      model.ModeBare,
		Holder:    "alice",
		CreatedAt: now.Add(-time.Minute),
		ExpiresAt: now.Add(2 * time.Hour),
		Active:    true,
	}
	if err := server.Store.AddLease(lease); err != nil {
		t.Fatal(err)
	}
	err := server.ensureGPUCanReserveWindow(context.Background(), 0, now.Add(time.Hour), now.Add(3*time.Hour))
	if err == nil || !strings.Contains(err.Error(), lease.ID) {
		t.Fatalf("overlapping scheduled reservation error = %v", err)
	}
}

func TestNodeHTTPAllowCreatesAuthorization(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := server.Store.RegisterSoftToken(key, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(protocol.AllowArgs{
		ID:   token.ID,
		Mode: model.ModeUser,
		User: "alice*",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/allow", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.nodeHTTPHandler().ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("allow status = %d, body=%s", response.Code, response.Body.String())
	}
	status, err := server.Store.KeyStatus(key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Authorizations) != 1 || status.Authorizations[0].Username != "alice*" {
		t.Fatalf("authorizations = %+v, want alice* user authorization", status.Authorizations)
	}
}

func TestClaimedMonitorRejectsRunGPUWhenBusy(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	secret, token, err := server.Store.RegisterSoftToken(key, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, tokenHash, err := server.Store.ValidateToken(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	auth := model.Authorization{
		ID:        "auth_run",
		Mode:      model.ModeBare,
		TokenHash: tokenHash,
		TokenMode: token.Mode,
		Holder:    token.Name,
		UID:       1000,
		GID:       1000,
		RootPID:   99,
		CgroupRel: "gpuardian/auth_run",
		CreatedAt: time.Now().UTC(),
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	if err := server.Store.AddAuthorization(auth); err != nil {
		t.Fatal(err)
	}
	killer := &daemonFakeKiller{}
	server.Killer = killer
	server.GPU = fakeAMD{processes: []model.GPUProcess{
		{GPU: 0, PID: 100, MemBytes: 1},
		{GPU: 0, PID: 200, MemBytes: 1},
	}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{
		99:  {PID: 99, UID: 1000, Cgroup: "0::/gpuardian/auth_run"},
		100: {PID: 100, UID: 1000, Cgroup: "0::/gpuardian/auth_run"},
		200: {PID: 200, UID: 2000, Cgroup: "0::/user.slice"},
	}}

	server.monitorOnce(context.Background())

	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.SoftClaims) != 0 {
		t.Fatalf("busy GPU should not be claimed, got %+v", status.SoftClaims)
	}
	if len(killer.killed) != 1 || killer.killed[0] != 100 {
		t.Fatalf("expected gpuardian pid to be killed, got %v", killer.killed)
	}
}

func TestMonitorEvictsRevokedReservationBeforePruningEvidence(t *testing.T) {
	server := testServer(t)
	rootKey, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	secret, token, _, err := server.Store.RegisterHardReservations(rootKey, "alice", []int{0}, "1h", now)
	if err != nil {
		t.Fatal(err)
	}
	authorization := model.Authorization{
		ID:        "auth_revoked",
		Mode:      model.ModeUser,
		TokenHash: token.Hash,
		TokenMode: token.Mode,
		Holder:    token.Name,
		UID:       1000,
		CreatedAt: now.UTC(),
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	if err := server.Store.AddAuthorization(authorization); err != nil {
		t.Fatal(err)
	}
	if err := server.Store.Revoke(secret); err != nil {
		t.Fatal(err)
	}

	killer := &daemonFakeKiller{}
	server.Killer = killer
	server.GPU = fakeAMD{processes: []model.GPUProcess{{GPU: 0, PID: 123, MemBytes: 1}}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{123: {PID: 123, UID: 1000}}}
	server.monitorOnce(context.Background())

	if len(killer.killed) != 1 || killer.killed[0] != 123 {
		t.Fatalf("revoked process kills = %v, want [123]", killer.killed)
	}
	state, err := server.Store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tokens) == 0 || len(state.Reservations) == 0 || len(state.Authorizations) == 0 {
		t.Fatalf("revoked evidence was pruned before quiescence was confirmed: %+v", state)
	}
	server.GPU = fakeAMD{}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{}}
	for i := 1; i <= evictionCleanSamples; i++ {
		server.monitorOnce(context.Background())
		state, err = server.Store.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if i < evictionCleanSamples && len(state.Authorizations) == 0 {
			t.Fatalf("revoked evidence pruned after only %d clean samples", i)
		}
	}
	if len(state.Tokens) != 0 || len(state.Reservations) != 0 || len(state.Authorizations) != 0 {
		t.Fatalf("revoked evidence was not pruned after confirmed quiescence: %+v", state)
	}
}

func TestRevokeSerializesWithEnforcement(t *testing.T) {
	server := testServer(t)
	server.enforceMu.Lock()
	done := make(chan error, 1)
	go func() { done <- server.revoke("missing") }()
	select {
	case <-done:
		server.enforceMu.Unlock()
		t.Fatal("revoke did not wait for the enforcement transaction")
	case <-time.After(25 * time.Millisecond):
	}
	server.enforceMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revoke remained blocked after enforcement completed")
	}
}

func TestConnectionAdmissionReservesRootCapacity(t *testing.T) {
	server := testServer(t)
	for i := 0; i < maxRPCConnections-reservedRootRPCConnections; i++ {
		uid := 1000 + i/maxRPCConnectionsUID
		if !server.acquireConnection(uid) {
			t.Fatalf("non-root connection %d rejected before reserved boundary", i)
		}
	}
	if server.acquireConnection(2000) {
		t.Fatal("non-root connection consumed root-reserved capacity")
	}
	for i := 0; i < reservedRootRPCConnections; i++ {
		if !server.acquireConnection(0) {
			t.Fatalf("root-reserved connection %d was rejected", i)
		}
	}
	if server.acquireConnection(0) {
		t.Fatal("connection admission exceeded the absolute cap")
	}
}

func TestDockerResolveAdmissionIsBounded(t *testing.T) {
	server := testServer(t)
	releases := make([]func(), 0, maxConcurrentDockerResolves)
	for range maxConcurrentDockerResolves {
		release, err := server.acquireDockerResolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := server.acquireDockerResolve(context.Background()); err == nil {
		t.Fatal("docker resolver admission exceeded its cap")
	}
	for _, release := range releases {
		release()
	}
}

func TestNodeHTTPAdmissionIsBounded(t *testing.T) {
	server := testServer(t)
	releases := make([]func(), 0, maxConcurrentNodeHTTP)
	for range maxConcurrentNodeHTTP {
		release, ok := server.acquireNodeHTTPRequest()
		if !ok {
			t.Fatal("node HTTP admission rejected before its cap")
		}
		releases = append(releases, release)
	}
	if _, ok := server.acquireNodeHTTPRequest(); ok {
		t.Fatal("node HTTP admission exceeded its cap")
	}
	for _, release := range releases {
		release()
	}
}

func TestBypassAddSerializesWithEnforcement(t *testing.T) {
	server := testServer(t)
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{123: {PID: 123, StartTime: 42}}}
	server.enforceMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := server.addBypass(protocol.BypassAddArgs{Type: model.BypassPID, PID: 123, TTL: "1h", Reason: "maintenance"}, time.Now())
		done <- err
	}()
	select {
	case <-done:
		server.enforceMu.Unlock()
		t.Fatal("bypass add did not wait for the enforcement transaction")
	case <-time.After(25 * time.Millisecond):
	}
	server.enforceMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("bypass add remained blocked after enforcement completed")
	}
}

func TestCommandBypassRequiresRootUID(t *testing.T) {
	server := testServer(t)
	_, err := server.addBypass(protocol.BypassAddArgs{
		Type: model.BypassCommand, Command: "/usr/bin/gpuagent", UID: 1000, TTL: "1h", Reason: "maintenance",
	}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "uid 0") {
		t.Fatalf("non-root command bypass error = %v, want uid 0 restriction", err)
	}
}

func TestAdminMethodsRequireRootKey(t *testing.T) {
	server := testServer(t)
	if _, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "show_keys", Args: []byte(`{"root_key":"bad"}`)}); !errors.Is(err, store.ErrInvalidRootKey) {
		t.Fatalf("show_keys err=%v, want invalid root key", err)
	}
	revokeArgs, _ := json.Marshal(protocol.RevokeArgs{RootKey: "bad", ID: "missing"})
	if _, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "revoke", Args: revokeArgs}); !errors.Is(err, store.ErrInvalidRootKey) {
		t.Fatalf("revoke err=%v, want invalid root key", err)
	}
	bypassArgs, _ := json.Marshal(protocol.BypassAddArgs{RootKey: "bad", Type: model.BypassPID, PID: 1, TTL: "1h"})
	if _, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "bypass_add", Args: bypassArgs}); !errors.Is(err, store.ErrInvalidRootKey) {
		t.Fatalf("bypass err=%v, want invalid root key", err)
	}
}

func TestRevokeDeletesFromKeyStatus(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := server.Store.RegisterSoftToken(key, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.RevokeArgs{RootKey: key, ID: secret})
	if _, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "revoke", Args: args}); err != nil {
		t.Fatal(err)
	}
	status, err := server.Store.KeyStatus(key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Tokens) != 0 {
		t.Fatalf("revoked token should be deleted from key status: %+v", status.Tokens)
	}
}

func TestEnsureGPUCanReserveRejectsBusyGPU(t *testing.T) {
	server := testServer(t)
	server.GPU = fakeAMD{processes: []model.GPUProcess{{GPU: 0, PID: 10, MemBytes: 1}}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000, Cmdline: []string{"python"}}}}
	if err := server.ensureGPUCanReserve(context.Background(), 0); err == nil {
		t.Fatal("expected busy gpu error")
	}
}

func TestPSIncludesHardReservationRow(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := server.Store.RegisterHardReservations(key, "alice", []int{3}, "1h", time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := server.ps(context.Background(), time.Now(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GPU != "3" || !strings.Contains(rows[0].Command, "reserved until") {
		t.Fatalf("unexpected ps rows: %+v", rows)
	}
}

func TestPSAggregatesMultiGPUProcessRows(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	secret, token, err := server.Store.RegisterSoftToken(key, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	_, tokenHash, err := server.Store.ValidateToken(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	authorization := model.Authorization{
		ID:        "auth_multi",
		Mode:      model.ModeUser,
		TokenHash: tokenHash,
		TokenMode: token.Mode,
		Holder:    token.Name,
		Username:  "alice",
		CreatedAt: now,
		Active:    true,
	}
	if err := server.Store.AddAuthorization(authorization); err != nil {
		t.Fatal(err)
	}
	server.GPU = fakeAMD{processes: []model.GPUProcess{
		{GPU: 0, PID: 123, MemBytes: 1},
		{GPU: 1, PID: 123, MemBytes: 1},
	}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{
		123: {PID: 123, Username: "alice", Cmdline: []string{"python", "train.py"}},
	}}

	rows, err := server.ps(context.Background(), now, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "auth_multi/123" || rows[0].GPU != "0,1" || rows[0].Command != "python train.py" {
		t.Fatalf("expected one aggregated ps row, got %+v", rows)
	}
}

func TestDockerAllowAliasCreatesAuthorization(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := server.Store.RegisterSoftToken(key, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.DockerAllowArgs{Container: "trainer"})
	result, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "allow_docker", Token: secret, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	allow := result.(model.AllowResult)
	if allow.AuthorizationID == "" || allow.Mode != model.ModeDocker {
		t.Fatalf("unexpected allow result: %+v", allow)
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Authorizations) != 1 || status.Authorizations[0].Mode != model.ModeDocker {
		t.Fatalf("expected docker authorization, got %+v", status.Authorizations)
	}
}

func TestDockerAllowWildcardStoresPattern(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := server.Store.RegisterSoftToken(key, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.DockerAllowArgs{Container: "codex*"})
	result, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "allow_docker", Token: secret, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	allow := result.(model.AllowResult)
	if allow.AuthorizationID == "" || allow.ContainerPattern != "codex*" || allow.ContainerID != "" {
		t.Fatalf("unexpected allow result: %+v", allow)
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Authorizations) != 1 || status.Authorizations[0].ContainerPattern != "codex*" {
		t.Fatalf("expected wildcard docker authorization, got %+v", status.Authorizations)
	}
}

func TestDockerAllowRejectsContainerWhenResolutionFails(t *testing.T) {
	server := testServer(t)
	server.Runtime = daemonMissingDockerRuntime{}
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := server.Store.RegisterSoftToken(key, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.DockerAllowArgs{Container: "future-container"})
	if _, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "allow_docker", Token: secret, Args: args}); err == nil {
		t.Fatal("unresolved exact container created a deferred authorization")
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Authorizations) != 0 {
		t.Fatalf("failed container resolution persisted authorization: %+v", status.Authorizations)
	}
}

func TestUserAllowWildcardDoesNotLookupUser(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := server.Store.RegisterSoftToken(key, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.UserAllowArgs{User: "codex*"})
	result, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "allow_user", Token: secret, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	allow := result.(model.AllowResult)
	if allow.AuthorizationID == "" || allow.Username != "codex*" {
		t.Fatalf("unexpected allow result: %+v", allow)
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Authorizations) != 1 || status.Authorizations[0].Username != "codex*" || status.Authorizations[0].UID != -1 {
		t.Fatalf("expected wildcard user authorization, got %+v", status.Authorizations)
	}
}

func TestCommandEnvDoesNotInjectGPUVisibility(t *testing.T) {
	env := commandEnv([]string{"PATH=/bin", "KEY=rg_secret", "ROOT_KEY=rk_secret", "GPUARDIAN_WEB_PASSWORD=secret"})
	for _, item := range env {
		if strings.HasPrefix(item, "HIP_VISIBLE_DEVICES=") ||
			strings.HasPrefix(item, "ROCR_VISIBLE_DEVICES=") ||
			strings.HasPrefix(item, "GPU_DEVICE_ORDINAL=") ||
			item == "KEY=rg_secret" {
			t.Fatalf("unexpected env item %q", item)
		}
	}
	if len(env) != 1 || env[0] != "PATH=/bin" {
		t.Fatalf("unexpected env: %v", env)
	}
	if env := commandEnv(nil); env == nil || len(env) != 0 {
		t.Fatalf("missing client env must not inherit daemon env: %#v", env)
	}
}

func TestPrepareUnixSocketRefusesLiveListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := prepareUnixSocket(path); err == nil {
		t.Fatal("live unix socket was replaced")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("live listener was damaged: %v", err)
	}
	_ = conn.Close()
}

func TestPrepareUnixSocketRemovesOnlyStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepareUnixSocket(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket remains: %v", err)
	}
}

func TestRemoveOwnedUnixSocketPreservesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.sock")
	first, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	first.SetUnlinkOnClose(false)
	defer first.Close()
	expected, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := removeOwnedUnixSocket(path, expected); err == nil {
		t.Fatal("replacement socket was treated as owned")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("replacement socket was removed: %v", err)
	}
}

func TestAcquireUnixSocketLockRejectsSecondOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	first, err := acquireUnixSocketLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := acquireUnixSocketLock(path); err == nil {
		second.Close()
		t.Fatal("second socket lock owner unexpectedly succeeded")
	}
}

func TestRunCommandAllowsNoGPU(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	secret, token, err := server.Store.RegisterSoftToken(key, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, tokenHash, err := server.Store.ValidateToken(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	client, srv := net.Pipe()
	defer client.Close()
	defer srv.Close()
	result, err := server.runCommand(context.Background(), srv, "1", secret, token, tokenHash, peer{UID: os.Getuid(), GID: os.Getgid()}, protocol.RunArgs{
		Command: []string{"/bin/true"},
		Workdir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.AuthorizationID == "" {
		t.Fatalf("unexpected run result: %+v", result)
	}
}

func TestRunConnectionCloseKillsProcessGroup(t *testing.T) {
	server := testServer(t)
	key, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := server.Store.RegisterSoftToken(key, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	script := "trap 'kill \"$child\" 2>/dev/null; wait \"$child\"; exit 0' TERM; sleep 30 & child=$!; echo $child > " + strconv.Quote(childFile) + "; wait $child"

	client, srv := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handleConn(context.Background(), srv)
	}()
	args, _ := json.Marshal(protocol.RunArgs{Command: []string{"/bin/sh", "-c", script}, Workdir: dir})
	req, _ := json.Marshal(protocol.Request{ID: "run1", Method: "run", Token: secret, Args: args})
	if _, err := client.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}
	childPID := waitForPIDFile(t, childFile)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if !waitForPIDExit(childPID, 5*time.Second) {
		t.Fatalf("child pid %d still alive after client close", childPID)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server connection handler did not exit")
	}
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Authorizations) != 0 {
		t.Fatalf("authorization should be released after client close: %+v", status.Authorizations)
	}
}

func TestNormalizePeerGroupsIncludesSupplementaryGroups(t *testing.T) {
	groups := normalizePeerGroups(1001, []string{"1001", "44", "109", "1001", "invalid"})
	want := []uint32{1001, 44, 109}
	if len(groups) != len(want) {
		t.Fatalf("groups=%v want=%v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Fatalf("groups=%v want=%v", groups, want)
		}
	}
}

func TestPeerProcessGroupsUsesConnectedProcessStatus(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "123")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "status"), []byte("Uid:\t1000\t1000\t1000\t1000\nGroups:\t44 109\n"), 0600); err != nil {
		t.Fatal(err)
	}
	groups, err := peerProcessGroups(root, 123, 1000, 1001)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{1001, 44, 109}
	if len(groups) != len(want) {
		t.Fatalf("groups=%v want=%v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Fatalf("groups=%v want=%v", groups, want)
		}
	}
	if _, err := peerProcessGroups(root, 123, 2000, 1001); err == nil {
		t.Fatal("peer UID mismatch was accepted")
	}
}

func TestReadBootIDValidatesKernelValue(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sys", "kernel", "random")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	bootIDPath := filepath.Join(path, "boot_id")
	const bootID = "11111111-2222-3333-4444-555555555555"
	if err := os.WriteFile(bootIDPath, []byte(bootID+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := readBootID(root); err != nil || got != bootID {
		t.Fatalf("readBootID = %q, %v", got, err)
	}
	if err := os.WriteFile(bootIDPath, []byte("not-a-boot-id\n"), 0444); err != nil {
		t.Fatal(err)
	}
	if _, err := readBootID(root); err == nil {
		t.Fatal("invalid boot id was accepted")
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}

func waitForPIDExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !pidAlive(pid)
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err != syscall.ESRCH
}

func TestCleanupFinishedBareLeaseReleasesDeadProcess(t *testing.T) {
	server := testServer(t)
	cgroupPath := filepath.Join(server.Cfg.CgroupRoot, "lease_done")
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	lease := model.Lease{
		ID:         "lease_done",
		GPU:        2,
		Mode:       model.ModeBare,
		Holder:     "alice",
		RootPID:    12345,
		CgroupPath: cgroupPath,
		CreatedAt:  time.Now().Add(-time.Minute),
		ExpiresAt:  time.Now().Add(time.Hour),
		Active:     true,
	}
	if err := server.Store.AddLease(lease); err != nil {
		t.Fatal(err)
	}
	server.cleanupFinishedBareLeases()
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Leases) != 0 {
		t.Fatalf("expected finished lease to be released, got %+v", status.Leases)
	}
}

func TestMonitorCleansExpiredManagedCgroupWhenAMDTelemetryFails(t *testing.T) {
	server := testServer(t)
	server.GPU = failingAMD{err: errors.New("telemetry unavailable")}
	rootKey, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	historicalNow := time.Now().Add(-2 * time.Hour)
	if _, _, _, err := server.Store.RegisterScheduledReservations(rootKey, "old", "", []int{1}, historicalNow, historicalNow.Add(time.Hour), historicalNow); err != nil {
		t.Fatal(err)
	}
	cgroupPath := filepath.Join(server.Cfg.CgroupRoot, "auth_expired")
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	authorization := model.Authorization{
		ID:         "auth_expired",
		Mode:       model.ModeBare,
		CgroupPath: cgroupPath,
		CreatedAt:  time.Now().Add(-2 * time.Hour),
		ExpiresAt:  time.Now().Add(-time.Hour),
		Active:     true,
	}
	if err := server.Store.AddAuthorization(authorization); err != nil {
		t.Fatal(err)
	}

	server.monitorOnce(context.Background())

	if _, err := os.Stat(cgroupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired managed cgroup still exists after telemetry failure: %v", err)
	}
	state, err := server.Store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Authorizations) != 0 {
		t.Fatalf("expired managed authorization was not narrowly pruned: %+v", state.Authorizations)
	}
	if len(state.Tokens) != 0 || len(state.Reservations) != 0 {
		t.Fatalf("unreferenced expired entitlements accumulated during telemetry outage: tokens=%+v reservations=%+v", state.Tokens, state.Reservations)
	}
}

func TestDryRunKeepsNonemptyExpiredManagedAuthorization(t *testing.T) {
	server := testServer(t)
	server.Cfg.DryRun = true
	cgroupPath := filepath.Join(server.Cfg.CgroupRoot, "auth_running")
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"), []byte("123\n"), 0644); err != nil {
		t.Fatal(err)
	}
	authorization := model.Authorization{
		ID:         "auth_running",
		Mode:       model.ModeBare,
		CgroupPath: cgroupPath,
		CreatedAt:  time.Now().Add(-2 * time.Hour),
		ExpiresAt:  time.Now().Add(-time.Hour),
		Active:     true,
	}
	if err := server.Store.AddAuthorization(authorization); err != nil {
		t.Fatal(err)
	}
	server.cleanupExpiredAuthorizations()
	state, err := server.Store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Authorizations) != 1 || !state.Authorizations[0].Active {
		t.Fatalf("dry-run discarded live cgroup evidence: %+v", state.Authorizations)
	}
}

func TestCleanupFinishedBareLeaseKeepsNonEmptyCgroup(t *testing.T) {
	server := testServer(t)
	cgroupPath := filepath.Join(server.Cfg.CgroupRoot, "lease_child")
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"), []byte("99999\n"), 0644); err != nil {
		t.Fatal(err)
	}
	lease := model.Lease{
		ID:         "lease_child",
		GPU:        2,
		Mode:       model.ModeBare,
		Holder:     "alice",
		RootPID:    12345,
		CgroupPath: cgroupPath,
		CreatedAt:  time.Now().Add(-time.Minute),
		ExpiresAt:  time.Now().Add(time.Hour),
		Active:     true,
	}
	if err := server.Store.AddLease(lease); err != nil {
		t.Fatal(err)
	}
	server.cleanupFinishedBareLeases()
	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Leases) != 1 {
		t.Fatalf("expected non-empty cgroup lease to stay active, got %+v", status.Leases)
	}
}

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		SocketPath:  filepath.Join(dir, "gpuardian.sock"),
		StatePath:   filepath.Join(dir, "state.json"),
		RootKeyPath: filepath.Join(dir, "root.key"),
		AuditLog:    filepath.Join(dir, "audit.log"),
		CgroupRoot:  filepath.Join(dir, "cgroup"),
		ProcRoot:    filepath.Join(dir, "proc"),
	}
	st := store.New(cfg)
	if err := st.Load(); err != nil {
		t.Fatal(err)
	}
	return &Server{
		Cfg:                       cfg,
		Store:                     st,
		GPU:                       fakeAMD{},
		Proc:                      daemonFakeProc{infos: map[int]model.ProcInfo{}},
		Runtime:                   daemonFakeRuntime{},
		Interval:                  time.Hour,
		bootID:                    "11111111-2222-3333-4444-555555555555",
		allowUnsafeCgroupFallback: true,
		resolvePeer: func(net.Conn, string) (peer, error) {
			return peer{PID: os.Getpid(), UID: os.Getuid(), GID: os.Getgid(), Groups: []uint32{uint32(os.Getgid())}}, nil
		},
	}
}
