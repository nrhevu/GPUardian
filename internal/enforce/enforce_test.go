package enforce

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuardian/internal/model"
	"gpuardian/internal/proc"
	"gpuardian/internal/runtime"
)

type fakeProc struct {
	infos map[int]model.ProcInfo
}

type sequenceProc struct {
	infos []model.ProcInfo
	errs  []error
	calls int
}

type countingProc struct {
	infos     map[int]model.ProcInfo
	infoCalls map[int]int
}

func (f *countingProc) Exists(pid int) bool {
	_, ok := f.infos[pid]
	return ok
}

func (f *countingProc) Info(pid int) (model.ProcInfo, error) {
	f.infoCalls[pid]++
	info, ok := f.infos[pid]
	if !ok {
		return model.ProcInfo{}, errors.New("missing")
	}
	return info, nil
}

func (f *sequenceProc) Exists(int) bool {
	return true
}

func (f *sequenceProc) Info(int) (model.ProcInfo, error) {
	index := f.calls
	f.calls++
	if index < len(f.errs) && f.errs[index] != nil {
		return model.ProcInfo{}, f.errs[index]
	}
	if index >= len(f.infos) {
		return model.ProcInfo{}, errors.New("missing sequence info")
	}
	return f.infos[index], nil
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

type notFoundRuntime struct{}

func (notFoundRuntime) ResolveDockerContainer(context.Context, string) (string, error) {
	return "", runtime.ErrNotFound
}

func (notFoundRuntime) DockerContainerName(context.Context, string) (string, error) {
	return "", runtime.ErrNotFound
}

func (notFoundRuntime) NamespaceForContainer(context.Context, string) (string, error) {
	return "", runtime.ErrNotFound
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

type selectiveFailKiller struct {
	killed []int
	fail   int
}

func (f *selectiveFailKiller) Kill(info model.ProcInfo, _ string) error {
	f.killed = append(f.killed, info.PID)
	if info.PID == f.fail {
		return errors.New("injected kill failure")
	}
	return nil
}

func (f *fakeKiller) Kill(info model.ProcInfo, message string) error {
	f.killed = append(f.killed, info.PID)
	f.messages = append(f.messages, message)
	return nil
}

func gpuProcess(gpu, pid int) model.GPUProcess {
	return model.GPUProcess{GPU: gpu, PID: pid, MemBytes: 1}
}

func TestRealKillerRefusesChangedOrUnverifiableIdentityBeforeTerm(t *testing.T) {
	tests := []struct {
		name   string
		info   model.ProcInfo
		reader proc.Reader
	}{
		{
			name:   "missing original start time",
			info:   model.ProcInfo{PID: 10},
			reader: fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 1}}},
		},
		{
			name:   "changed start time",
			info:   model.ProcInfo{PID: 10, StartTime: 1},
			reader: fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 2}}},
		},
		{
			name: "unreadable current identity",
			info: model.ProcInfo{PID: 10, StartTime: 1},
			reader: &sequenceProc{
				infos: []model.ProcInfo{{PID: 10, StartTime: 1}},
				errs:  []error{errors.New("stat unavailable")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var signals []syscall.Signal
			killer := RealKiller{
				Proc: tt.reader,
				signal: func(_ int, signal syscall.Signal) error {
					signals = append(signals, signal)
					return nil
				},
			}
			if err := killer.Kill(tt.info, ""); err == nil {
				t.Fatal("Kill unexpectedly accepted an unverifiable identity")
			}
			if len(signals) != 0 {
				t.Fatalf("signals = %v, want none", signals)
			}
		})
	}
}

func TestRealKillerRevalidatesIdentityBeforeKill(t *testing.T) {
	info := model.ProcInfo{PID: 10, StartTime: 1}
	reader := &sequenceProc{infos: []model.ProcInfo{
		info,
		{PID: 10, StartTime: 2},
	}}
	var signals []syscall.Signal
	killer := RealKiller{
		Grace: time.Nanosecond,
		Proc:  reader,
		signal: func(_ int, signal syscall.Signal) error {
			signals = append(signals, signal)
			return nil
		},
		alive: func(int) bool { return true },
	}
	if err := killer.Kill(info, ""); err == nil {
		t.Fatal("Kill unexpectedly accepted a changed identity before SIGKILL")
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want SIGTERM only", signals)
	}
	if reader.calls != 2 {
		t.Fatalf("identity reads = %d, want one before each possible signal", reader.calls)
	}
}

