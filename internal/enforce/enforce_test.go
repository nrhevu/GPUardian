package enforce

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"rocguard/internal/model"
)

type fakeProc struct {
	infos map[int]model.ProcInfo
}

func (f fakeProc) Exists(pid int) bool {
	_, ok := f.infos[pid]
	return ok
}

func (f fakeProc) Info(pid int) (model.ProcInfo, error) {
	info, ok := f.infos[pid]
	if !ok {
		return model.ProcInfo{}, errors.New("missing")
	}
	return info, nil
}

type fakeRuntime struct {
	namespaces map[string]string
	names      map[string]string
}

func (f fakeRuntime) ResolveDockerContainer(context.Context, string) (string, error) {
	return "", nil
}

func (f fakeRuntime) DockerContainerName(_ context.Context, id string) (string, error) {
	name, ok := f.names[id]
	if !ok {
		return "", errors.New("missing container name")
	}
	return name, nil
}

func (f fakeRuntime) NamespaceForContainer(_ context.Context, id string) (string, error) {
	ns, ok := f.namespaces[id]
	if !ok {
		return "", errors.New("missing namespace")
	}
	return ns, nil
}

type fakeKiller struct {
	killed   []int
	messages []string
}

func (f *fakeKiller) Kill(info model.ProcInfo, message string) error {
	f.killed = append(f.killed, info.PID)
	f.messages = append(f.messages, message)
	return nil
}

func gpuProcess(gpu, pid int) model.GPUProcess {
	return model.GPUProcess{GPU: gpu, PID: pid, MemBytes: 1}
}

