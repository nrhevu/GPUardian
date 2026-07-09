package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rocguardd/internal/config"
	"rocguardd/internal/model"
	"rocguardd/internal/protocol"
	"rocguardd/internal/store"
)

type fakeAMD struct {
	processes []model.GPUProcess
}

func (f fakeAMD) Processes(context.Context) ([]model.GPUProcess, error) {
	return f.processes, nil
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
	if register.Token == "" || len(register.ReservationIDs) != 1 || len(register.GPUs) != 1 || register.GPUs[0] != 2 {
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
	if register.Token == "" || len(register.ReservationIDs) != 2 || len(register.GPUs) != 2 || register.GPUs[0] != 0 || register.GPUs[1] != 1 {
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
	server.AMD = fakeAMD{processes: []model.GPUProcess{{GPU: 2, PID: 10, MemBytes: 1}}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000, Cmdline: []string{"python"}}}}
	args, _ := json.Marshal(protocol.RegisterArgs{RootKey: key, Mode: model.TokenModeReserved, Name: "alice", GPUs: []int{1, 2}, TTL: "1h"})
	if _, err := server.dispatch(context.Background(), peer{}, protocol.Request{ID: "1", Method: "register", Args: args}); err == nil {
		t.Fatal("expected busy gpu error")
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
	server.AMD = fakeAMD{processes: []model.GPUProcess{{GPU: 2, PID: 10, MemBytes: 0}}}
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

func TestClaimedMonitorClaimsRunGPUAndKillsUnauthorized(t *testing.T) {
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
		CgroupRel: "rocguard/auth_run",
		CreatedAt: time.Now().UTC(),
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	if err := server.Store.AddAuthorization(auth); err != nil {
		t.Fatal(err)
	}
	killer := &daemonFakeKiller{}
	server.Killer = killer
	server.AMD = fakeAMD{processes: []model.GPUProcess{
		{GPU: 0, PID: 100, MemBytes: 1},
		{GPU: 0, PID: 200, MemBytes: 1},
	}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{
		99:  {PID: 99, UID: 1000, Cgroup: "0::/rocguard/auth_run"},
		100: {PID: 100, UID: 1000, Cgroup: "0::/rocguard/auth_run"},
		200: {PID: 200, UID: 2000, Cgroup: "0::/user.slice"},
	}}

	server.monitorOnce(context.Background())

	status, err := server.Store.Status(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.SoftClaims) != 1 || status.SoftClaims[0].GPU != 0 || status.SoftClaims[0].AuthorizationID != auth.ID {
		t.Fatalf("expected claimed GPU to be stored, got %+v", status.SoftClaims)
	}
	if len(killer.killed) != 1 || killer.killed[0] != 200 {
		t.Fatalf("expected unauthorized pid to be killed, got %v", killer.killed)
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
	server.AMD = fakeAMD{processes: []model.GPUProcess{{GPU: 0, PID: 10, MemBytes: 1}}}
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
	rows, err := server.ps(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GPU != 3 || !strings.Contains(rows[0].Command, "reserved until") {
		t.Fatalf("unexpected ps rows: %+v", rows)
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
	env := commandEnv([]string{"PATH=/bin", "KEY=rg_secret"})
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
	result, err := server.runCommand(context.Background(), srv, "1", token, tokenHash, peer{UID: os.Getuid(), GID: os.Getgid()}, protocol.RunArgs{
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

func TestPeerGroupsIncludesSupplementaryGroups(t *testing.T) {
	dir := t.TempDir()
	statusDir := filepath.Join(dir, "1234")
	if err := os.MkdirAll(statusDir, 0755); err != nil {
		t.Fatal(err)
	}
	status := "Name:\trocguard\nGroups:\t1001 44 109 1001\n"
	if err := os.WriteFile(filepath.Join(statusDir, "status"), []byte(status), 0644); err != nil {
		t.Fatal(err)
	}
	groups := peerGroups(dir, 1234, 1001)
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
		SocketPath:  filepath.Join(dir, "rocguard.sock"),
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
		Cfg:      cfg,
		Store:    st,
		AMD:      fakeAMD{},
		Proc:     daemonFakeProc{infos: map[int]model.ProcInfo{}},
		Runtime:  daemonFakeRuntime{},
		Interval: time.Hour,
	}
}