func TestRealKillerReportsProcessThatSurvivesKill(t *testing.T) {
	info := model.ProcInfo{PID: 10, StartTime: 1}
	reader := fakeProc{infos: map[int]model.ProcInfo{10: info}}
	var signals []syscall.Signal
	killer := RealKiller{
		Grace: time.Nanosecond,
		Proc:  reader,
		signal: func(_ int, signal syscall.Signal) error {
			signals = append(signals, signal)
			return nil
		},
		alive: func(int) bool { return true },
	}
	if err := killer.Kill(info, ""); err == nil {
		t.Fatal("Kill unexpectedly reported success while the process remained alive")
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want SIGTERM then SIGKILL", signals)
	}
}

func TestEvictExpiredReservedProcessesAtExpiryAcrossScopes(t *testing.T) {
	expiresAt := fixedNow().Add(time.Hour)
	scopes := []struct {
		name          string
		authorization model.Authorization
		matching      model.ProcInfo
		unrelated     model.ProcInfo
		runtime       fakeRuntime
	}{
		{
			name:          "bare",
			authorization: model.Authorization{Mode: model.ModeBare, CgroupRel: "gpuardian/auth_old"},
			matching:      model.ProcInfo{PID: 10, StartTime: 10, Cgroup: "0::/gpuardian/auth_old/child"},
			unrelated:     model.ProcInfo{PID: 11, StartTime: 11, Cgroup: "0::/gpuardian/other"},
		},
		{
			name:          "docker",
			authorization: model.Authorization{Mode: model.ModeDocker, ContainerID: strings.Repeat("a", 64)},
			matching:      model.ProcInfo{PID: 10, StartTime: 10, ContainerID: strings.Repeat("a", 64)},
			unrelated:     model.ProcInfo{PID: 11, StartTime: 11, ContainerID: strings.Repeat("b", 64)},
		},
		{
			name:          "k8s",
			authorization: model.Authorization{Mode: model.ModeK8s, Namespace: "training"},
			matching:      model.ProcInfo{PID: 10, StartTime: 10, ContainerID: "container-a"},
			unrelated:     model.ProcInfo{PID: 11, StartTime: 11, ContainerID: "container-b"},
			runtime: fakeRuntime{namespaces: map[string]string{
				"container-a": "training",
				"container-b": "other",
			}},
		},
		{
			name:          "user",
			authorization: model.Authorization{Mode: model.ModeUser, UID: 1000},
			matching:      model.ProcInfo{PID: 10, StartTime: 10, UID: 1000},
			unrelated:     model.ProcInfo{PID: 11, StartTime: 11, UID: 2000},
		},
	}
	phases := []struct {
		name     string
		now      time.Time
		wantKill bool
	}{
		{name: "before expiry", now: expiresAt.Add(-time.Nanosecond)},
		{name: "at expiry", now: expiresAt, wantKill: true},
	}
	for _, scope := range scopes {
		for _, phase := range phases {
			t.Run(scope.name+"/"+phase.name, func(t *testing.T) {
				authorization := scope.authorization
				authorization.ID = "auth_old"
				authorization.TokenHash = "hash_old"
				authorization.TokenMode = model.TokenModeReserved
				authorization.Holder = "alice"
				authorization.CreatedAt = fixedNow()
				authorization.ExpiresAt = expiresAt
				authorization.Active = true
				token := model.Token{
					ID:        "tok_old",
					Hash:      "hash_old",
					Name:      "alice",
					Mode:      model.TokenModeReserved,
					CreatedAt: fixedNow(),
					ExpiresAt: expiresAt,
				}
				reservation := model.Reservation{
					ID:        "res_old",
					GPU:       0,
					TokenHash: "hash_old",
					Holder:    "alice",
					CreatedAt: fixedNow(),
					StartsAt:  fixedNow(),
					ExpiresAt: expiresAt,
					Active:    true,
				}
				reader := &countingProc{
					infos: map[int]model.ProcInfo{
						10: scope.matching,
						11: scope.unrelated,
					},
					infoCalls: make(map[int]int),
				}
				killer := &fakeKiller{}
				authorizer := Authorizer{
					Proc:    reader,
					Runtime: scope.runtime,
					Killer:  killer,
					Now:     func() time.Time { return phase.now },
				}
				state := model.State{
					Tokens:         []model.Token{token},
					Reservations:   []model.Reservation{reservation},
					Authorizations: []model.Authorization{authorization},
				}
				processes := []model.GPUProcess{
					gpuProcess(0, 10),
					gpuProcess(0, 10),
					gpuProcess(0, 11),
				}
				decisions, _, err := authorizer.EvictExpiredReservedProcesses(context.Background(), state, processes)
				if err != nil {
					t.Fatal(err)
				}
				if !phase.wantKill {
					if len(decisions) != 0 || len(killer.killed) != 0 {
						t.Fatalf("before expiry decisions=%+v killed=%v, want none", decisions, killer.killed)
					}
					return
				}
				if len(decisions) != 1 || decisions[0].Action != "kill" {
					t.Fatalf("decisions=%+v, want one kill", decisions)
				}
				if len(killer.killed) != 1 || killer.killed[0] != 10 {
					t.Fatalf("killed=%v, want matching pid 10 only", killer.killed)
				}
				if reader.infoCalls[10] != 1 || reader.infoCalls[11] != 1 {
					t.Fatalf("info calls=%v, want one per sampled pid despite duplicate rows", reader.infoCalls)
				}
			})
		}
	}
}