func TestNoLeaseSkipsGPU(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10}}},
		Killer: killer,
		Now:    fixedNow,
	}
	decisions, err := auth.Enforce(context.Background(), model.State{}, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "skip" || len(killer.killed) != 0 {
		t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestNoGPUResourceUsageSkipsProcess(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0)},
	}
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{{GPU: 0, PID: 10, MemBytes: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "skip" || decisions[0].Reason != "no gpu resource usage" || len(killer.killed) != 0 {
		t.Fatalf("parent-only GPU process should be skipped: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestUnauthorizedPIDIsKilledOnLeasedGPU(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}},
		Killer: killer,
		Now:    fixedNow,
	}
	lease := activeLease(model.ModeBare, 0)
	lease.Holder = "alice"
	state := model.State{Leases: []model.Lease{lease}}
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "kill" || len(killer.killed) != 1 || killer.killed[0] != 10 {
		t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
	}
	if decisions[0].LeaseID != "lease_test" || !strings.Contains(decisions[0].Reason, "alice (lease=lease_test)") {
		t.Fatalf("kill reason should include holder and lease: %+v", decisions[0])
	}
	if len(killer.messages) != 1 || !strings.Contains(killer.messages[0], "gpu is held by alice (lease=lease_test)") {
		t.Fatalf("kill message should include holder and lease: %v", killer.messages)
	}
}

func TestBypassAllowsPID(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Leases: []model.Lease{activeLease(model.ModeBare, 0)},
		Bypasses: []model.BypassRule{{
			Type:      model.BypassPID,
			PID:       10,
			ExpiresAt: fixedNow().Add(time.Hour),
		}},
	}
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "allow" || len(killer.killed) != 0 {
		t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestDockerLeaseAllowsMatchingContainer(t *testing.T) {
	killer := &fakeKiller{}
	containerID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, ContainerID: containerID}}},
		Killer: killer,
		Now:    fixedNow,
	}
	lease := activeLease(model.ModeDocker, 0)
	lease.ContainerID = containerID
	state := model.State{Leases: []model.Lease{lease}}
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "allow" || len(killer.killed) != 0 {
		t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestHardReservationKillsUnauthorizedWithoutAuthorizedProcess(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0)},
	}
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "kill" || len(killer.killed) != 1 {
		t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestDryRunDoesNotKill(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}},
		Killer: killer,
		Now:    fixedNow,
		DryRun: true,
	}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0)},
	}
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "kill" || len(killer.killed) != 0 {
		t.Fatalf("dry run should decide kill without invoking killer: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestHardReservationAllowsMatchingAuthorizationScopes(t *testing.T) {
	containerID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := []struct {
		name    string
		auth    model.Authorization
		info    model.ProcInfo
		runtime fakeRuntime
	}{
		{
			name: "bare",
			auth: authorization("auth_bare", "hash_reserved", model.TokenModeReserved, model.ModeBare, func(a *model.Authorization) {
				a.RootPID = 10
			}),
			info: model.ProcInfo{PID: 10, UID: 1000},
		},
		{
			name: "docker",
			auth: authorization("auth_docker", "hash_reserved", model.TokenModeReserved, model.ModeDocker, func(a *model.Authorization) {
				a.ContainerID = containerID
			}),
			info: model.ProcInfo{PID: 10, ContainerID: containerID},
		},
		{
			name: "k8s",
			auth: authorization("auth_k8s", "hash_reserved", model.TokenModeReserved, model.ModeK8s, func(a *model.Authorization) {
				a.Namespace = "training"
			}),
			info:    model.ProcInfo{PID: 10, ContainerID: containerID},
			runtime: fakeRuntime{namespaces: map[string]string{containerID: "training"}},
		},
		{
			name: "user",
			auth: authorization("auth_user", "hash_reserved", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
				a.UID = 1000
			}),
			info: model.ProcInfo{PID: 10, UID: 1000},
		},
		{
			name: "docker wildcard",
			auth: authorization("auth_docker_wildcard", "hash_reserved", model.TokenModeReserved, model.ModeDocker, func(a *model.Authorization) {
				a.ContainerPattern = "codex*"
			}),
			info:    model.ProcInfo{PID: 10, ContainerID: containerID},
			runtime: fakeRuntime{names: map[string]string{containerID: "codex-worker"}},
		},
		{
			name: "k8s wildcard",
			auth: authorization("auth_k8s_wildcard", "hash_reserved", model.TokenModeReserved, model.ModeK8s, func(a *model.Authorization) {
				a.Namespace = "train*"
			}),
			info:    model.ProcInfo{PID: 10, ContainerID: containerID},
			runtime: fakeRuntime{namespaces: map[string]string{containerID: "training"}},
		},
		{
			name: "user wildcard",
			auth: authorization("auth_user_wildcard", "hash_reserved", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
				a.UID = -1
				a.Username = "codex*"
			}),
			info: model.ProcInfo{PID: 10, UID: 1000, Username: "codex-runner"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			killer := &fakeKiller{}
			rt := tt.runtime
			if rt.namespaces == nil {
				rt.namespaces = map[string]string{}
			}
			if rt.names == nil {
				rt.names = map[string]string{}
			}
			authz := Authorizer{
				Proc:    fakeProc{infos: map[int]model.ProcInfo{10: tt.info}},
				Runtime: rt,
				Killer:  killer,
				Now:     fixedNow,
			}
			state := model.State{
				Tokens:         []model.Token{token("hash_reserved", model.TokenModeReserved)},
				Reservations:   []model.Reservation{reservation("hash_reserved", 0)},
				Authorizations: []model.Authorization{tt.auth},
			}
			decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
			if err != nil {
				t.Fatal(err)
			}
			if decisions[0].Action != "allow" || len(killer.killed) != 0 {
				t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
			}
		})
	}
}

