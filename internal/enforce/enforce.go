package enforce

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"rocguardd/internal/model"
	"rocguardd/internal/proc"
	"rocguardd/internal/runtime"
)

type Killer interface {
	Kill(info model.ProcInfo, message string) error
}

type RealKiller struct {
	Grace time.Duration
}

func (k RealKiller) Kill(info model.ProcInfo, message string) error {
	if message != "" {
		_ = proc.WriteMessageToStderr(info, message+"\n")
	}
	if err := syscall.Kill(info.PID, syscall.SIGTERM); err != nil && !isNoSuchProcess(err) {
		return err
	}
	grace := k.Grace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	time.Sleep(grace)
	if err := syscall.Kill(info.PID, 0); err != nil {
		return nil
	}
	if err := syscall.Kill(info.PID, syscall.SIGKILL); err != nil && !isNoSuchProcess(err) {
		return err
	}
	return nil
}

type Authorizer struct {
	Proc    proc.Reader
	Runtime runtime.Resolver
	Killer  Killer
	Now     func() time.Time
	OnAudit func(model.AuditEvent)
	DryRun  bool
}

type Decision struct {
	Process model.GPUProcess
	Info    model.ProcInfo
	Action  string
	Reason  string
	LeaseID string
}

func (a Authorizer) Enforce(ctx context.Context, state model.State, processes []model.GPUProcess) ([]Decision, error) {
	now := a.now()
	var decisions []Decision
	for _, gpuProcess := range processes {
		leases := activeLeasesForGPU(state.Leases, gpuProcess.GPU, now)
		if len(leases) == 0 {
			decisions = append(decisions, Decision{Process: gpuProcess, Action: "skip", Reason: "gpu has no active lease"})
			continue
		}
		if a.Proc == nil || !a.Proc.Exists(gpuProcess.PID) {
			decisions = append(decisions, Decision{Process: gpuProcess, Action: "skip", Reason: "stale pid"})
			continue
		}
		info, err := a.Proc.Info(gpuProcess.PID)
		if err != nil {
			decisions = append(decisions, Decision{Process: gpuProcess, Action: "skip", Reason: "proc info unavailable"})
			continue
		}
		if bypassMatch(state.Bypasses, info, now) {
			decisions = append(decisions, Decision{Process: gpuProcess, Info: info, Action: "allow", Reason: "bypass"})
			continue
		}
		if leaseID, ok := a.matchesAnyLease(ctx, leases, info, now); ok {
			decisions = append(decisions, Decision{Process: gpuProcess, Info: info, Action: "allow", Reason: "lease", LeaseID: leaseID})
			continue
		}
		holder := leaseHolder(leases)
		leaseID := firstLeaseID(leases)
		reason := fmt.Sprintf("unauthorized GPU access on gpu=%d pid=%d; gpu is held by %s", gpuProcess.GPU, gpuProcess.PID, holder)
		decision := Decision{Process: gpuProcess, Info: info, Action: "kill", Reason: reason, LeaseID: leaseID}
		decisions = append(decisions, decision)
		a.audit(model.AuditEvent{
			Time:    now.UTC(),
			Kind:    "kill",
			Message: reason,
			GPU:     gpuProcess.GPU,
			PID:     gpuProcess.PID,
			LeaseID: leaseID,
			User:    holder,
		})
		if !a.DryRun && a.Killer != nil {
			msg := fmt.Sprintf("rocguard killed pid=%d on gpu=%d: unauthorized GPU access; gpu is held by %s; use KEY=... rocguard run --gpu %d -- <command>", gpuProcess.PID, gpuProcess.GPU, holder, gpuProcess.GPU)
			if err := a.Killer.Kill(info, msg); err != nil {
				return decisions, err
			}
		}
	}
	return decisions, nil
}

func (a Authorizer) BusyProcessesForLease(ctx context.Context, state model.State, processes []model.GPUProcess, tentative *model.Lease) ([]Decision, error) {
	now := a.now()
	var busy []Decision
	for _, gpuProcess := range processes {
		if tentative != nil && gpuProcess.GPU != tentative.GPU {
			continue
		}
		if tentative == nil {
			continue
		}
		if a.Proc == nil || !a.Proc.Exists(gpuProcess.PID) {
			continue
		}
		info, err := a.Proc.Info(gpuProcess.PID)
		if err != nil {
			continue
		}
		if bypassMatch(state.Bypasses, info, now) {
			continue
		}
		if tentative.Mode != "" && a.leaseMatches(ctx, *tentative, info, now) {
			continue
		}
		busy = append(busy, Decision{Process: gpuProcess, Info: info, Action: "busy", Reason: "gpu already has non-bypassed process"})
	}
	return busy, nil
}

func (a Authorizer) matchesAnyLease(ctx context.Context, leases []model.Lease, info model.ProcInfo, now time.Time) (string, bool) {
	for _, lease := range leases {
		if a.leaseMatches(ctx, lease, info, now) {
			return lease.ID, true
		}
	}
	return "", false
}

func (a Authorizer) leaseMatches(ctx context.Context, lease model.Lease, info model.ProcInfo, now time.Time) bool {
	if !lease.Active || !now.Before(lease.ExpiresAt) {
		return false
	}
	switch lease.Mode {
	case model.ModeBare:
		return lease.RootPID == info.PID ||
			(lease.CgroupRel != "" && strings.Contains(info.Cgroup, lease.CgroupRel)) ||
			(lease.CgroupPath != "" && strings.Contains(info.Cgroup, strings.TrimPrefix(lease.CgroupPath, "/sys/fs/cgroup/")))
	case model.ModeDocker:
		return sameContainer(info.ContainerID, lease.ContainerID)
	case model.ModeK8s:
		if info.ContainerID == "" || a.Runtime == nil {
			return false
		}
		ns, err := a.Runtime.NamespaceForContainer(ctx, info.ContainerID)
		return err == nil && ns == lease.Namespace
	default:
		return false
	}
}

func activeLeasesForGPU(leases []model.Lease, gpu int, now time.Time) []model.Lease {
	var out []model.Lease
	for _, lease := range leases {
		if lease.GPU == gpu && lease.Active && now.Before(lease.ExpiresAt) {
			out = append(out, lease)
		}
	}
	return out
}

func leaseHolder(leases []model.Lease) string {
	var parts []string
	for _, lease := range leases {
		holder := strings.TrimSpace(lease.Holder)
		if holder == "" {
			holder = "unknown"
		}
		if lease.ID != "" {
			holder = fmt.Sprintf("%s (lease=%s)", holder, lease.ID)
		}
		parts = append(parts, holder)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ", ")
}

func firstLeaseID(leases []model.Lease) string {
	if len(leases) == 0 {
		return ""
	}
	return leases[0].ID
}

func bypassMatch(rules []model.BypassRule, info model.ProcInfo, now time.Time) bool {
	for _, rule := range rules {
		if rule.Revoked || !now.Before(rule.ExpiresAt) {
			continue
		}
		switch rule.Type {
		case model.BypassPID:
			if rule.PID == info.PID {
				return true
			}
		case model.BypassCommand:
			if rule.UID == info.UID && rule.Command != "" && rule.Command == info.CommandPath {
				return true
			}
		}
	}
	return false
}

func sameContainer(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func (a Authorizer) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a Authorizer) audit(event model.AuditEvent) {
	if a.OnAudit != nil {
		a.OnAudit(event)
	}
}

func isNoSuchProcess(err error) bool {
	return err == os.ErrProcessDone || err == syscall.ESRCH
}