func TestEvictExpiredReservedProcessesHandlesRevokedEntitlement(t *testing.T) {
	now := fixedNow().Add(time.Minute)
	tests := []struct {
		name                string
		revokeToken         bool
		revokeReservation   bool
		revokeAuthorization bool
	}{
		{name: "revoked token", revokeToken: true},
		{name: "revoked reservation", revokeReservation: true},
		{name: "revoked authorization", revokeAuthorization: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := token("hash_old", model.TokenModeReserved)
			token.Revoked = tt.revokeToken
			reservation := reservation("hash_old", 0)
			reservation.StartsAt = fixedNow()
			reservation.Revoked = tt.revokeReservation
			authorization := authorization("auth_old", "hash_old", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
				a.UID = 1000
				a.Revoked = tt.revokeAuthorization
			})
			killer := &fakeKiller{}
			authorizer := Authorizer{
				Proc: fakeProc{infos: map[int]model.ProcInfo{
					10: {PID: 10, StartTime: 10, UID: 1000},
				}},
				Killer: killer,
				Now:    func() time.Time { return now },
			}
			state := model.State{
				Tokens:         []model.Token{token},
				Reservations:   []model.Reservation{reservation},
				Authorizations: []model.Authorization{authorization},
			}
			if _, _, err := authorizer.EvictExpiredReservedProcesses(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)}); err != nil {
				t.Fatal(err)
			}
			if len(killer.killed) != 1 || killer.killed[0] != 10 {
				t.Fatalf("killed=%v, want pid 10", killer.killed)
			}
		})
	}
}

func TestEvictExpiredReservedProcessesTreatsZeroMemoryContextAsPresent(t *testing.T) {
	now := fixedNow().Add(time.Minute)
	token := token("hash_old", model.TokenModeReserved)
	token.Revoked = true
	reservation := reservation("hash_old", 0)
	reservation.StartsAt = fixedNow()
	authorization := authorization("auth_old", "hash_old", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
		a.UID = 1000
	})
	killer := &fakeKiller{}
	authorizer := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 10, UID: 1000}}},
		Killer: killer,
		Now:    func() time.Time { return now },
	}
	state := model.State{
		Tokens:         []model.Token{token},
		Reservations:   []model.Reservation{reservation},
		Authorizations: []model.Authorization{authorization},
	}
	processes := []model.GPUProcess{{GPU: 0, PID: 10, MemBytes: 0}}
	decisions, assessment, err := authorizer.EvictExpiredReservedProcesses(context.Background(), state, processes)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != "kill" || len(killer.killed) != 1 || killer.killed[0] != 10 {
		t.Fatalf("decisions=%+v killed=%v, want zero-memory GPU context killed", decisions, killer.killed)
	}
	if clean := assessment.Authorizations[authorization.ID]; clean {
		t.Fatal("zero-memory GPU context was incorrectly assessed as clean")
	}
}

