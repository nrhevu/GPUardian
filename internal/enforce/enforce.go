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
	if waitForProcessExit(info.PID, grace) {
		return nil
	}
	if err := syscall.Kill(info.PID, syscall.SIGKILL); err != nil && !isNoSuchProcess(err) {
		return err
	}
	_ = waitForProcessExit(info.PID, 500*time.Millisecond)
	return nil
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		sleep := 100 * time.Millisecond
		if remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !isNoSuchProcess(err)
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
	Process   model.GPUProcess
	Info      model.ProcInfo
	Action    string
	Reason    string
	LeaseID   string
	AuthID    string
	ClaimID   string
	Holder    string
	TokenHash string
	Claim     model.SoftClaim
}

type processView struct {
	Process  model.GPUProcess
	Info     model.ProcInfo
	Bypassed bool
}

func (a Authorizer) Enforce(ctx context.Context, state model.State, processes []model.GPUProcess) ([]Decision, error) {
	now := a.now()
	var decisions []Decision
	byGPU := map[int][]processView{}
	for _, gpuProcess := range processes {
		if !usesGPUResources(gpuProcess) {
			decisions = append(decisions, Decision{Process: gpuProcess, Action: "skip", Reason: "no gpu resource usage"})
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
		view := processView{Process: gpuProcess, Info: info}
		if BypassMatch(state.Bypasses, info, now) {
			view.Bypassed = true
			decisions = append(decisions, Decision{Process: gpuProcess, Info: info, Action: "allow", Reason: "bypass"})
		}
		byGPU[gpuProcess.GPU] = append(byGPU[gpuProcess.GPU], view)
	}

	for gpu, views := range byGPU {
		reservations := activeReservationsForGPU(state.Reservations, gpu, now)
		if len(reservations) > 0 {
			if err := a.enforceHard(ctx, state, gpu, reservations, views, &decisions, now); err != nil {
				return decisions, err
			}
			continue
		}
		leases := activeLeasesForGPU(state.Leases, gpu, now)
		if len(leases) > 0 {
			if err := a.enforceLegacyLeases(ctx, state, gpu, leases, views, &decisions, now); err != nil {
				return decisions, err
			}
			continue
		}
		if err := a.enforceSoft(ctx, state, gpu, views, &decisions, now); err != nil {
			return decisions, err
		}
	}

	for _, claim := range state.SoftClaims {
		if !a.claimHasMatchingProcess(ctx, state, claim, byGPU[claim.GPU], now) {
			decisions = append(decisions, Decision{Action: "release_claim", ClaimID: claim.ID, Reason: "claimed GPU has no matching process"})
			a.audit(model.AuditEvent{
				Time:    now.UTC(),
				Kind:    "claim_released",
				Message: "claimed GPU released after authorized process disappeared",
				GPU:     claim.GPU,
				User:    claim.Holder,
			})
		}
	}

	return decisions, nil
}

func (a Authorizer) BusyProcessesForGPU(ctx context.Context, state model.State, processes []model.GPUProcess, gpu int) ([]Decision, error) {
	now := a.now()
	var busy []Decision
	for _, gpuProcess := range processes {
		if gpuProcess.GPU != gpu {
			continue
		}
		if !usesGPUResources(gpuProcess) {
			continue
		}
		if a.Proc == nil || !a.Proc.Exists(gpuProcess.PID) {
			continue
		}
		info, err := a.Proc.Info(gpuProcess.PID)
		if err != nil {
			continue
		}
		if BypassMatch(state.Bypasses, info, now) {
			continue
		}
		busy = append(busy, Decision{Process: gpuProcess, Info: info, Action: "busy", Reason: "gpu already has non-bypassed process"})
	}
	return busy, nil
}

func (a Authorizer) BusyProcessesForLease(ctx context.Context, state model.State, processes []model.GPUProcess, tentative *model.Lease) ([]Decision, error) {
	if tentative == nil {
		return nil, nil
	}
	now := a.now()
	var busy []Decision
	for _, gpuProcess := range processes {
		if gpuProcess.GPU != tentative.GPU {
			continue
		}
		if !usesGPUResources(gpuProcess) {
			continue
		}
		if a.Proc == nil || !a.Proc.Exists(gpuProcess.PID) {
			continue
		}
		info, err := a.Proc.Info(gpuProcess.PID)
		if err != nil {
			continue
		}
		if BypassMatch(state.Bypasses, info, now) {
			continue
		}
		if tentative.Mode != "" && a.leaseMatches(ctx, *tentative, info, now) {
			continue
		}
		busy = append(busy, Decision{Process: gpuProcess, Info: info, Action: "busy", Reason: "gpu already has non-bypassed process"})
	}
	return busy, nil
}

func (a Authorizer) enforceHard(ctx context.Context, state model.State, gpu int, reservations []model.Reservation, views []processView, decisions *[]Decision, now time.Time) error {
	tokenFilter := map[string]bool{}
	for _, reservation := range reservations {
		tokenFilter[reservation.TokenHash] = true
	}
	holder := reservationHolder(reservations)
	reservationID := firstReservationID(reservations)
	for _, view := range views {
		if view.Bypassed {
			continue
		}
		auth, ok := a.matchAnyAuthorization(ctx, state, gpu, view.Info, tokenFilter, "", now)
		if ok {
			*decisions = append(*decisions, Decision{
				Process:   view.Process,
				Info:      view.Info,
				Action:    "allow",
				Reason:    "authorization",
				AuthID:    auth.ID,
				Holder:    auth.Holder,
				TokenHash: auth.TokenHash,
			})
			continue
		}
		reason := fmt.Sprintf("unauthorized GPU access on gpu=%d pid=%d; gpu is reserved by %s", gpu, view.Process.PID, holder)
		if err := a.kill(decisions, view, reason, reservationID, "", holder, tokenHashForReservations(reservations), now); err != nil {
			return err
		}
	}
	return nil
}

func usesGPUResources(process model.GPUProcess) bool {
	return process.MemBytes > 0
}

func (a Authorizer) enforceLegacyLeases(ctx context.Context, state model.State, gpu int, leases []model.Lease, views []processView, decisions *[]Decision, now time.Time) error {
	holder := leaseHolder(leases)
	leaseID := firstLeaseID(leases)
	for _, view := range views {
		if view.Bypassed {
			continue
		}
		if matchedLeaseID, ok := a.matchesAnyLease(ctx, leases, view.Info, now); ok {
			*decisions = append(*decisions, Decision{
				Process: view.Process,
				Info:    view.Info,
				Action:  "allow",
				Reason:  "lease",
				LeaseID: matchedLeaseID,
				Holder:  holder,
			})
			continue
		}
		reason := fmt.Sprintf("unauthorized GPU access on gpu=%d pid=%d; gpu is held by %s", gpu, view.Process.PID, holder)
		if err := a.kill(decisions, view, reason, leaseID, "", holder, "", now); err != nil {
			return err
		}
	}
	return nil
}

func (a Authorizer) enforceSoft(ctx context.Context, state model.State, gpu int, views []processView, decisions *[]Decision, now time.Time) error {
	claims := activeSoftClaimsForGPU(state, gpu, now)
	if len(claims) > 0 && a.claimHasMatchingProcess(ctx, state, claims[0], views, now) {
		claim := claims[0]
		tokenFilter := map[string]bool{claim.TokenHash: true}
		for _, view := range views {
			if view.Bypassed {
				continue
			}
			auth, ok := a.matchAnyAuthorization(ctx, state, gpu, view.Info, tokenFilter, model.TokenModeClaimed, now)
			if ok {
				*decisions = append(*decisions, Decision{
					Process:   view.Process,
					Info:      view.Info,
					Action:    "allow",
					Reason:    "claimed",
					AuthID:    auth.ID,
					ClaimID:   claim.ID,
					Holder:    auth.Holder,
					TokenHash: auth.TokenHash,
				})
				continue
			}
			reason := fmt.Sprintf("unauthorized GPU access on gpu=%d pid=%d; gpu is claimed by %s", gpu, view.Process.PID, claim.Holder)
			if err := a.kill(decisions, view, reason, "", claim.AuthorizationID, claim.Holder, claim.TokenHash, now); err != nil {
				return err
			}
		}
		return nil
	}

	type authorizedView struct {
		view processView
		auth model.Authorization
	}
	var authorized []authorizedView
	var unauthorized []processView
	for _, view := range views {
		if view.Bypassed {
			continue
		}
		auth, ok := a.matchAnyAuthorization(ctx, state, gpu, view.Info, nil, model.TokenModeClaimed, now)
		if ok {
			authorized = append(authorized, authorizedView{view: view, auth: auth})
		} else {
			unauthorized = append(unauthorized, view)
		}
	}
	if len(authorized) == 0 {
		for _, view := range unauthorized {
			*decisions = append(*decisions, Decision{Process: view.Process, Info: view.Info, Action: "skip", Reason: "gpu has no active claimed authorization"})
		}
		return nil
	}
	if len(unauthorized) > 0 {
		msg := fmt.Sprintf("claimed-mode authorization rejected on gpu=%d because non-authorized GPU processes are already present", gpu)
		a.audit(model.AuditEvent{Time: now.UTC(), Kind: "claim_rejected", Message: msg, GPU: gpu, User: authorized[0].auth.Holder})
		for _, view := range unauthorized {
			*decisions = append(*decisions, Decision{Process: view.Process, Info: view.Info, Action: "skip", Reason: "gpu already has non-authorized process"})
		}
		for _, item := range authorized {
			reason := fmt.Sprintf("claimed GPU access rejected on gpu=%d pid=%d; gpu already has non-authorized process", gpu, item.view.Process.PID)
			if err := a.kill(decisions, item.view, reason, "", item.auth.ID, "existing GPU process", item.auth.TokenHash, now); err != nil {
				return err
			}
		}
		return nil
	}

	claimAuth := authorized[0].auth
	claim := model.SoftClaim{
		GPU:             gpu,
		TokenHash:       claimAuth.TokenHash,
		AuthorizationID: claimAuth.ID,
		Holder:          claimAuth.Holder,
		CreatedAt:       now.UTC(),
		UpdatedAt:       now.UTC(),
	}
	*decisions = append(*decisions, Decision{Action: "claim", Reason: "claimed", AuthID: claimAuth.ID, Holder: claimAuth.Holder, TokenHash: claimAuth.TokenHash, Claim: claim})
	a.audit(model.AuditEvent{Time: now.UTC(), Kind: "claim", Message: "GPU claimed", GPU: gpu, User: claimAuth.Holder})
	for _, item := range authorized {
		if item.auth.TokenHash == claimAuth.TokenHash {
			*decisions = append(*decisions, Decision{
				Process:   item.view.Process,
				Info:      item.view.Info,
				Action:    "allow",
				Reason:    "claimed",
				AuthID:    item.auth.ID,
				Holder:    item.auth.Holder,
				TokenHash: item.auth.TokenHash,
			})
			continue
		}
		reason := fmt.Sprintf("unauthorized GPU access on gpu=%d pid=%d; gpu is claimed by %s", gpu, item.view.Process.PID, claimAuth.Holder)
		if err := a.kill(decisions, item.view, reason, "", claimAuth.ID, claimAuth.Holder, claimAuth.TokenHash, now); err != nil {
			return err
		}
	}
	for _, view := range unauthorized {
		reason := fmt.Sprintf("unauthorized GPU access on gpu=%d pid=%d; gpu is claimed by %s", gpu, view.Process.PID, claimAuth.Holder)
		if err := a.kill(decisions, view, reason, "", claimAuth.ID, claimAuth.Holder, claimAuth.TokenHash, now); err != nil {
			return err
		}
	}
	return nil
}

func (a Authorizer) kill(decisions *[]Decision, view processView, reason, leaseID, authID, holder, tokenHash string, now time.Time) error {
	decision := Decision{
		Process:   view.Process,
		Info:      view.Info,
		Action:    "kill",
		Reason:    reason,
		LeaseID:   leaseID,
		AuthID:    authID,
		Holder:    holder,
		TokenHash: tokenHash,
	}
	*decisions = append(*decisions, decision)
	a.audit(model.AuditEvent{
		Time:    now.UTC(),
		Kind:    "kill",
		Message: reason,
		GPU:     view.Process.GPU,
		PID:     view.Process.PID,
		LeaseID: leaseID,
		User:    holder,
	})
	if a.DryRun || a.Killer == nil {
		return nil
	}
	msg := fmt.Sprintf("rocguard killed pid=%d on gpu=%d: %s", view.Process.PID, view.Process.GPU, reason)
	return a.Killer.Kill(view.Info, msg)
}

func (a Authorizer) claimHasMatchingProcess(ctx context.Context, state model.State, claim model.SoftClaim, views []processView, now time.Time) bool {
	if claim.TokenHash == "" {
		return false
	}
	for _, view := range views {
		if view.Bypassed {
			continue
		}
		if _, ok := a.matchAnyAuthorization(ctx, state, claim.GPU, view.Info, map[string]bool{claim.TokenHash: true}, model.TokenModeClaimed, now); ok {
			return true
		}
	}
	return false
}

func (a Authorizer) matchAnyAuthorization(ctx context.Context, state model.State, gpu int, info model.ProcInfo, tokenFilter map[string]bool, tokenMode string, now time.Time) (model.Authorization, bool) {
	for _, authorization := range state.Authorizations {
		if !authorization.Active || authorization.Revoked || authorizationExpired(authorization, now) {
			continue
		}
		if tokenFilter != nil && !tokenFilter[authorization.TokenHash] {
			continue
		}
		token, ok := activeTokenByHash(state.Tokens, authorization.TokenHash, now)
		if !ok {
			continue
		}
		mode := normalizeTokenMode(token.Mode)
		if tokenMode != "" && mode != tokenMode {
			continue
		}
		authorization.TokenMode = mode
		if a.authorizationMatches(ctx, authorization, gpu, info, now) {
			return authorization, true
		}
	}
	return model.Authorization{}, false
}

func (a Authorizer) authorizationMatches(ctx context.Context, authorization model.Authorization, gpu int, info model.ProcInfo, now time.Time) bool {
	if !authorization.Active || authorization.Revoked || authorizationExpired(authorization, now) {
		return false
	}
	switch authorization.Mode {
	case model.ModeBare:
		return authorization.RootPID == info.PID ||
			(authorization.CgroupRel != "" && strings.Contains(info.Cgroup, authorization.CgroupRel)) ||
			(authorization.CgroupPath != "" && strings.Contains(info.Cgroup, strings.TrimPrefix(authorization.CgroupPath, "/sys/fs/cgroup/")))
	case model.ModeDocker:
		if authorization.ContainerPattern != "" {
			if info.ContainerID == "" || a.Runtime == nil {
				return false
			}
			name, err := a.Runtime.DockerContainerName(ctx, info.ContainerID)
			return err == nil && wildcardMatch(authorization.ContainerPattern, name)
		}
		return sameContainer(info.ContainerID, authorization.ContainerID)
	case model.ModeK8s:
		if info.ContainerID == "" || a.Runtime == nil {
			return false
		}
		ns, err := a.Runtime.NamespaceForContainer(ctx, info.ContainerID)
		return err == nil && wildcardMatch(authorization.Namespace, ns)
	case model.ModeUser:
		if authorization.UID >= 0 {
			return info.UID == authorization.UID
		}
		return authorization.Username != "" && wildcardMatch(authorization.Username, info.Username)
	default:
		return false
	}
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

func activeReservationsForGPU(reservations []model.Reservation, gpu int, now time.Time) []model.Reservation {
	var out []model.Reservation
	for _, reservation := range reservations {
		if reservation.GPU == gpu && model.ReservationActiveAt(reservation, now) {
			out = append(out, reservation)
		}
	}
	return out
}

func activeSoftClaimsForGPU(state model.State, gpu int, now time.Time) []model.SoftClaim {
	var out []model.SoftClaim
	for _, claim := range state.SoftClaims {
		if claim.GPU != gpu {
			continue
		}
		if token, ok := activeTokenByHash(state.Tokens, claim.TokenHash, now); ok && normalizeTokenMode(token.Mode) == model.TokenModeClaimed {
			out = append(out, claim)
		}
	}
	return out
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

func activeTokenByHash(tokens []model.Token, hash string, now time.Time) (model.Token, bool) {
	for _, token := range tokens {
		if token.Hash != hash || token.Revoked {
			continue
		}
		if timeIsSet(token.ExpiresAt) && !now.Before(token.ExpiresAt) {
			continue
		}
		token.Mode = normalizeTokenMode(token.Mode)
		return token, true
	}
	return model.Token{}, false
}

func authorizationExpired(authorization model.Authorization, now time.Time) bool {
	return timeIsSet(authorization.ExpiresAt) && !now.Before(authorization.ExpiresAt)
}

func reservationHolder(reservations []model.Reservation) string {
	var parts []string
	for _, reservation := range reservations {
		holder := strings.TrimSpace(reservation.Holder)
		if holder == "" {
			holder = "unknown"
		}
		if reservation.ID != "" {
			holder = fmt.Sprintf("%s (reservation=%s)", holder, reservation.ID)
		}
		parts = append(parts, holder)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ", ")
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

func firstReservationID(reservations []model.Reservation) string {
	if len(reservations) == 0 {
		return ""
	}
	return reservations[0].ID
}

func firstLeaseID(leases []model.Lease) string {
	if len(leases) == 0 {
		return ""
	}
	return leases[0].ID
}

func tokenHashForReservations(reservations []model.Reservation) string {
	if len(reservations) == 0 {
		return ""
	}
	return reservations[0].TokenHash
}

func BypassMatch(rules []model.BypassRule, info model.ProcInfo, now time.Time) bool {
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

func wildcardMatch(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	value = strings.TrimSpace(value)
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	if parts[0] != "" {
		if !strings.HasPrefix(value, parts[0]) {
			return false
		}
		pos = len(parts[0])
	}
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			continue
		}
		index := strings.Index(value[pos:], part)
		if index < 0 {
			return false
		}
		pos += index + len(part)
	}
	last := parts[len(parts)-1]
	return last == "" || strings.HasSuffix(value, last)
}

func normalizeTokenMode(mode string) string {
	mode = strings.TrimSpace(strings.ToLower(mode))
	switch mode {
	case "":
		return model.TokenModeClaimed
	default:
		return mode
	}
}

func timeIsSet(value time.Time) bool {
	return !value.IsZero()
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
