package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
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

type daemonFakeRuntime struct{}

func (daemonFakeRuntime) ResolveDockerContainer(context.Context, string) (string, error) {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
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

	args, _ := json.Marshal(protocol.RegisterArgs{RootKey: key, Name: "alice", TTL: "1h"})
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

func TestEnsureLeaseCanStartRejectsBusyBareGPU(t *testing.T) {
	server := testServer(t)
	server.AMD = fakeAMD{processes: []model.GPUProcess{{GPU: 0, PID: 10}}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000, Cmdline: []string{"python"}}}}
	lease := model.Lease{ID: "lease_test", GPU: 0, Mode: model.ModeBare, Active: true, ExpiresAt: time.Now().Add(time.Hour)}
	if err := server.ensureLeaseCanStart(context.Background(), lease); err == nil {
		t.Fatal("expected busy gpu error")
	}
}

func TestEnsureLeaseCanStartAllowsMatchingDockerContainer(t *testing.T) {
	server := testServer(t)
	containerID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	server.AMD = fakeAMD{processes: []model.GPUProcess{{GPU: 0, PID: 10}}}
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000, ContainerID: containerID}}}
	server.Runtime = daemonFakeRuntime{}
	lease := model.Lease{ID: "lease_test", GPU: 0, Mode: model.ModeDocker, ContainerID: containerID, Active: true, ExpiresAt: time.Now().Add(time.Hour)}
	if err := server.ensureLeaseCanStart(context.Background(), lease); err != nil {
		t.Fatalf("expected matching docker container to be allowed: %v", err)
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