func TestEvictExpiredReservedProcessesKeepsBypassedStaleEvidence(t *testing.T) {
	now := fixedNow().Add(time.Minute)
	token := token("hash_old", model.TokenModeReserved)
	token.Revoked = true
	reservation := reservation("hash_old", 0)
	reservation.StartsAt = fixedNow()
	authorization := authorization("auth_old", "hash_old", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
		a.UID = 1000
	})
	killer := &fakeKiller{}
	authorizer := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 10, UID: 1000}}},
		Killer: killer,
		BootID: "boot-a",
		Now:    func() time.Time { return now },
	}
	state := model.State{
		Tokens:         []model.Token{token},
		Reservations:   []model.Reservation{reservation},
		Authorizations: []model.Authorization{authorization},
		Bypasses: []model.BypassRule{{
			Type:      model.BypassPID,
			PID:       10,
			StartTime: 10,
			BootID:    "boot-a",
			ExpiresAt: now.Add(time.Hour),
		}},
	}
	decisions, assessment, err := authorizer.EvictExpiredReservedProcesses(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 || len(killer.killed) != 0 {
		t.Fatalf("bypassed process decisions=%+v killed=%v, want no kill", decisions, killer.killed)
	}
	if clean := assessment.Authorizations[authorization.ID]; clean {
		t.Fatal("bypassed matching process was incorrectly assessed as clean")
	}
}

func TestEvictExpiredReservedProcessesHandlesRevokedClaimedAuthorization(t *testing.T) {
	now := fixedNow()
	authorization := authorization("auth_claimed", "hash_claimed", model.TokenModeClaimed, model.ModeUser, func(a *model.Authorization) {
		a.UID = 1000
		a.Revoked = true
	})
	killer := &fakeKiller{}
	authorizer := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}},
		Killer: killer,
		Now:    func() time.Time { return now },
	}
	state := model.State{
		Tokens:         []model.Token{{ID: "tok_claimed", Hash: "hash_claimed", Mode: model.TokenModeClaimed, CreatedAt: now, Revoked: true}},
		Authorizations: []model.Authorization{authorization},
	}
	decisions, _, err := authorizer.EvictExpiredReservedProcesses(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != "kill" || len(killer.killed) != 1 || killer.killed[0] != 10 {
		t.Fatalf("decisions=%+v killed=%v, want revoked claimed process killed", decisions, killer.killed)
	}
}

func TestEvictExpiredReservedProcessesPreservesCurrentOverlappingAuthorization(t *testing.T) {
	now := fixedNow().Add(time.Hour)
	oldToken := token("hash_old", model.TokenModeReserved)
	oldToken.ExpiresAt = now
	oldReservation := reservation("hash_old", 0)
	oldReservation.StartsAt = fixedNow()
	oldReservation.ExpiresAt = now
	oldAuthorization := authorization("auth_old", "hash_old", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
		a.UID = 1000
	})
	oldAuthorization.ExpiresAt = now

	currentToken := token("hash_current", model.TokenModeReserved)
	currentToken.ExpiresAt = now.Add(time.Hour)
	currentReservation := reservation("hash_current", 0)
	currentReservation.StartsAt = now.Add(-time.Minute)
	currentReservation.ExpiresAt = now.Add(time.Hour)
	currentAuthorization := authorization("auth_current", "hash_current", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
		a.UID = 1000
	})
	currentAuthorization.ExpiresAt = now.Add(time.Hour)

	killer := &fakeKiller{}
	authorizer := Authorizer{
		Proc: fakeProc{infos: map[int]model.ProcInfo{
			10: {PID: 10, StartTime: 10, UID: 1000},
		}},
		Killer: killer,
		Now:    func() time.Time { return now },
	}
	state := model.State{
		Tokens:         []model.Token{oldToken, currentToken},
		Reservations:   []model.Reservation{oldReservation, currentReservation},
		Authorizations: []model.Authorization{oldAuthorization, currentAuthorization},
	}
	decisions, _, err := authorizer.EvictExpiredReservedProcesses(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 || len(killer.killed) != 0 {
		t.Fatalf("decisions=%+v killed=%v, want current authorization preserved", decisions, killer.killed)
	}
}