func TestSoftClaimCreatedOnCleanAuthorizedGPU(t *testing.T) {
	killer := &fakeKiller{}
	authz := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens:         []model.Token{token("hash_claimed", model.TokenModeClaimed)},
		Authorizations: []model.Authorization{authorization("auth_user", "hash_claimed", model.TokenModeClaimed, model.ModeUser, func(a *model.Authorization) { a.UID = 1000 })},
	}
	decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "claim" || decisions[1].Action != "allow" || len(killer.killed) != 0 {
		t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestSoftClaimRejectsAuthorizedProcessOnBusyGPU(t *testing.T) {
	killer := &fakeKiller{}
	authz := Authorizer{
		Proc: fakeProc{infos: map[int]model.ProcInfo{
			10: {PID: 10, UID: 1000},
			11: {PID: 11, UID: 2000},
		}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens:         []model.Token{token("hash_claimed", model.TokenModeClaimed)},
		Authorizations: []model.Authorization{authorization("auth_user", "hash_claimed", model.TokenModeClaimed, model.ModeUser, func(a *model.Authorization) { a.UID = 1000 })},
	}
	decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10), gpuProcess(0, 11)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].Action != "skip" || decisions[1].Action != "kill" {
		t.Fatalf("expected existing process to be skipped and rocguard process to be killed: %+v", decisions)
	}
	if len(killer.killed) != 1 || killer.killed[0] != 10 {
		t.Fatalf("expected rocguard pid to be killed: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestSoftClaimRejectsRunCgroupOnBusyGPU(t *testing.T) {
	killer := &fakeKiller{}
	authz := Authorizer{
		Proc: fakeProc{infos: map[int]model.ProcInfo{
			20: {PID: 20, UID: 1000, Cgroup: "0::/rocguard/auth_run"},
			21: {PID: 21, UID: 2000, Cgroup: "0::/user.slice"},
		}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens: []model.Token{token("hash_claimed", model.TokenModeClaimed)},
		Authorizations: []model.Authorization{authorization("auth_run", "hash_claimed", model.TokenModeClaimed, model.ModeBare, func(a *model.Authorization) {
			a.RootPID = 19
			a.CgroupRel = "rocguard/auth_run"
		})},
	}
	decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 20), gpuProcess(0, 21)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].Action != "skip" || decisions[1].Action != "kill" {
		t.Fatalf("expected existing process to be skipped and run cgroup pid to be killed: %+v", decisions)
	}
	if len(killer.killed) != 1 || killer.killed[0] != 20 {
		t.Fatalf("expected run cgroup pid to be killed: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestSoftClaimKillsLaterUnauthorizedProcess(t *testing.T) {
	killer := &fakeKiller{}
	authz := Authorizer{
		Proc: fakeProc{infos: map[int]model.ProcInfo{
			10: {PID: 10, UID: 1000},
			11: {PID: 11, UID: 2000},
		}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens:         []model.Token{token("hash_claimed", model.TokenModeClaimed)},
		Authorizations: []model.Authorization{authorization("auth_user", "hash_claimed", model.TokenModeClaimed, model.ModeUser, func(a *model.Authorization) { a.UID = 1000 })},
		SoftClaims:     []model.SoftClaim{{ID: "claim_test", GPU: 0, TokenHash: "hash_claimed", AuthorizationID: "auth_user", Holder: "alice"}},
	}
	decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10), gpuProcess(0, 11)})
	if err != nil {
		t.Fatal(err)
	}
	if len(killer.killed) != 1 || killer.killed[0] != 11 {
		t.Fatalf("expected later unauthorized pid to be killed: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestStalePIDIsIgnored(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{Leases: []model.Lease{activeLease(model.ModeBare, 0)}}
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "skip" || len(killer.killed) != 0 {
		t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func activeLease(mode string, gpu int) model.Lease {
	return model.Lease{
		ID:        "lease_test",
		GPU:       gpu,
		Mode:      mode,
		CreatedAt: fixedNow(),
		ExpiresAt: fixedNow().Add(time.Hour),
		Active:    true,
	}
}

func token(hash, mode string) model.Token {
	return model.Token{
		ID:        "tok_test",
		Hash:      hash,
		Name:      "alice",
		Mode:      mode,
		CreatedAt: fixedNow(),
		ExpiresAt: fixedNow().Add(time.Hour),
	}
}

func reservation(hash string, gpu int) model.Reservation {
	return model.Reservation{
		ID:        "res_test",
		GPU:       gpu,
		TokenHash: hash,
		Holder:    "alice",
		CreatedAt: fixedNow(),
		ExpiresAt: fixedNow().Add(time.Hour),
		Active:    true,
	}
}

func authorization(id, hash, tokenMode, mode string, apply func(*model.Authorization)) model.Authorization {
	out := model.Authorization{
		ID:        id,
		Mode:      mode,
		TokenHash: hash,
		TokenMode: tokenMode,
		Holder:    "alice",
		CreatedAt: fixedNow(),
		ExpiresAt: fixedNow().Add(time.Hour),
		Active:    true,
	}
	if apply != nil {
		apply(&out)
	}
	return out
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
}
