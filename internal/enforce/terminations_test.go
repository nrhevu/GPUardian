package enforce

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gpuardian/internal/model"
)

// stagedFakeKiller records staged signals; Kill (the blocking path) must never
// be used when Terminations is set.
type stagedFakeKiller struct {
	mu       sync.Mutex
	signals  []string
	grace    time.Duration
	forceErr error
}

func (k *stagedFakeKiller) record(kind string, pid int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.signals = append(k.signals, fmt.Sprintf("%s:%d", kind, pid))
}

func (k *stagedFakeKiller) Kill(info model.ProcInfo, _ string) error {
	k.record("BLOCKING", info.PID)
	return nil
}

func (k *stagedFakeKiller) Terminate(info model.ProcInfo, _ string) error {
	k.record("TERM", info.PID)
	return nil
}

func (k *stagedFakeKiller) ForceKill(info model.ProcInfo, _ string) error {
	k.record("KILL", info.PID)
	return k.forceErr
}

func (k *stagedFakeKiller) GracePeriod() time.Duration { return k.grace }

func (k *stagedFakeKiller) sent() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return strings.Join(k.signals, ",")
}

// liveProc reports a missing PID with the wrapped ENOENT the /proc reader
// returns for a process that has exited.
type liveProc struct {
	mu    sync.Mutex
	infos map[int]model.ProcInfo
}

func (p *liveProc) Exists(pid int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.infos[pid]
	return ok
}

func (p *liveProc) Info(pid int) (model.ProcInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, ok := p.infos[pid]
	if !ok {
		return model.ProcInfo{}, fmt.Errorf("read process start time: %w", &fs.PathError{Op: "open", Path: "stat", Err: syscall.ENOENT})
	}
	return info, nil
}

func (p *liveProc) set(pid int, info *model.ProcInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if info == nil {
		delete(p.infos, pid)
		return
	}
	p.infos[pid] = *info
}

type stagedHarness struct {
	now        time.Time
	killer     *stagedFakeKiller
	proc       *liveProc
	authorizer Authorizer
	audits     []string
	state      model.State
}

func newStagedHarness(pids ...int) *stagedHarness {
	h := &stagedHarness{
		now:    fixedNow(),
		killer: &stagedFakeKiller{grace: 2 * time.Second},
		proc:   &liveProc{infos: map[int]model.ProcInfo{}},
		state: model.State{
			Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
			Reservations: []model.Reservation{reservation("hash_reserved", 0)},
		},
	}
	for _, pid := range pids {
		h.proc.infos[pid] = model.ProcInfo{PID: pid, StartTime: uint64(pid), UID: 1000}
	}
	h.authorizer = Authorizer{
		Proc:         h.proc,
		Killer:       h.killer,
		Terminations: NewTerminations(),
		Now:          func() time.Time { return h.now },
		OnAudit: func(event model.AuditEvent) {
			h.audits = append(h.audits, fmt.Sprintf("%s:%d", event.Kind, event.PID))
		},
	}
	return h
}

// pass runs one monitor pass: enforcement on the sampled PIDs followed by
// escalation with the pass's decisions.
func (h *stagedHarness) pass(t *testing.T, pids ...int) []Decision {
	t.Helper()
	processes := make([]model.GPUProcess, 0, len(pids))
	for _, pid := range pids {
		processes = append(processes, gpuProcess(0, pid))
	}
	decisions, err := h.authorizer.Enforce(context.Background(), h.state, processes)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.authorizer.EscalateTerminations(context.Background(), h.state, decisions); err != nil {
		t.Fatal(err)
	}
	return decisions
}

func TestStagedKillTerminatesThenEscalatesWithoutBlocking(t *testing.T) {
	h := newStagedHarness(10)
	steps := []struct {
		name        string
		advance     time.Duration
		exited      bool
		wantSignals string
		wantAudits  string
		wantPending int
	}{
		{name: "SIGTERM on detection", wantSignals: "TERM:10", wantAudits: "kill_term:10", wantPending: 1},
		{name: "grace period: no signal, decision still kill", advance: time.Second, wantSignals: "TERM:10", wantAudits: "kill_term:10", wantPending: 1},
		{name: "SIGKILL after grace", advance: time.Second, wantSignals: "TERM:10,KILL:10", wantAudits: "kill_term:10,kill_escalated:10", wantPending: 1},
		{name: "exit confirmed", advance: time.Second, exited: true, wantSignals: "TERM:10,KILL:10", wantAudits: "kill_term:10,kill_escalated:10,kill:10", wantPending: 0},
	}
	for _, step := range steps {
		h.now = h.now.Add(step.advance)
		var sampled []int
		if step.exited {
			h.proc.set(10, nil)
		} else {
			sampled = []int{10}
		}
		decisions := h.pass(t, sampled...)
		if !step.exited && (len(decisions) != 1 || decisions[0].Action != "kill") {
			t.Fatalf("%s: decisions = %+v", step.name, decisions)
		}
		if got := h.killer.sent(); got != step.wantSignals {
			t.Fatalf("%s: signals = %s, want %s", step.name, got, step.wantSignals)
		}
		if got := strings.Join(h.audits, ","); got != step.wantAudits {
			t.Fatalf("%s: audits = %s, want %s", step.name, got, step.wantAudits)
		}
		if got := h.authorizer.Terminations.Len(); got != step.wantPending {
			t.Fatalf("%s: pending = %d, want %d", step.name, got, step.wantPending)
		}
	}
}