func TestEvictionDoesNotApplyReservedScopeToUnreservedGPU(t *testing.T) {
	now := fixedNow()
	reservedToken := token("hash_reserved", model.TokenModeReserved)
	reservedToken.ExpiresAt = now.Add(time.Hour)
	reservedReservation := reservation("hash_reserved", 0)
	reservedReservation.StartsAt = now.Add(-time.Minute)
	reservedReservation.ExpiresAt = now.Add(time.Hour)
	reservedAuthorization := authorization("auth_reserved", "hash_reserved", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
		a.UID = 1000
	})
	reservedAuthorization.ExpiresAt = now.Add(time.Hour)
	killer := &fakeKiller{}
	authorizer := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 10, UID: 1000}}},
		Killer: killer,
		Now:    func() time.Time { return now },
	}
	state := model.State{
		Tokens:         []model.Token{reservedToken},
		Reservations:   []model.Reservation{reservedReservation},
		Authorizations: []model.Authorization{reservedAuthorization},
	}
	decisions, _, err := authorizer.EvictExpiredReservedProcesses(context.Background(), state, []model.GPUProcess{gpuProcess(1, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 || len(killer.killed) != 0 {
		t.Fatalf("unrelated GPU process was targeted: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestEvictionDoesNotTreatFutureReservationAsStale(t *testing.T) {
	now := fixedNow()
	reservedToken := token("hash_reserved", model.TokenModeReserved)
	reservedToken.ExpiresAt = now.Add(2 * time.Hour)
	futureReservation := reservation("hash_reserved", 0)
	futureReservation.StartsAt = now.Add(time.Hour)
	futureReservation.ExpiresAt = now.Add(2 * time.Hour)
	authorization := authorization("auth_reserved", "hash_reserved", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
		a.UID = 1000
	})
	authorization.ExpiresAt = now.Add(2 * time.Hour)
	killer := &fakeKiller{}
	authorizer := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 10, UID: 1000}}},
		Killer: killer,
		Now:    func() time.Time { return now },
	}
	state := model.State{
		Tokens:         []model.Token{reservedToken},
		Reservations:   []model.Reservation{futureReservation},
		Authorizations: []model.Authorization{authorization},
	}
	decisions, assessment, err := authorizer.EvictExpiredReservedProcesses(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 0 || len(killer.killed) != 0 {
		t.Fatalf("future reservation was treated as stale: decisions=%+v killed=%v", decisions, killer.killed)
	}
	if _, tracked := assessment.Authorizations[authorization.ID]; tracked {
		t.Fatalf("future authorization should not be considered ready for stale-scope pruning: %+v", assessment)
	}
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
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 42, UID: 1000}}},
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
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 42, UID: 1000}}},
		Killer: killer,
		BootID: "boot-a",
		Now:    fixedNow,
	}
	state := model.State{
		Leases: []model.Lease{activeLease(model.ModeBare, 0)},
		Bypasses: []model.BypassRule{{
			Type:      model.BypassPID,
			PID:       10,
			StartTime: 42,
			BootID:    "boot-a",
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

func TestPIDBypassDoesNotFollowReusedPID(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 43, UID: 1000}}},
		Killer: killer,
		BootID: "boot-a",
		Now:    fixedNow,
	}
	state := model.State{
		Leases: []model.Lease{activeLease(model.ModeBare, 0)},
		Bypasses: []model.BypassRule{{
			Type:      model.BypassPID,
			PID:       10,
			StartTime: 42,
			BootID:    "boot-a",
			ExpiresAt: fixedNow().Add(time.Hour),
		}},
	}
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != "kill" || len(killer.killed) != 1 {
		t.Fatalf("reused pid bypassed enforcement: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestPIDBypassDoesNotSurviveBoot(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 42, UID: 1000}}},
		Killer: killer,
		BootID: "boot-b",
		Now:    fixedNow,
	}
	state := model.State{
		Leases: []model.Lease{activeLease(model.ModeBare, 0)},
		Bypasses: []model.BypassRule{{
			Type:      model.BypassPID,
			PID:       10,
			StartTime: 42,
			BootID:    "boot-a",
			ExpiresAt: fixedNow().Add(time.Hour),
		}},
	}
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != "kill" || len(killer.killed) != 1 {
		t.Fatalf("previous-boot pid bypassed enforcement: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestCommandBypassRejectsLegacyNonRootRule(t *testing.T) {
	rule := model.BypassRule{
		Type:      model.BypassCommand,
		UID:       1000,
		Command:   "/usr/bin/gpuagent",
		ExpiresAt: fixedNow().Add(time.Hour),
	}
	info := model.ProcInfo{UID: 1000, CommandPath: rule.Command}
	if BypassMatch([]model.BypassRule{rule}, info, fixedNow(), "") {
		t.Fatal("legacy non-root command bypass unexpectedly matched")
	}
	rule.UID = 0
	info.UID = 0
	if !BypassMatch([]model.BypassRule{rule}, info, fixedNow(), "") {
		t.Fatal("root command bypass did not match")
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

func TestContainerPrefixRequiresDockerMinimumIDLength(t *testing.T) {
	full := "aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	if sameContainer(full, "aaaaaaaaaaa") {
		t.Fatal("11-character container prefix unexpectedly matched")
	}
	if !sameContainer(full, "aaaaaaaaaaaa") {
		t.Fatal("12-character container prefix should match its full ID")
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

func TestUnknownMemoryUsageDoesNotBypassEnforcement(t *testing.T) {
	killer := &fakeKiller{}
	authorizer := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 10}}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0)},
	}
	process := model.GPUProcess{GPU: 0, PID: 10, MemBytesUnknown: true}
	if _, err := authorizer.Enforce(context.Background(), state, []model.GPUProcess{process}); err != nil {
		t.Fatal(err)
	}
	if len(killer.killed) != 1 {
		t.Fatalf("unknown memory process bypassed enforcement: %v", killer.killed)
	}
}

