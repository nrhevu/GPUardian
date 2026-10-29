package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuardian/internal/enforce"
	"gpuardian/internal/gpusmi"
	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
	runtimepkg "gpuardian/internal/runtime"
	"gpuardian/internal/telemetry"
)

// exitingProc reports a missing PID the way the /proc reader does for an
// exited process, and counts Info reads.
type exitingProc struct {
	mu    sync.Mutex
	infos map[int]model.ProcInfo
	reads map[int]int
}

func (p *exitingProc) Exists(pid int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.infos[pid]
	return ok
}

func (p *exitingProc) Info(pid int) (model.ProcInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reads == nil {
		p.reads = make(map[int]int)
	}
	p.reads[pid]++
	info, ok := p.infos[pid]
	if !ok {
		return model.ProcInfo{}, fmt.Errorf("read process start time: %w", os.ErrNotExist)
	}
	return info, nil
}

func (p *exitingProc) remove(pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.infos, pid)
}

func (p *exitingProc) readCount(pid int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads[pid]
}

type stagedDaemonKiller struct {
	mu      sync.Mutex
	signals []string
	grace   time.Duration
}

func (k *stagedDaemonKiller) record(kind string, pid int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.signals = append(k.signals, fmt.Sprintf("%s:%d", kind, pid))
}

func (k *stagedDaemonKiller) Kill(info model.ProcInfo, _ string) error {
	k.record("BLOCKING", info.PID)
	return nil
}

func (k *stagedDaemonKiller) Terminate(info model.ProcInfo, _ string) error {
	k.record("TERM", info.PID)
	return nil
}

func (k *stagedDaemonKiller) ForceKill(info model.ProcInfo, _ string) error {
	k.record("KILL", info.PID)
	return nil
}

func (k *stagedDaemonKiller) GracePeriod() time.Duration { return k.grace }

const testGrace = 50 * time.Millisecond

func (k *stagedDaemonKiller) sent() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return strings.Join(k.signals, ",")
}

var _ enforce.StagedKiller = (*stagedDaemonKiller)(nil)