func TestEscalateTerminationsOutcomes(t *testing.T) {
	tests := []struct {
		name string
		// prepare runs after SIGTERM and returns the decisions of the pass
		// that calls EscalateTerminations.
		prepare     func(h *stagedHarness) []Decision
		advance     time.Duration
		wantSignals string
		wantAudit   string
		wantPending int
	}{
		{
			name: "allowed by this pass",
			prepare: func(h *stagedHarness) []Decision {
				return []Decision{{Action: "allow", Process: gpuProcess(0, 10), Info: model.ProcInfo{PID: 10, StartTime: 10}}}
			},
			advance:     3 * time.Second,
			wantSignals: "TERM:10",
			wantAudit:   "kill_cancelled:10",
		},
		{
			name: "bypassed after SIGTERM",
			prepare: func(h *stagedHarness) []Decision {
				h.authorizer.BootID = "boot"
				h.state.Bypasses = []model.BypassRule{{Type: model.BypassPID, PID: 10, StartTime: 10, BootID: "boot", ExpiresAt: fixedNow().Add(time.Hour)}}
				return nil
			},
			advance:     3 * time.Second,
			wantSignals: "TERM:10",
			wantAudit:   "kill_cancelled:10",
		},
		{
			name: "pid reused by another process",
			prepare: func(h *stagedHarness) []Decision {
				h.proc.set(10, &model.ProcInfo{PID: 10, StartTime: 99})
				return nil
			},
			advance:     3 * time.Second,
			wantSignals: "TERM:10",
			wantAudit:   "kill:10",
		},
		{
			name:        "released GPU memory but still alive",
			prepare:     func(*stagedHarness) []Decision { return nil },
			advance:     3 * time.Second,
			wantSignals: "TERM:10,KILL:10",
			wantAudit:   "kill_escalated:10",
			wantPending: 1,
		},
		{
			name:        "within grace period",
			prepare:     func(*stagedHarness) []Decision { return nil },
			advance:     time.Second,
			wantSignals: "TERM:10",
			wantPending: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newStagedHarness(10)
			h.pass(t, 10)
			h.audits = nil
			decisions := tc.prepare(h)
			h.now = h.now.Add(tc.advance)
			if err := h.authorizer.EscalateTerminations(context.Background(), h.state, decisions); err != nil {
				t.Fatal(err)
			}
			if got := h.killer.sent(); got != tc.wantSignals {
				t.Fatalf("signals = %s, want %s", got, tc.wantSignals)
			}
			if got := strings.Join(h.audits, ","); got != tc.wantAudit {
				t.Fatalf("audits = %s, want %s", got, tc.wantAudit)
			}
			if got := h.authorizer.Terminations.Len(); got != tc.wantPending {
				t.Fatalf("pending = %d, want %d", got, tc.wantPending)
			}
		})
	}
}

func TestEscalateTerminationsReportsSurvivorOfSIGKILL(t *testing.T) {
	h := newStagedHarness(10)
	h.pass(t, 10)
	h.now = h.now.Add(3 * time.Second)
	if err := h.authorizer.EscalateTerminations(context.Background(), h.state, nil); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Second)
	err := h.authorizer.EscalateTerminations(context.Background(), h.state, nil)
	if err == nil || !strings.Contains(err.Error(), "did not exit after SIGKILL") {
		t.Fatalf("survivor error = %v", err)
	}
	if got := strings.Join(h.audits, ","); got != "kill_term:10,kill_escalated:10,kill_failed:10" {
		t.Fatalf("audits = %s", got)
	}
	if h.authorizer.Terminations.Len() != 0 {
		t.Fatal("survivor stayed pending; a later pass must restart the SIGTERM sequence")
	}
	h.pass(t, 10)
	if got := h.killer.sent(); got != "TERM:10,KILL:10,TERM:10" {
		t.Fatalf("signals after restart = %s", got)
	}
}

func TestEscalateTerminationsKeepsEntryWhenSIGKILLFails(t *testing.T) {
	h := newStagedHarness(10)
	h.killer.forceErr = errors.New("refusing to signal pid 10: revalidate process identity: permission denied")
	h.pass(t, 10)
	h.now = h.now.Add(3 * time.Second)
	if err := h.authorizer.EscalateTerminations(context.Background(), h.state, nil); err == nil {
		t.Fatal("SIGKILL failure was not reported")
	}
	if h.authorizer.Terminations.Len() != 1 {
		t.Fatal("failed escalation was forgotten instead of retried")
	}
	h.killer.forceErr = nil
	if err := h.authorizer.EscalateTerminations(context.Background(), h.state, nil); err != nil {
		t.Fatal(err)
	}
	if got := h.killer.sent(); got != "TERM:10,KILL:10,KILL:10" {
		t.Fatalf("signals = %s", got)
	}
}