func TestRuntimeLookupFailureDefersKill(t *testing.T) {
	containerID := strings.Repeat("a", 64)
	killer := &fakeKiller{}
	authorizer := Authorizer{
		Proc:    fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, ContainerID: containerID}}},
		Runtime: fakeRuntime{},
		Killer:  killer,
		Now:     fixedNow,
	}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0)},
		Authorizations: []model.Authorization{authorization("auth_k8s", "hash_reserved", model.TokenModeReserved, model.ModeK8s, func(a *model.Authorization) {
			a.Namespace = "training"
		})},
	}
	decisions, err := authorizer.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err == nil {
		t.Fatal("runtime lookup failure was not reported")
	}
	if len(killer.killed) != 0 || len(decisions) != 1 || decisions[0].Action != "skip" {
		t.Fatalf("runtime uncertainty killed process: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestEnforcementContinuesAfterIndependentKillFailure(t *testing.T) {
	killer := &selectiveFailKiller{fail: 10}
	authorizer := Authorizer{
		Proc: fakeProc{infos: map[int]model.ProcInfo{
			10: {PID: 10, StartTime: 10},
			11: {PID: 11, StartTime: 11},
		}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0)},
	}
	_, err := authorizer.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10), gpuProcess(0, 11)})
	if err == nil {
		t.Fatal("kill failure was not returned")
	}
	if len(killer.killed) != 2 {
		t.Fatalf("later target was skipped after failure: %v", killer.killed)
	}
}

func TestEnforcementTargetsMultiGPUProcessOnce(t *testing.T) {
	killer := &fakeKiller{}
	authorizer := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 10}}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens: []model.Token{
			token("hash_zero", model.TokenModeReserved),
			token("hash_one", model.TokenModeReserved),
		},
		Reservations: []model.Reservation{
			reservation("hash_zero", 0),
			reservation("hash_one", 1),
		},
	}
	if _, err := authorizer.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10), gpuProcess(1, 10)}); err != nil {
		t.Fatal(err)
	}
	if len(killer.killed) != 1 {
		t.Fatalf("multi-GPU pid targeted %d times: %v", len(killer.killed), killer.killed)
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
				a.CgroupRel = "gpuardian/auth_bare"
			}),
			info: model.ProcInfo{PID: 10, UID: 1000, Cgroup: "0::/gpuardian/auth_bare"},
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
			name: "docker deferred name",
			auth: authorization("auth_docker_deferred", "hash_reserved", model.TokenModeReserved, model.ModeDocker, func(a *model.Authorization) {
				a.ContainerPattern = "future-container"
			}),
			info:    model.ProcInfo{PID: 10, ContainerID: containerID},
			runtime: fakeRuntime{names: map[string]string{containerID: "future-container"}},
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

func TestUnrelatedRuntimeAuthorizationDoesNotDisableEnforcement(t *testing.T) {
	containerID := strings.Repeat("a", 64)
	tests := []struct {
		name string
		auth model.Authorization
	}{
		{
			name: "docker wildcard does not match k8s container",
			auth: authorization("auth_docker", "hash_reserved", model.TokenModeReserved, model.ModeDocker, func(a *model.Authorization) {
				a.ContainerPattern = "trainer*"
			}),
		},
		{
			name: "k8s scope does not match docker container",
			auth: authorization("auth_k8s", "hash_reserved", model.TokenModeReserved, model.ModeK8s, func(a *model.Authorization) {
				a.Namespace = "training"
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			killer := &fakeKiller{}
			authz := Authorizer{
				Proc:    fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 10, ContainerID: containerID}}},
				Runtime: notFoundRuntime{},
				Killer:  killer,
				Now:     fixedNow,
			}
			state := model.State{
				Tokens:         []model.Token{token("hash_reserved", model.TokenModeReserved)},
				Reservations:   []model.Reservation{reservation("hash_reserved", 0)},
				Authorizations: []model.Authorization{test.auth},
			}
			decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
			if err != nil {
				t.Fatal(err)
			}
			if len(killer.killed) != 1 || len(decisions) != 1 || decisions[0].Action != "kill" {
				t.Fatalf("unrelated runtime scope deferred enforcement: decisions=%+v killed=%v", decisions, killer.killed)
			}
		})
	}
}

