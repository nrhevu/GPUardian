package enforce

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"rocguardd/internal/model"
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
}

func (f fakeRuntime) ResolveDockerContainer(context.Context, string) (string, error) {
	return "", nil
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

func TestNoLeaseSkipsGPU(t *testing.T) {
	killer := &fakeKiller{}
	auth := Authorizer{
		Proc:   fakeProc{infos: map[int]model.ProcInfo{10: {PID: 10}}},
		Killer: killer,
		Now:    fixedNow,
	}
	decisions, err := auth.Enforce(context.Background(), model.State{}, []model.GPUProcess{{GPU: 0, PID: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "skip" || len(killer.killed) != 0 {
		t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
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
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{{GPU: 0, PID: 10}})
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
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{{GPU: 0, PID: 10}})
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
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{{GPU: 0, PID: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].Action != "allow" || len(killer.killed) != 0 {
		t.Fatalf("unexpected decisions=%+v killed=%v", decisions, killer.killed)
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
	decisions, err := auth.Enforce(context.Background(), state, []model.GPUProcess{{GPU: 0, PID: 10}})
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

func fixedNow() time.Time {
	return time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
}