func TestStagedKillsAwaitingEscalationDoNotSpendKillBudget(t *testing.T) {
	pids := make([]int, 0, maxKillsPerEnforcementPass+1)
	for pid := 1; pid <= maxKillsPerEnforcementPass+1; pid++ {
		pids = append(pids, pid)
	}
	h := newStagedHarness(pids...)
	h.pass(t, pids...)
	if got := strings.Count(h.killer.sent(), "TERM"); got != maxKillsPerEnforcementPass {
		t.Fatalf("first pass SIGTERMs = %d, want budget %d", got, maxKillsPerEnforcementPass)
	}
	h.now = h.now.Add(time.Second)
	h.pass(t, pids...)
	if got := strings.Count(h.killer.sent(), "TERM"); got != maxKillsPerEnforcementPass+1 {
		t.Fatalf("SIGTERMs after second pass = %d, want the deferred pid signalled", got)
	}
	if strings.Contains(h.killer.sent(), "BLOCKING") {
		t.Fatal("staged authorizer used the blocking killer")
	}
}

func TestStagedKillRequiresTerminationsAndLiveMode(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*Authorizer)
		wantSignals string
	}{
		{name: "without tracker uses blocking kill", mutate: func(a *Authorizer) { a.Terminations = nil }, wantSignals: "BLOCKING:10"},
		{name: "dry run signals nothing", mutate: func(a *Authorizer) { a.DryRun = true }, wantSignals: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newStagedHarness(10)
			tc.mutate(&h.authorizer)
			h.pass(t, 10)
			if got := h.killer.sent(); got != tc.wantSignals {
				t.Fatalf("signals = %s, want %s", got, tc.wantSignals)
			}
		})
	}
}

func TestRealKillerStagedSignalsRevalidateIdentity(t *testing.T) {
	tests := []struct {
		name        string
		current     model.ProcInfo
		force       bool
		wantErr     bool
		wantSignals []syscall.Signal
	}{
		{name: "terminate", current: model.ProcInfo{PID: 10, StartTime: 1}, wantSignals: []syscall.Signal{syscall.SIGTERM}},
		{name: "force kill", current: model.ProcInfo{PID: 10, StartTime: 1}, force: true, wantSignals: []syscall.Signal{syscall.SIGKILL}},
		{name: "terminate refuses reused pid", current: model.ProcInfo{PID: 10, StartTime: 2}, wantErr: true},
		{name: "force kill refuses reused pid", current: model.ProcInfo{PID: 10, StartTime: 2}, force: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var signals []syscall.Signal
			killer := RealKiller{
				Proc: fakeProc{infos: map[int]model.ProcInfo{10: tc.current}},
				signal: func(_ int, signal syscall.Signal) error {
					signals = append(signals, signal)
					return nil
				},
				alive: func(int) bool { t.Fatal("staged signal waited for exit"); return false },
			}
			info := model.ProcInfo{PID: 10, StartTime: 1}
			var err error
			if tc.force {
				err = killer.ForceKill(info, "")
			} else {
				err = killer.Terminate(info, "")
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if fmt.Sprint(signals) != fmt.Sprint(tc.wantSignals) {
				t.Fatalf("signals = %v, want %v", signals, tc.wantSignals)
			}
		})
	}
	if got := (RealKiller{}).GracePeriod(); got != 2*time.Second {
		t.Fatalf("default grace = %v", got)
	}
}

func TestPrepareKillValidationSeesEveryPendingKillFirst(t *testing.T) {
	var events []string
	authz := Authorizer{
		Proc: fakeProc{infos: map[int]model.ProcInfo{
			10: {PID: 10, StartTime: 10, UID: 1000},
			11: {PID: 11, StartTime: 11, UID: 1000},
		}},
		Killer: &fakeKiller{},
		Now:    fixedNow,
		PrepareKillValidation: func(_ context.Context, processes []model.GPUProcess) {
			events = append(events, fmt.Sprintf("prepare:%d", len(processes)))
		},
		ValidateKill: func(_ context.Context, process model.GPUProcess) error {
			events = append(events, fmt.Sprintf("validate:%d", process.PID))
			return nil
		},
	}
	state := model.State{
		Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
		Reservations: []model.Reservation{reservation("hash_reserved", 0), reservation("hash_reserved", 1)},
	}
	if _, err := authz.Enforce(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10), gpuProcess(1, 11)}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0] != "prepare:2" {
		t.Fatalf("events = %v, want one prepare with both kills before validation", events)
	}
}