func TestEnforcementCapsKillsPerPass(t *testing.T) {
	infos := make(map[int]model.ProcInfo)
	processes := make([]model.GPUProcess, 0, maxKillsPerEnforcementPass+2)
	for pid := 1; pid <= maxKillsPerEnforcementPass+2; pid++ {
		infos[pid] = model.ProcInfo{PID: pid, StartTime: uint64(pid), UID: 1000}
		processes = append(processes, gpuProcess(0, pid))
	}
	killer := &fakeKiller{}
	authz := Authorizer{Proc: fakeProc{infos: infos}, Killer: killer, Now: fixedNow}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0)},
	}
	decisions, err := authz.Enforce(context.Background(), state, processes)
	if err != nil {
		t.Fatal(err)
	}
	if len(killer.killed) != maxKillsPerEnforcementPass {
		t.Fatalf("kills in one pass = %d, want cap %d", len(killer.killed), maxKillsPerEnforcementPass)
	}
	deferred := 0
	for _, decision := range decisions {
		if decision.Action == "skip" && strings.Contains(decision.Reason, "kill budget") {
			deferred++
		}
	}
	if deferred != 2 {
		t.Fatalf("deferred decisions = %d, want 2: %+v", deferred, decisions)
	}
}

func TestEnforcementRevalidatesGPUMembershipBeforeKill(t *testing.T) {
	killer := &fakeKiller{}
	authz := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, StartTime: 10, UID: 1000}}},
		Killer: killer,
		ValidateKill: func(context.Context, model.GPUProcess) error {
			return errors.New("no longer on gpu")
		},
		Now: fixedNow,
	}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0)},
	}
	decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err == nil || !strings.Contains(err.Error(), "no longer on gpu") {
		t.Fatalf("GPU revalidation error = %v", err)
	}
	if len(killer.killed) != 0 {
		t.Fatalf("stale GPU pid was killed: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestUserWildcardLookupFailureDefersEnforcement(t *testing.T) {
	killer := &fakeKiller{}
	authz := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}},
		Killer: killer,
		UsernameLookup: func(int) (string, error) {
			return "", errors.New("nss unavailable")
		},
		Now: fixedNow,
	}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0)},
		Authorizations: []model.Authorization{authorization("auth_user_wildcard", "hash_reserved", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
			a.UID = -1
			a.Username = "alice*"
		})},
	}
	decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err == nil || !strings.Contains(err.Error(), "nss unavailable") {
		t.Fatalf("lookup error = %v, want NSS failure", err)
	}
	if len(killer.killed) != 0 {
		t.Fatalf("lookup uncertainty killed pid: decisions=%+v killed=%v", decisions, killer.killed)
	}
	if len(decisions) != 1 || decisions[0].Action != "skip" {
		t.Fatalf("lookup uncertainty decisions=%+v, want one skip", decisions)
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

func TestManagedKeyUsesReservedEntitlementAndClaimsFreeGPU(t *testing.T) {
	for _, test := range []struct {
		name         string
		reservations []model.Reservation
		wantFirst    string
	}{
		{name: "own reservation", reservations: []model.Reservation{reservation("hash_managed", 0)}, wantFirst: "allow"},
		{name: "free gpu", wantFirst: "claim"},
	} {
		t.Run(test.name, func(t *testing.T) {
			killer := &fakeKiller{}
			authz := Authorizer{Proc: fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}}, Killer: killer, Now: fixedNow}
			managed := token("hash_managed", model.TokenModeManaged)
			managed.Managed, managed.Version, managed.ExpiresAt = true, 1, time.Time{}
			auth := authorization("auth_managed", managed.Hash, model.TokenModeManaged, model.ModeUser, func(a *model.Authorization) {
				a.UID = 1000
				a.TokenVersion = 1
				a.ExpiresAt = time.Time{}
			})
			state := model.State{Tokens: []model.Token{managed}, Reservations: test.reservations, Authorizations: []model.Authorization{auth}}
			decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
			if err != nil {
				t.Fatal(err)
			}
			if len(decisions) == 0 || decisions[0].Action != test.wantFirst || len(killer.killed) != 0 {
				t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
			}
		})
	}
}

