package enforce

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"gpuardian/internal/model"
	"gpuardian/internal/proc"
)

// Terminations remembers processes that received SIGTERM from a staged kill so
// later passes can confirm their exit or escalate to SIGKILL once the grace
// period has passed. Entries are keyed by PID and start time, so a reused PID
// is never mistaken for the signalled process. It is safe for concurrent use
// and must outlive individual enforcement passes.
type Terminations struct {
	mu      sync.Mutex
	pending map[terminationKey]*termination
}

type terminationKey struct {
	pid       int
	startTime uint64
}

type termination struct {
	key       terminationKey
	request   pendingKill
	deadline  time.Time
	escalated bool
}

func NewTerminations() *Terminations {
	return &Terminations{pending: make(map[terminationKey]*termination)}
}

// Len reports how many signalled processes still await exit confirmation or
// escalation.
func (t *Terminations) Len() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

func (t *Terminations) awaiting(pid int, startTime uint64) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.pending[terminationKey{pid: pid, startTime: startTime}]
	return ok
}

func (t *Terminations) record(request pendingKill, deadline time.Time) {
	key := terminationKey{pid: request.view.Process.PID, startTime: request.view.Info.StartTime}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		t.pending = make(map[terminationKey]*termination)
	}
	t.pending[key] = &termination{key: key, request: request, deadline: deadline}
}

func (t *Terminations) entries() []termination {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]termination, 0, len(t.pending))
	for _, entry := range t.pending {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key.pid != out[j].key.pid {
			return out[i].key.pid < out[j].key.pid
		}
		return out[i].key.startTime < out[j].key.startTime
	})
	return out
}

func (t *Terminations) remove(key terminationKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pending, key)
}

func (t *Terminations) markEscalated(key terminationKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry := t.pending[key]; entry != nil {
		entry.escalated = true
	}
}

func (a Authorizer) stagedKiller() (StagedKiller, bool) {
	if a.DryRun || a.Terminations == nil || a.Killer == nil {
		return nil, false
	}
	staged, ok := a.Killer.(StagedKiller)
	return staged, ok
}

// EscalateTerminations advances processes that received SIGTERM on an earlier
// pass. Call it once per pass, after stale-scope eviction and enforcement,
// with all of that pass's decisions:
//   - a process the pass allowed (for example after a new bypass or
//     authorization) is spared;
//   - a process that exited, or whose PID now belongs to another process, is
//     confirmed killed;
//   - a process still alive after the grace period receives SIGKILL once its
//     identity (start time) is revalidated. GPU membership is deliberately not
//     rechecked, exactly like the blocking killer: a workload cannot dodge
//     SIGKILL by briefly releasing GPU memory after SIGTERM;
//   - a process that survived SIGKILL on an earlier pass is reported as a kill
//     failure and forgotten, so a later pass restarts the sequence if it still
//     uses a GPU without authorization.
//
// decisions may be nil when no GPU sample is available; escalation then
// proceeds on identity and bypass rules alone, as the blocking killer would.
func (a Authorizer) EscalateTerminations(ctx context.Context, state model.State, decisions []Decision) error {
	staged, ok := a.stagedKiller()
	if !ok || a.Terminations.Len() == 0 {
		return nil
	}
	allowed := make(map[terminationKey]bool)
	for _, decision := range decisions {
		if decision.Action == "allow" && decision.Process.PID > 0 {
			allowed[terminationKey{pid: decision.Process.PID, startTime: decision.Info.StartTime}] = true
		}
	}
	now := a.now()
	var escalationErrors []error
	for _, entry := range a.Terminations.entries() {
		if err := ctx.Err(); err != nil {
			escalationErrors = append(escalationErrors, err)
			break
		}
		request := entry.request
		if allowed[entry.key] {
			a.Terminations.remove(entry.key)
			a.auditTermination(request, "kill_cancelled", "SIGKILL cancelled; process is now authorized", now)
			continue
		}
		exited, info, err := a.terminationExited(entry.key)
		if err != nil {
			escalationErrors = append(escalationErrors, fmt.Errorf("inspect signalled pid %d: %w", entry.key.pid, err))
			continue
		}
		if exited {
			a.Terminations.remove(entry.key)
			a.auditTermination(request, "kill", "process exited", now)
			continue
		}
		if BypassMatch(state.Bypasses, info, now, a.BootID) {
			a.Terminations.remove(entry.key)
			a.auditTermination(request, "kill_cancelled", "SIGKILL cancelled; process is now bypassed", now)
			continue
		}
		if entry.escalated {
			a.Terminations.remove(entry.key)
			err := fmt.Errorf("pid %d did not exit after SIGKILL", entry.key.pid)
			a.auditTermination(request, "kill_failed", err.Error(), now)
			escalationErrors = append(escalationErrors, err)
			continue
		}
		if now.Before(entry.deadline) {
			continue
		}
		msg := fmt.Sprintf("gpuardian killed pid=%d on gpu=%d: %s", request.view.Process.PID, request.view.Process.GPU, request.reason)
		if err := staged.ForceKill(request.view.Info, msg); err != nil {
			if exited, _, inspectErr := a.terminationExited(entry.key); inspectErr == nil && exited {
				a.Terminations.remove(entry.key)
				a.auditTermination(request, "kill", "process exited", now)
				continue
			}
			// Keep the entry; the next pass retries after revalidating again.
			a.auditTermination(request, "kill_failed", err.Error(), now)
			escalationErrors = append(escalationErrors, err)
			continue
		}
		a.Terminations.markEscalated(entry.key)
		a.auditTermination(request, "kill_escalated", "SIGKILL sent after grace period", now)
	}
	return errors.Join(escalationErrors...)
}

// terminationExited reports whether the signalled process is gone. It reads
// /proc without any per-pass memo so the answer reflects the current process.
func (a Authorizer) terminationExited(key terminationKey) (bool, model.ProcInfo, error) {
	reader := a.Proc
	if memo, ok := reader.(interface{ Uncached() proc.Reader }); ok {
		reader = memo.Uncached()
	}
	if reader == nil {
		return false, model.ProcInfo{}, errors.New("process identity reader is unavailable")
	}
	info, err := reader.Info(key.pid)
	if err != nil {
		if proc.IsNotExist(err) {
			return true, model.ProcInfo{}, nil
		}
		return false, model.ProcInfo{}, err
	}
	if info.PID != key.pid || info.StartTime != key.startTime {
		return true, info, nil
	}
	return false, info, nil
}

func (a Authorizer) auditTermination(request pendingKill, kind, outcome string, now time.Time) {
	a.audit(model.AuditEvent{
		Time: now.UTC(), Kind: kind, Message: request.reason + ": " + outcome,
		GPU: request.view.Process.GPU, PID: request.view.Process.PID, LeaseID: request.leaseID, User: request.holder,
	})
}