func reserveGPUs(t *testing.T, server *Server, holder string, gpus ...int) {
	t.Helper()
	rootKey, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, _, _, err := server.Store.RegisterScheduledReservations(rootKey, holder, "", gpus, now, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
}

func auditKinds(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event model.AuditEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func TestMonitorStagesKillAcrossPasses(t *testing.T) {
	server := testServer(t)
	reserveGPUs(t, server, "bob", 0)
	killer := &stagedDaemonKiller{grace: testGrace}
	server.Killer = killer
	procs := &exitingProc{infos: map[int]model.ProcInfo{123: {PID: 123, StartTime: 7, UID: 1000}}}
	server.Proc = procs
	server.GPU = fakeAMD{processes: []model.GPUProcess{{GPU: 0, PID: 123, MemBytes: 1}}}

	server.monitorOnce(context.Background())
	if got := killer.sent(); got != "TERM:123" {
		t.Fatalf("first pass signals = %s, want SIGTERM only", got)
	}
	server.monitorOnce(context.Background())
	if got := killer.sent(); got != "TERM:123" {
		t.Fatalf("pass within the grace period signalled again: %s", got)
	}
	time.Sleep(testGrace)
	server.monitorOnce(context.Background())
	if got := killer.sent(); got != "TERM:123,KILL:123" {
		t.Fatalf("second pass signals = %s, want SIGKILL after the grace period", got)
	}
	procs.remove(123)
	server.GPU = fakeAMD{}
	server.monitorOnce(context.Background())
	if got := killer.sent(); got != "TERM:123,KILL:123" {
		t.Fatalf("signals after exit = %s", got)
	}
	if got := strings.Join(auditKinds(t, server.Cfg.AuditLog), ","); got != "kill_term,kill_escalated,kill" {
		t.Fatalf("audit kinds = %s", got)
	}
	if server.terminationTracker().Len() != 0 {
		t.Fatal("exited process still pending escalation")
	}
}

func TestMonitorEscalatesWhileGPUInventoryFails(t *testing.T) {
	server := testServer(t)
	reserveGPUs(t, server, "bob", 0)
	killer := &stagedDaemonKiller{grace: testGrace}
	server.Killer = killer
	server.Proc = &exitingProc{infos: map[int]model.ProcInfo{123: {PID: 123, StartTime: 7, UID: 1000}}}
	server.GPU = fakeAMD{processes: []model.GPUProcess{{GPU: 0, PID: 123, MemBytes: 1}}}
	server.monitorOnce(context.Background())
	time.Sleep(testGrace)
	server.GPU = failingAMD{err: errors.New("amd-smi timed out")}
	server.monitorOnce(context.Background())
	if got := killer.sent(); got != "TERM:123,KILL:123" {
		t.Fatalf("signals = %s, want SIGKILL despite the inventory outage", got)
	}
}

// subsetAMD implements gpusmi.GPUProcessProvider and records calls.
type subsetAMD struct {
	mu          sync.Mutex
	processes   []model.GPUProcess
	fullCalls   int
	subsetCalls [][]int
}

func (p *subsetAMD) Processes(context.Context) ([]model.GPUProcess, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fullCalls++
	return append([]model.GPUProcess(nil), p.processes...), nil
}

func (p *subsetAMD) ProcessesForGPUs(_ context.Context, gpus []int) ([]model.GPUProcess, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	requested := append([]int(nil), gpus...)
	sort.Ints(requested)
	p.subsetCalls = append(p.subsetCalls, requested)
	var out []model.GPUProcess
	for _, process := range p.processes {
		for _, gpu := range gpus {
			if process.GPU == gpu {
				out = append(out, process)
			}
		}
	}
	return out, nil
}

var _ gpusmi.GPUProcessProvider = (*subsetAMD)(nil)

func TestKillValidationResamplesOnlyTargetedGPUs(t *testing.T) {
	server := testServer(t)
	reserveGPUs(t, server, "bob", 1, 2)
	killer := &daemonFakeKiller{}
	server.Killer = killer
	server.Proc = &exitingProc{infos: map[int]model.ProcInfo{
		123: {PID: 123, StartTime: 7, UID: 1000},
		124: {PID: 124, StartTime: 8, UID: 1000},
		125: {PID: 125, StartTime: 9, UID: 1000},
	}}
	provider := &subsetAMD{processes: []model.GPUProcess{
		{GPU: 1, PID: 123, MemBytes: 1},
		{GPU: 2, PID: 124, MemBytes: 1},
		{GPU: 3, PID: 125, MemBytes: 1},
	}}
	server.GPU = provider
	server.monitorOnce(context.Background())
	sort.Ints(killer.killed)
	if fmt.Sprint(killer.killed) != "[123 124]" {
		t.Fatalf("killed = %v", killer.killed)
	}
	if provider.fullCalls != 1 {
		t.Fatalf("full samples = %d, want only the monitor's own", provider.fullCalls)
	}
	if len(provider.subsetCalls) != 1 || fmt.Sprint(provider.subsetCalls[0]) != "[1 2]" {
		t.Fatalf("validation samples = %v, want one sample of GPUs 1 and 2", provider.subsetCalls)
	}
}

// lockProbeAMD signals when a fresh sample is requested.
type lockProbeAMD struct {
	fakeAMD
	sampled chan time.Time
}

func (p *lockProbeAMD) FreshProcesses(_ context.Context, notBefore time.Time) ([]model.GPUProcess, error) {
	p.sampled <- notBefore
	return p.processes, nil
}

func TestRegisterSamplesGPUsOutsideEnforcementLock(t *testing.T) {
	server := testServer(t)
	provider := &lockProbeAMD{sampled: make(chan time.Time, 1)}
	server.GPU = provider
	rootKey, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.RegisterArgs{RootKey: rootKey, Mode: model.TokenModeReserved, Name: "alice", GPUs: []int{0}, TTL: "1h"})
	requested := time.Now()
	server.enforceMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := server.dispatch(context.Background(), peer{UID: 0}, protocol.Request{Method: "register", Args: args})
		done <- err
	}()
	select {
	case notBefore := <-provider.sampled:
		if notBefore.Before(requested) || notBefore.After(time.Now()) {
			t.Fatalf("sample freshness bound %v is not the request time", notBefore)
		}
	case <-time.After(5 * time.Second):
		server.enforceMu.Unlock()
		t.Fatal("register did not sample GPUs while enforcement held the lock")
	}
	server.enforceMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPSReusesMonitorProcReads(t *testing.T) {
	server := testServer(t)
	procs := &exitingProc{infos: map[int]model.ProcInfo{42: {PID: 42, StartTime: 1, UID: 1000, Cmdline: []string{"python"}}}}
	server.Proc = procs
	server.GPU = fakeAMD{processes: []model.GPUProcess{{GPU: 3, PID: 42, MemBytes: 1}}}
	server.monitorOnce(context.Background())
	if got := procs.readCount(42); got != 1 {
		t.Fatalf("monitor reads = %d, want 1", got)
	}
	for i := 0; i < 3; i++ {
		if _, err := server.ps(context.Background(), time.Now(), "", true); err != nil {
			t.Fatal(err)
		}
		if _, err := server.Snapshot(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if got := procs.readCount(42); got != 1 {
		t.Fatalf("reads after ps/snapshot = %d, want the monitor's single read reused", got)
	}
	server.monitorOnce(context.Background())
	if got := procs.readCount(42); got != 2 {
		t.Fatalf("reads after a new sample = %d, want a fresh read", got)
	}
}

type flakyDeviceAMD struct {
	fakeAMD
	calls int
}

func (p *flakyDeviceAMD) Devices(context.Context) (map[int]gpusmi.DeviceInfo, error) {
	p.calls++
	if p.calls == 1 {
		return nil, errors.New("amd-smi static failed")
	}
	return map[int]gpusmi.DeviceInfo{0: {Vendor: "amd"}}, nil
}

func TestReadCachesRefreshAndRetry(t *testing.T) {
	t.Run("metrics refresh without telemetry", func(t *testing.T) {
		server := testServer(t)
		provider := &countingSnapshotAMD{}
		server.GPU = provider
		for i := 0; i < 2; i++ {
			if _, err := server.metricsForRead(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if provider.metricCalls != 1 {
			t.Fatalf("metric samples = %d, want cached", provider.metricCalls)
		}
		server.metricsReadAt = time.Now().Add(-metricsReadTTL)
		if _, err := server.metricsForRead(context.Background()); err != nil {
			t.Fatal(err)
		}
		if provider.metricCalls != 2 {
			t.Fatalf("metric samples = %d, want refresh after TTL", provider.metricCalls)
		}
	})
	t.Run("device failures are retried, successes kept", func(t *testing.T) {
		server := testServer(t)
		provider := &flakyDeviceAMD{}
		server.GPU = provider
		steps := []struct {
			age       time.Duration
			wantErr   bool
			wantCalls int
		}{
			{wantErr: true, wantCalls: 1},
			{age: time.Second, wantErr: true, wantCalls: 1},
			{age: devicesRetryInterval, wantCalls: 2},
			{age: time.Hour, wantCalls: 2},
		}
		for i, step := range steps {
			if step.age > 0 {
				server.devicesReadAt = time.Now().Add(-step.age)
			}
			devices, err := server.devicesForRead(context.Background())
			if (err != nil) != step.wantErr || (!step.wantErr && len(devices) != 1) {
				t.Fatalf("step %d: devices = %v, err = %v", i, devices, err)
			}
			if provider.calls != step.wantCalls {
				t.Fatalf("step %d: device queries = %d, want %d", i, provider.calls, step.wantCalls)
			}
		}
	})
}

// cachedOnlyRuntime implements runtime.CachedLookup with an empty cache and
// counts real lookups.
type cachedOnlyRuntime struct {
	daemonFakeRuntime
	mu      sync.Mutex
	lookups int
}

func (r *cachedOnlyRuntime) DockerContainerName(context.Context, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups++
	return "trainer", nil
}

func (r *cachedOnlyRuntime) CachedDockerContainerName(string) (string, bool) { return "", false }

func (r *cachedOnlyRuntime) CachedKubernetesWorkload(string) (runtimepkg.KubernetesWorkload, bool) {
	return runtimepkg.KubernetesWorkload{}, false
}

func TestObservedJobDefersUncachedRuntimeLookup(t *testing.T) {
	server := testServer(t)
	dir := t.TempDir()
	box, err := telemetry.Open(filepath.Join(dir, "node.id"), filepath.Join(dir, "outbox"), "boot-test")
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	server.Telemetry = box
	resolver := &cachedOnlyRuntime{}
	server.Runtime = resolver
	state := model.State{
		Tokens:         []model.Token{{ID: "tok_group", Hash: "hash", Mode: model.TokenModeReserved}},
		Authorizations: []model.Authorization{{ID: "auth_docker", TokenHash: "hash", TokenMode: model.TokenModeReserved, Mode: model.ModeDocker, Holder: "alice"}},
	}
	info := model.ProcInfo{PID: 42, StartTime: 99, UID: 1001, Username: "alice", ContainerID: strings.Repeat("a", 64)}
	server.trackObservedTelemetryJobs(state, []enforce.Decision{{Action: "allow", AuthID: "auth_docker", Process: model.GPUProcess{PID: 42, GPU: 0}, Info: info}}, time.Now().UTC())
	if resolver.lookups != 0 {
		t.Fatalf("runtime lookups while tracking = %d, want none under the enforcement lock", resolver.lookups)
	}
	server.completePendingRuntimeContexts(context.Background())
	if resolver.lookups != 1 {
		t.Fatalf("deferred runtime lookups = %d, want 1", resolver.lookups)
	}
	page, err := box.Page("", 10)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var started, updated telemetry.JobEvent
	for _, event := range page.Events {
		kinds = append(kinds, event.Type)
		switch event.Type {
		case telemetry.EventJobStarted:
			_ = json.Unmarshal(event.Payload, &started)
		case telemetry.EventJobUpdated:
			_ = json.Unmarshal(event.Payload, &updated)
		}
	}
	if strings.Join(kinds, ",") != telemetry.EventJobStarted+","+telemetry.EventJobUpdated {
		t.Fatalf("events = %v", kinds)
	}
	if started.RuntimeContext == nil || started.RuntimeContext.DockerContainerName != "" || started.RuntimeContext.Username != "alice" {
		t.Fatalf("started runtime context = %+v", started.RuntimeContext)
	}
	if updated.ExecutionID != started.ExecutionID || updated.RuntimeContext == nil || updated.RuntimeContext.DockerContainerName != "trainer" ||
		updated.RuntimeContext.ContainerID != info.ContainerID {
		t.Fatalf("updated runtime context = %+v", updated.RuntimeContext)
	}
}

func TestRunJobFinishesWhenReleaseFails(t *testing.T) {
	server := testServer(t)
	dir := t.TempDir()
	box, err := telemetry.Open(filepath.Join(dir, "node.id"), filepath.Join(dir, "outbox"), "boot-test")
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	server.Telemetry = box
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
	go drainConn(client)
	// After the authorization is activated, replace state.json with a
	// directory so the release at command exit cannot be saved.
	script := `sleep 0.3; rm -f "$1" && mkdir "$1"`
	_, err = server.runCommand(context.Background(), srv, "1", secret, token, tokenHash, peer{UID: os.Getuid(), GID: os.Getgid()}, protocol.RunArgs{
		Command: []string{"/bin/sh", "-c", script, "sh", server.Cfg.StatePath},
		Workdir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("release unexpectedly succeeded")
	}
	server.telemetryJobsMu.Lock()
	leaked := len(server.runJobs)
	server.telemetryJobsMu.Unlock()
	if leaked != 0 {
		t.Fatalf("run job leaked after failed release: %d entries", leaked)
	}
	page, err := box.Page("", 10)
	if err != nil {
		t.Fatal(err)
	}
	finished := false
	for _, event := range page.Events {
		finished = finished || event.Type == telemetry.EventJobFinished
	}
	if !finished {
		t.Fatal("run job was not finished")
	}
}

func drainConn(conn net.Conn) {
	buf := make([]byte, 4096)
	for {
		if _, err := conn.Read(buf); err != nil {
			return
		}
	}
}