func TestManagedKeyCannotUseAnotherOwnersReservation(t *testing.T) {
	killer := &fakeKiller{}
	authz := Authorizer{Proc: fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10, UID: 1000}}}, Killer: killer, Now: fixedNow}
	managed := token("hash_managed", model.TokenModeManaged)
	managed.Managed, managed.Version, managed.ExpiresAt = true, 1, time.Time{}
	auth := authorization("auth_managed", managed.Hash, model.TokenModeManaged, model.ModeUser, func(a *model.Authorization) {
		a.UID = 1000
		a.TokenVersion = 1
		a.ExpiresAt = time.Time{}
	})
	other := reservation("hash_other", 0)
	other.Holder = "bob"
	state := model.State{Tokens: []model.Token{managed}, Reservations: []model.Reservation{other}, Authorizations: []model.Authorization{auth}}
	decisions, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != "kill" || len(killer.killed) != 1 {
		t.Fatalf("managed key crossed reservation ownership: decisions=%+v killed=%v", decisions, killer.killed)
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
		t.Fatalf("expected existing process to be skipped and gpuardian process to be killed: %+v", decisions)
	}
	if len(killer.killed) != 1 || killer.killed[0] != 10 {
		t.Fatalf("expected gpuardian pid to be killed: decisions=%+v killed=%v", decisions, killer.killed)
	}
}

func TestSoftClaimRejectsRunCgroupOnBusyGPU(t *testing.T) {
	killer := &fakeKiller{}
	authz := Authorizer{
		Proc: fakeProc{infos: map[int]model.ProcInfo{
			20: {PID: 20, UID: 1000, Cgroup: "0::/gpuardian/auth_run"},
			21: {PID: 21, UID: 2000, Cgroup: "0::/user.slice"},
		}},
		Killer: killer,
		Now:    fixedNow,
	}
	state := model.State{
		Tokens: []model.Token{token("hash_claimed", model.TokenModeClaimed)},
		Authorizations: []model.Authorization{authorization("auth_run", "hash_claimed", model.TokenModeClaimed, model.ModeBare, func(a *model.Authorization) {
			a.RootPID = 19
			a.CgroupRel = "gpuardian/auth_run"
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

func TestBareAuthorizationAndLeaseRequireCgroupSubtreeMatch(t *testing.T) {
	targets := []struct {
		name       string
		cgroupRel  string
		cgroupPath string
	}{
		{name: "relative", cgroupRel: "gpuardian/auth_run"},
		{name: "absolute", cgroupPath: "/sys/fs/cgroup/gpuardian/auth_run"},
	}
	cases := []struct {
		name   string
		cgroup string
		want   bool
	}{
		{name: "exact", cgroup: "0::/gpuardian/auth_run", want: true},
		{name: "descendant", cgroup: "0::/gpuardian/auth_run/child.scope", want: true},
		{name: "matching second hierarchy", cgroup: "7:cpu:/other\n8:memory:/gpuardian/auth_run/child", want: true},
		{name: "prefix collision", cgroup: "0::/gpuardian/auth_runner", want: false},
		{name: "embedded substring", cgroup: "0::/other/gpuardian/auth_run", want: false},
		{name: "parent", cgroup: "0::/gpuardian", want: false},
	}
	authorizer := Authorizer{}
	now := fixedNow()
	for _, target := range targets {
		for _, tc := range cases {
			t.Run(target.name+"/"+tc.name, func(t *testing.T) {
				info := model.ProcInfo{PID: 10, Cgroup: tc.cgroup}
				authorization := model.Authorization{
					Mode:       model.ModeBare,
					RootPID:    info.PID,
					CgroupRel:  target.cgroupRel,
					CgroupPath: target.cgroupPath,
					ExpiresAt:  now.Add(time.Hour),
					Active:     true,
				}
				lease := model.Lease{
					Mode:       model.ModeBare,
					RootPID:    info.PID,
					CgroupRel:  target.cgroupRel,
					CgroupPath: target.cgroupPath,
					ExpiresAt:  now.Add(time.Hour),
					Active:     true,
				}
				if got := authorizer.authorizationMatches(context.Background(), authorization, 0, info, now); got != tc.want {
					t.Fatalf("authorization match = %v, want %v", got, tc.want)
				}
				if got := authorizer.leaseMatches(context.Background(), lease, info, now); got != tc.want {
					t.Fatalf("lease match = %v, want %v", got, tc.want)
				}
			})
		}
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
