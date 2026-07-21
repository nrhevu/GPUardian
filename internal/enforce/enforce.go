package enforce

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuardian/internal/model"
	"gpuardian/internal/proc"
	"gpuardian/internal/runtime"
)

type Killer interface {
	Kill(info model.ProcInfo, message string) error
}

type RealKiller struct {
	Grace time.Duration
	Proc  proc.Reader

	signal func(int, syscall.Signal) error
	alive  func(int) bool
}

const (
	sysPIDFDSendSignal = 424
	sysPIDFDOpen       = 434
)

func (k RealKiller) Kill(info model.ProcInfo, _ string) error {
	if info.PID <= 0 || info.StartTime == 0 {
		return fmt.Errorf("refusing to signal pid %d: process identity is unverifiable", info.PID)
	}
	pidfd := -1
	if k.signal == nil {
		var err error
		pidfd, err = openPIDFD(info.PID)
		if err != nil {
			return fmt.Errorf("open pidfd for pid %d: %w", info.PID, err)
		}
		defer syscall.Close(pidfd)
	}
	if err := k.validateIdentity(info); err != nil {
		return err
	}
	if err := k.sendSignal(info.PID, pidfd, syscall.SIGTERM); err != nil && !isNoSuchProcess(err) {
		return err
	}
	grace := k.Grace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	if waitForProcessExitWith(info.PID, grace, k.processAlive) {
		return nil
	}
	if err := k.validateIdentity(info); err != nil {
		return err
	}
	if err := k.sendSignal(info.PID, pidfd, syscall.SIGKILL); err != nil && !isNoSuchProcess(err) {
		return err
	}
	if !waitForProcessExitWith(info.PID, 500*time.Millisecond, k.processAlive) {
		return fmt.Errorf("pid %d did not exit after SIGKILL", info.PID)
	}
	return nil
}

func (k RealKiller) validateIdentity(info model.ProcInfo) error {
	if info.PID <= 0 || info.StartTime == 0 {
		return fmt.Errorf("refusing to signal pid %d: process identity is unverifiable", info.PID)
	}
	reader := k.Proc
	if reader == nil {
		reader = proc.NewFSReader("/proc")
	}
	current, err := reader.Info(info.PID)
	if err != nil {
		return fmt.Errorf("refusing to signal pid %d: revalidate process identity: %w", info.PID, err)
	}
	if current.PID != info.PID || current.StartTime == 0 || current.StartTime != info.StartTime {
		return fmt.Errorf("refusing to signal pid %d: process identity changed", info.PID)
	}
	return nil
}

func (k RealKiller) sendSignal(pid, pidfd int, signal syscall.Signal) error {
	if k.signal != nil {
		return k.signal(pid, signal)
	}
	return pidfdSendSignal(pidfd, signal)
}

func openPIDFD(pid int) (int, error) {
	fd, _, errno := syscall.Syscall(sysPIDFDOpen, uintptr(pid), 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

func pidfdSendSignal(pidfd int, signal syscall.Signal) error {
	_, _, errno := syscall.Syscall6(sysPIDFDSendSignal, uintptr(pidfd), uintptr(signal), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func (k RealKiller) processAlive(pid int) bool {
	if k.alive != nil {
		return k.alive(pid)
	}
	return processAlive(pid)
}

func waitForProcessExitWith(pid int, timeout time.Duration, alive func(int) bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !alive(pid) {
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
	Proc           proc.Reader
	Runtime        runtime.Resolver
	Killer         Killer
	BootID         string
	ValidateKill   func(context.Context, model.GPUProcess) error
	UsernameLookup func(int) (string, error)
	Now            func() time.Time
	OnAudit        func(model.AuditEvent)
	DryRun         bool
	pendingKills   *[]pendingKill
	activeTokens   map[string]model.Token
}

type pendingKill struct {
	view      processView
	reason    string
	leaseID   string
	authID    string
	holder    string
	tokenHash string
	now       time.Time
}

const maxKillsPerEnforcementPass = 8

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

type gpuTokenKey struct {
	gpu       int
	tokenHash string
}

// EvictionAssessment records whether each stale non-cgroup scope was
// conclusively quiescent in the current process sample. The daemon requires
// repeated clean samples before discarding the scope used to find stragglers.
type EvictionAssessment struct {
	Authorizations map[string]bool
	Leases         map[string]bool
}

func (a Authorizer) Enforce(ctx context.Context, state model.State, processes []model.GPUProcess) ([]Decision, error) {
	var pending []pendingKill
	planner := a
	planner.activeTokens = make(map[string]model.Token, len(state.Tokens))
	for _, token := range state.Tokens {
		planner.activeTokens[token.Hash] = token
	}
	if !a.DryRun {
		planner.pendingKills = &pending
	}
	decisions, planningErr := planner.enforce(ctx, state, processes)
	return decisions, errors.Join(planningErr, a.executePendingKills(ctx, pending))
}

func (a Authorizer) enforce(ctx context.Context, state model.State, processes []model.GPUProcess) ([]Decision, error) {
	now := a.now()
	var decisions []Decision
	var enforcementErrors []error
	targeted := make(map[int]bool)
	byGPU := map[int][]processView{}
	for _, gpuProcess := range processes {
		if err := ctx.Err(); err != nil {
			return decisions, errors.Join(append(enforcementErrors, err)...)
		}
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
		if BypassMatch(state.Bypasses, info, now, a.BootID) {
			view.Bypassed = true
			decisions = append(decisions, Decision{Process: gpuProcess, Info: info, Action: "allow", Reason: "bypass"})
		}
		byGPU[gpuProcess.GPU] = append(byGPU[gpuProcess.GPU], view)
	}

	for gpu, views := range byGPU {
		if err := ctx.Err(); err != nil {
			return decisions, errors.Join(append(enforcementErrors, err)...)
		}
		reservations := activeReservationsForGPU(state.Reservations, gpu, now)
		if len(reservations) > 0 {
			if err := a.enforceHard(ctx, state, gpu, reservations, views, &decisions, targeted, now); err != nil {
				enforcementErrors = append(enforcementErrors, err)
			}
			continue
		}
		leases := activeLeasesForGPU(state.Leases, gpu, now)
		if len(leases) > 0 {
			if err := a.enforceLegacyLeases(ctx, state, gpu, leases, views, &decisions, targeted, now); err != nil {
				enforcementErrors = append(enforcementErrors, err)
			}
			continue
		}
		if err := a.enforceSoft(ctx, state, gpu, views, &decisions, targeted, now); err != nil {
			enforcementErrors = append(enforcementErrors, err)
		}
	}

	for _, claim := range state.SoftClaims {
		if err := ctx.Err(); err != nil {
			return decisions, errors.Join(append(enforcementErrors, err)...)
		}
		matching, err := a.claimHasMatchingProcess(ctx, state, claim, byGPU[claim.GPU], now)
		if err != nil {
			enforcementErrors = append(enforcementErrors, err)
			continue
		}
		if !matching {
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

	return decisions, errors.Join(enforcementErrors...)
}

// EvictExpiredReservedProcesses removes sampled GPU processes that still match
// an authorization or legacy lease whose entitlement has expired, been
// revoked, or disappeared. Call it before invalid state is pruned so the stale
// scope is still available to identify the affected process.
func (a Authorizer) EvictExpiredReservedProcesses(ctx context.Context, state model.State, processes []model.GPUProcess) ([]Decision, EvictionAssessment, error) {
	var pending []pendingKill
	planner := a
	if !a.DryRun {
		planner.pendingKills = &pending
	}
	decisions, assessment, planningErr := planner.evictExpiredReservedProcesses(ctx, state, processes)
	killErr := a.executePendingKills(ctx, pending)
	if killErr != nil {
		markEvictionAssessmentUncertain(&assessment)
	}
	return decisions, assessment, errors.Join(planningErr, killErr)
}

func (a Authorizer) evictExpiredReservedProcesses(ctx context.Context, state model.State, processes []model.GPUProcess) ([]Decision, EvictionAssessment, error) {
	now := a.now()
	assessment := EvictionAssessment{
		Authorizations: make(map[string]bool),
		Leases:         make(map[string]bool),
	}
	if a.Proc == nil {
		return nil, assessment, nil
	}
	tokensByHash := make(map[string]model.Token, len(state.Tokens))
	for _, token := range state.Tokens {
		tokensByHash[token.Hash] = token
	}

	validReservations := make(map[gpuTokenKey]bool)
	staleReservations := make(map[gpuTokenKey]bool)
	reservationIDs := make(map[gpuTokenKey]string)
	for _, reservation := range state.Reservations {
		if reservation.TokenHash == "" {
			continue
		}
		key := gpuTokenKey{gpu: reservation.GPU, tokenHash: reservation.TokenHash}
		if reservationIDs[key] == "" {
			reservationIDs[key] = reservation.ID
		}
		token, tokenFound := tokensByHash[reservation.TokenHash]
		tokenValid := tokenFound &&
			!token.Revoked &&
			normalizeTokenMode(token.Mode) == model.TokenModeReserved &&
			(!timeIsSet(token.ExpiresAt) || now.Before(token.ExpiresAt))
		if tokenValid && model.ReservationActiveAt(reservation, now) {
			validReservations[key] = true
		} else if !tokenValid || !reservation.Active || reservation.Revoked || !now.Before(reservation.ExpiresAt) {
			// A future, otherwise-valid reservation is not stale evidence. Treating
			// it as stale would evict a matching authorization before its window.
			staleReservations[key] = true
		}
	}
	for _, authorization := range state.Authorizations {
		if authorization.Mode == model.ModeBare || authorization.CgroupPath != "" || authorizationGloballyValid(authorization, tokensByHash, now) {
			continue
		}
		mode := authorizationEvictionMode(authorization, tokensByHash)
		if mode == model.TokenModeClaimed || mode == model.TokenModeManaged || tokenHasKnownReservation(staleReservations, authorization.TokenHash) || tokenHasKnownReservation(validReservations, authorization.TokenHash) {
			assessment.Authorizations[authorization.ID] = true
		}
	}
	for _, lease := range state.Leases {
		if lease.Mode == model.ModeBare || lease.CgroupPath != "" || leaseEntitledOnGPU(lease, lease.GPU, tokensByHash, now) {
			continue
		}
		assessment.Leases[lease.ID] = true
	}

	type cachedProcess struct {
		info model.ProcInfo
		ok   bool
	}
	processesByPID := make(map[int]cachedProcess)
	killed := make(map[int]bool)
	var decisions []Decision
	var evictionErrors []error
	for _, gpuProcess := range processes {
		if err := ctx.Err(); err != nil {
			markEvictionAssessmentUncertain(&assessment)
			evictionErrors = append(evictionErrors, err)
			break
		}
		if gpuProcess.PID <= 0 || killed[gpuProcess.PID] {
			continue
		}

		cached, found := processesByPID[gpuProcess.PID]
		if !found {
			var err error
			cached.info, err = a.Proc.Info(gpuProcess.PID)
			if err != nil {
				if os.IsNotExist(err) {
					processesByPID[gpuProcess.PID] = cached
					continue
				}
				markEvictionAssessmentUncertain(&assessment)
				evictionErrors = append(evictionErrors, fmt.Errorf("inspect sampled pid %d before stale-scope eviction: %w", gpuProcess.PID, err))
				continue
			}
			cached.ok = true
			processesByPID[gpuProcess.PID] = cached
		}
		if !cached.ok {
			continue
		}
		bypassed := BypassMatch(state.Bypasses, cached.info, now, a.BootID)

		var invalidAuthorizations []model.Authorization
		var invalidLeases []model.Lease
		validMatch := false
		resolutionFailed := false
		for _, authorization := range state.Authorizations {
			if err := ctx.Err(); err != nil {
				markEvictionAssessmentUncertain(&assessment)
				evictionErrors = append(evictionErrors, err)
				resolutionFailed = true
				break
			}
			entitled, staleRelevant := authorizationEvictionState(authorization, gpuProcess.GPU, tokensByHash, validReservations, staleReservations, now)
			if !entitled && !staleRelevant {
				continue
			}
			matches, err := a.authorizationScopeMatchesForEviction(ctx, authorization, cached.info)
			if err != nil {
				markEvictionAssessmentUncertain(&assessment)
				evictionErrors = append(evictionErrors, err)
				resolutionFailed = true
				break
			}
			if !matches {
				continue
			}
			if entitled {
				validMatch = true
				break
			}
			invalidAuthorizations = append(invalidAuthorizations, authorization)
		}
		if !validMatch && !resolutionFailed {
			for _, lease := range state.Leases {
				if err := ctx.Err(); err != nil {
					markEvictionAssessmentUncertain(&assessment)
					evictionErrors = append(evictionErrors, err)
					resolutionFailed = true
					break
				}
				entitled, staleRelevant := leaseEvictionState(lease, gpuProcess.GPU, tokensByHash, now)
				if !entitled && !staleRelevant {
					continue
				}
				matches, err := a.leaseScopeMatchesForEviction(ctx, lease, cached.info)
				if err != nil {
					markEvictionAssessmentUncertain(&assessment)
					evictionErrors = append(evictionErrors, err)
					resolutionFailed = true
					break
				}
				if !matches {
					continue
				}
				if entitled {
					validMatch = true
					break
				}
				invalidLeases = append(invalidLeases, lease)
			}
		}
		if resolutionFailed {
			continue
		}
		if validMatch || (len(invalidAuthorizations) == 0 && len(invalidLeases) == 0) {
			continue
		}
		for _, authorization := range invalidAuthorizations {
			assessment.Authorizations[authorization.ID] = false
		}
		for _, lease := range invalidLeases {
			assessment.Leases[lease.ID] = false
		}
		if bypassed {
			continue
		}

		killed[gpuProcess.PID] = true
		reason := fmt.Sprintf("GPU entitlement expired or was revoked on gpu=%d pid=%d", gpuProcess.GPU, gpuProcess.PID)
		view := processView{Process: gpuProcess, Info: cached.info}
		var invalidAuthorization model.Authorization
		var invalidLease model.Lease
		if len(invalidAuthorizations) > 0 {
			invalidAuthorization = invalidAuthorizations[0]
		}
		if len(invalidLeases) > 0 {
			invalidLease = invalidLeases[0]
		}
		leaseID := invalidLease.ID
		authID := invalidAuthorization.ID
		holder := invalidAuthorization.Holder
		tokenHash := invalidAuthorization.TokenHash
		if authID != "" {
			leaseID = reservationIDs[gpuTokenKey{gpu: gpuProcess.GPU, tokenHash: tokenHash}]
		} else {
			holder = invalidLease.Holder
			tokenHash = invalidLease.TokenHash
		}
		if err := a.kill(&decisions, view, reason, leaseID, authID, holder, tokenHash, now); err != nil {
			markEvictionAssessmentUncertain(&assessment)
			evictionErrors = append(evictionErrors, err)
		}
	}
	return decisions, assessment, errors.Join(evictionErrors...)
}

func authorizationEvictionState(authorization model.Authorization, gpu int, tokens map[string]model.Token, validReservations, staleReservations map[gpuTokenKey]bool, now time.Time) (bool, bool) {
	globallyValid := authorizationGloballyValid(authorization, tokens, now)
	switch authorizationEvictionMode(authorization, tokens) {
	case model.TokenModeReserved:
		key := gpuTokenKey{gpu: gpu, tokenHash: authorization.TokenHash}
		if globallyValid {
			if validReservations[key] {
				return true, false
			}
			return false, staleReservations[key]
		}
		return false, validReservations[key] || staleReservations[key]
	case model.TokenModeClaimed:
		return globallyValid, !globallyValid
	case model.TokenModeManaged:
		key := gpuTokenKey{gpu: gpu, tokenHash: authorization.TokenHash}
		if validReservations[key] {
			return globallyValid, !globallyValid
		}
		return globallyValid, !globallyValid || staleReservations[key]
	default:
		return false, false
	}
}

func authorizationGloballyValid(authorization model.Authorization, tokens map[string]model.Token, now time.Time) bool {
	if !authorization.Active || authorization.Revoked || authorizationExpired(authorization, now) {
		return false
	}
	token, ok := tokens[authorization.TokenHash]
	return ok && !token.Revoked && (!token.Managed || authorization.TokenVersion == token.Version) && (!timeIsSet(token.ExpiresAt) || now.Before(token.ExpiresAt))
}

func authorizationEvictionMode(authorization model.Authorization, tokens map[string]model.Token) string {
	if token, ok := tokens[authorization.TokenHash]; ok {
		return normalizeTokenMode(token.Mode)
	}
	return normalizeTokenMode(authorization.TokenMode)
}

func tokenHasKnownReservation(reservations map[gpuTokenKey]bool, tokenHash string) bool {
	for key := range reservations {
		if key.tokenHash == tokenHash {
			return true
		}
	}
	return false
}

func leaseEntitledOnGPU(lease model.Lease, gpu int, tokens map[string]model.Token, now time.Time) bool {
	if !lease.Active || lease.GPU != gpu || !now.Before(lease.ExpiresAt) {
		return false
	}
	if lease.TokenHash == "" {
		return true
	}
	token, ok := tokens[lease.TokenHash]
	return ok && !token.Revoked && (!timeIsSet(token.ExpiresAt) || now.Before(token.ExpiresAt))
}

func leaseEvictionState(lease model.Lease, gpu int, tokens map[string]model.Token, now time.Time) (bool, bool) {
	if lease.GPU != gpu {
		return false, false
	}
	entitled := leaseEntitledOnGPU(lease, gpu, tokens, now)
	return entitled, !entitled
}

func markEvictionAssessmentUncertain(assessment *EvictionAssessment) {
	for id := range assessment.Authorizations {
		assessment.Authorizations[id] = false
	}
	for id := range assessment.Leases {
		assessment.Leases[id] = false
	}
}

func (a Authorizer) BusyProcessesForGPU(ctx context.Context, state model.State, processes []model.GPUProcess, gpu int) ([]Decision, error) {
	now := a.now()
	var busy []Decision
	for _, gpuProcess := range processes {
		if err := ctx.Err(); err != nil {
			return busy, err
		}
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
		if BypassMatch(state.Bypasses, info, now, a.BootID) {
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
		if err := ctx.Err(); err != nil {
			return busy, err
		}
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
		if BypassMatch(state.Bypasses, info, now, a.BootID) {
			continue
		}
		if tentative.Mode != "" {
			matches, err := a.leaseMatchesChecked(ctx, *tentative, info, now)
			if err != nil {
				return busy, err
			}
			if matches {
				continue
			}
		}
		busy = append(busy, Decision{Process: gpuProcess, Info: info, Action: "busy", Reason: "gpu already has non-bypassed process"})
	}
	return busy, nil
}

func (a Authorizer) enforceHard(ctx context.Context, state model.State, gpu int, reservations []model.Reservation, views []processView, decisions *[]Decision, targeted map[int]bool, now time.Time) error {
	tokenFilter := map[string]bool{}
	for _, reservation := range reservations {
		tokenFilter[reservation.TokenHash] = true
	}
	holder := reservationHolder(reservations)
	reservationID := firstReservationID(reservations)
	var enforcementErrors []error
	for _, view := range views {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(enforcementErrors, err)...)
		}
		if view.Bypassed {
			continue
		}
		auth, ok, err := a.matchAnyAuthorization(ctx, state, gpu, view.Info, tokenFilter, "", now)
		if err != nil {
			*decisions = append(*decisions, Decision{Process: view.Process, Info: view.Info, Action: "skip", Reason: "authorization resolution unavailable"})
			enforcementErrors = append(enforcementErrors, err)
			continue
		}
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
		if err := a.killOnce(decisions, targeted, view, reason, reservationID, "", holder, tokenHashForReservations(reservations), now); err != nil {
			enforcementErrors = append(enforcementErrors, err)
		}
	}
	return errors.Join(enforcementErrors...)
}

func usesGPUResources(process model.GPUProcess) bool {
	return process.MemBytesUnknown || process.MemBytes > 0
}

func (a Authorizer) enforceLegacyLeases(ctx context.Context, state model.State, gpu int, leases []model.Lease, views []processView, decisions *[]Decision, targeted map[int]bool, now time.Time) error {
	holder := leaseHolder(leases)
	leaseID := firstLeaseID(leases)
	var enforcementErrors []error
	for _, view := range views {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(enforcementErrors, err)...)
		}
		if view.Bypassed {
			continue
		}
		matchedLeaseID, ok, err := a.matchesAnyLease(ctx, leases, view.Info, now)
		if err != nil {
			*decisions = append(*decisions, Decision{Process: view.Process, Info: view.Info, Action: "skip", Reason: "lease resolution unavailable"})
			enforcementErrors = append(enforcementErrors, err)
			continue
		}
		if ok {
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
		if err := a.killOnce(decisions, targeted, view, reason, leaseID, "", holder, "", now); err != nil {
			enforcementErrors = append(enforcementErrors, err)
		}
	}
	return errors.Join(enforcementErrors...)
}

func (a Authorizer) enforceSoft(ctx context.Context, state model.State, gpu int, views []processView, decisions *[]Decision, targeted map[int]bool, now time.Time) error {
	var enforcementErrors []error
	claims := activeSoftClaimsForGPU(state, gpu, now)
	claimMatches := false
	if len(claims) > 0 {
		var err error
		claimMatches, err = a.claimHasMatchingProcess(ctx, state, claims[0], views, now)
		if err != nil {
			return err
		}
	}
	if len(claims) > 0 && claimMatches {
		claim := claims[0]
		tokenFilter := map[string]bool{claim.TokenHash: true}
		for _, view := range views {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(enforcementErrors, err)...)
			}
			if view.Bypassed {
				continue
			}
			auth, ok, err := a.matchAnyAuthorization(ctx, state, gpu, view.Info, tokenFilter, model.TokenModeClaimed, now)
			if err != nil {
				return err
			}
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
			if err := a.killOnce(decisions, targeted, view, reason, "", claim.AuthorizationID, claim.Holder, claim.TokenHash, now); err != nil {
				enforcementErrors = append(enforcementErrors, err)
			}
		}
		return errors.Join(enforcementErrors...)
	}

	type authorizedView struct {
		view processView
		auth model.Authorization
	}
	var authorized []authorizedView
	var unauthorized []processView
	for _, view := range views {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(enforcementErrors, err)...)
		}
		if view.Bypassed {
			continue
		}
		auth, ok, err := a.matchAnyAuthorization(ctx, state, gpu, view.Info, nil, model.TokenModeClaimed, now)
		if err != nil {
			return err
		}
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
			if err := a.killOnce(decisions, targeted, item.view, reason, "", item.auth.ID, "existing GPU process", item.auth.TokenHash, now); err != nil {
				enforcementErrors = append(enforcementErrors, err)
			}
		}
		return errors.Join(enforcementErrors...)
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
		if err := a.killOnce(decisions, targeted, item.view, reason, "", claimAuth.ID, claimAuth.Holder, claimAuth.TokenHash, now); err != nil {
			enforcementErrors = append(enforcementErrors, err)
		}
	}
	for _, view := range unauthorized {
		reason := fmt.Sprintf("unauthorized GPU access on gpu=%d pid=%d; gpu is claimed by %s", gpu, view.Process.PID, claimAuth.Holder)
		if err := a.killOnce(decisions, targeted, view, reason, "", claimAuth.ID, claimAuth.Holder, claimAuth.TokenHash, now); err != nil {
			enforcementErrors = append(enforcementErrors, err)
		}
	}
	return errors.Join(enforcementErrors...)
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
	if a.pendingKills != nil && !a.DryRun && len(*a.pendingKills) >= maxKillsPerEnforcementPass {
		decision.Action = "skip"
		decision.Reason = "kill budget exhausted; retry on next enforcement pass"
		*decisions = append(*decisions, decision)
		return nil
	}
	*decisions = append(*decisions, decision)
	request := pendingKill{view: view, reason: reason, leaseID: leaseID, authID: authID, holder: holder, tokenHash: tokenHash, now: now}
	if a.pendingKills != nil && !a.DryRun {
		*a.pendingKills = append(*a.pendingKills, request)
		return nil
	}
	err := a.performKill(context.Background(), request)
	a.auditKill(request, err)
	return err
}

func (a Authorizer) performKill(ctx context.Context, request pendingKill) error {
	if a.DryRun || a.Killer == nil {
		return nil
	}
	if a.ValidateKill != nil {
		if err := a.ValidateKill(ctx, request.view.Process); err != nil {
			return fmt.Errorf("revalidate GPU process before kill: %w", err)
		}
	}
	msg := fmt.Sprintf("gpuardian killed pid=%d on gpu=%d: %s", request.view.Process.PID, request.view.Process.GPU, request.reason)
	return a.Killer.Kill(request.view.Info, msg)
}

func (a Authorizer) auditKill(request pendingKill, killErr error) {
	event := model.AuditEvent{
		Time: request.now.UTC(), Message: request.reason, GPU: request.view.Process.GPU, PID: request.view.Process.PID,
		LeaseID: request.leaseID, User: request.holder,
	}
	if a.DryRun {
		event.Kind = "kill_dry_run"
		a.audit(event)
		return
	}
	if killErr != nil {
		event.Kind = "kill_failed"
		event.Message = request.reason + ": " + killErr.Error()
		a.audit(event)
		return
	}
	event.Kind = "kill"
	a.audit(event)
}

func (a Authorizer) executePendingKills(ctx context.Context, pending []pendingKill) error {
	if len(pending) == 0 {
		return nil
	}
	results := make([]error, len(pending))
	_, realKiller := a.Killer.(RealKiller)
	_, realKillerPointer := a.Killer.(*RealKiller)
	canRunConcurrently := realKiller || realKillerPointer
	if !canRunConcurrently || len(pending) == 1 {
		for i := range pending {
			select {
			case <-ctx.Done():
				results[i] = ctx.Err()
			default:
				results[i] = a.performKill(ctx, pending[i])
			}
		}
	} else {
		jobs := make(chan int)
		var wg sync.WaitGroup
		workers := min(8, len(pending))
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for index := range jobs {
					select {
					case <-ctx.Done():
						results[index] = ctx.Err()
					default:
						results[index] = a.performKill(ctx, pending[index])
					}
				}
			}()
		}
	dispatch:
		for index := range pending {
			select {
			case jobs <- index:
			case <-ctx.Done():
				for remaining := index; remaining < len(pending); remaining++ {
					results[remaining] = ctx.Err()
				}
				break dispatch
			}
		}
		close(jobs)
		wg.Wait()
	}
	var killErrors []error
	for i, result := range results {
		a.auditKill(pending[i], result)
		if result != nil {
			killErrors = append(killErrors, result)
		}
	}
	return errors.Join(killErrors...)
}

func (a Authorizer) killOnce(decisions *[]Decision, targeted map[int]bool, view processView, reason, leaseID, authID, holder, tokenHash string, now time.Time) error {
	if targeted[view.Process.PID] {
		*decisions = append(*decisions, Decision{Process: view.Process, Info: view.Info, Action: "skip", Reason: "pid already targeted by enforcement"})
		return nil
	}
	targeted[view.Process.PID] = true
	return a.kill(decisions, view, reason, leaseID, authID, holder, tokenHash, now)
}

func (a Authorizer) claimHasMatchingProcess(ctx context.Context, state model.State, claim model.SoftClaim, views []processView, now time.Time) (bool, error) {
	if claim.TokenHash == "" {
		return false, nil
	}
	for _, view := range views {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if view.Bypassed {
			continue
		}
		_, ok, err := a.matchAnyAuthorization(ctx, state, claim.GPU, view.Info, map[string]bool{claim.TokenHash: true}, model.TokenModeClaimed, now)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func (a Authorizer) matchAnyAuthorization(ctx context.Context, state model.State, gpu int, info model.ProcInfo, tokenFilter map[string]bool, tokenMode string, now time.Time) (model.Authorization, bool, error) {
	var resolutionErrors []error
	for _, authorization := range state.Authorizations {
		if err := ctx.Err(); err != nil {
			resolutionErrors = append(resolutionErrors, err)
			break
		}
		if !authorization.Active || authorization.Revoked || authorizationExpired(authorization, now) {
			continue
		}
		if tokenFilter != nil && !tokenFilter[authorization.TokenHash] {
			continue
		}
		token, ok := a.activeTokens[authorization.TokenHash]
		if a.activeTokens == nil {
			token, ok = activeTokenByHash(state.Tokens, authorization.TokenHash, now)
		}
		if !ok || token.Revoked || (timeIsSet(token.ExpiresAt) && !now.Before(token.ExpiresAt)) {
			continue
		}
		if token.Managed && authorization.TokenVersion != token.Version {
			continue
		}
		mode := normalizeTokenMode(token.Mode)
		if tokenMode != "" && mode != tokenMode && !(tokenMode == model.TokenModeClaimed && mode == model.TokenModeManaged) {
			continue
		}
		authorization.TokenMode = mode
		matches, err := a.authorizationMatchesChecked(ctx, authorization, gpu, info, now)
		if err != nil {
			resolutionErrors = append(resolutionErrors, err)
			continue
		}
		if matches {
			return authorization, true, nil
		}
	}
	return model.Authorization{}, false, errors.Join(resolutionErrors...)
}

func (a Authorizer) authorizationMatches(ctx context.Context, authorization model.Authorization, gpu int, info model.ProcInfo, now time.Time) bool {
	matched, _ := a.authorizationMatchesChecked(ctx, authorization, gpu, info, now)
	return matched
}

func (a Authorizer) authorizationMatchesChecked(ctx context.Context, authorization model.Authorization, gpu int, info model.ProcInfo, now time.Time) (bool, error) {
	if !authorization.Active || authorization.Revoked || authorizationExpired(authorization, now) {
		return false, nil
	}
	return a.authorizationScopeMatchesForEviction(ctx, authorization, info)
}

func (a Authorizer) authorizationScopeMatches(ctx context.Context, authorization model.Authorization, info model.ProcInfo) bool {
	matched, _ := a.authorizationScopeMatchesForEviction(ctx, authorization, info)
	return matched
}

func (a Authorizer) authorizationScopeMatchesForEviction(ctx context.Context, authorization model.Authorization, info model.ProcInfo) (bool, error) {
	switch authorization.Mode {
	case model.ModeBare:
		return bareCgroupMatches(info.Cgroup, authorization.CgroupRel, authorization.CgroupPath), nil
	case model.ModeDocker:
		if authorization.ContainerPattern != "" {
			if info.ContainerID == "" {
				return false, nil
			}
			if a.Runtime == nil {
				return false, fmt.Errorf("resolve docker scope for authorization %s: runtime resolver is unavailable", authorization.ID)
			}
			name, err := a.Runtime.DockerContainerName(ctx, info.ContainerID)
			if err != nil {
				if errors.Is(err, runtime.ErrNotFound) {
					return false, nil
				}
				return false, fmt.Errorf("resolve docker scope for authorization %s: %w", authorization.ID, err)
			}
			return wildcardMatch(authorization.ContainerPattern, name), nil
		}
		return sameContainer(info.ContainerID, authorization.ContainerID), nil
	case model.ModeK8s:
		if info.ContainerID == "" {
			return false, nil
		}
		if a.Runtime == nil {
			return false, fmt.Errorf("resolve k8s scope for authorization %s: runtime resolver is unavailable", authorization.ID)
		}
		ns, err := a.Runtime.NamespaceForContainer(ctx, info.ContainerID)
		if err != nil {
			if errors.Is(err, runtime.ErrNotFound) {
				return false, nil
			}
			return false, fmt.Errorf("resolve k8s scope for authorization %s: %w", authorization.ID, err)
		}
		return wildcardMatch(authorization.Namespace, ns), nil
	case model.ModeUser:
		if info.UID < 0 {
			return false, fmt.Errorf("resolve user scope for authorization %s: process uid is unavailable", authorization.ID)
		}
		if authorization.UID >= 0 {
			return info.UID == authorization.UID, nil
		}
		username := info.Username
		if username == "" {
			lookup := a.UsernameLookup
			if lookup == nil {
				lookup = proc.LookupUsername
			}
			var err error
			username, err = lookup(info.UID)
			if err != nil {
				return false, fmt.Errorf("resolve user scope for authorization %s: %w", authorization.ID, err)
			}
		}
		return authorization.Username != "" && wildcardMatch(authorization.Username, username), nil
	default:
		return false, nil
	}
}

func (a Authorizer) matchesAnyLease(ctx context.Context, leases []model.Lease, info model.ProcInfo, now time.Time) (string, bool, error) {
	var resolutionErrors []error
	for _, lease := range leases {
		if err := ctx.Err(); err != nil {
			resolutionErrors = append(resolutionErrors, err)
			break
		}
		matches, err := a.leaseMatchesChecked(ctx, lease, info, now)
		if err != nil {
			resolutionErrors = append(resolutionErrors, err)
			continue
		}
		if matches {
			return lease.ID, true, nil
		}
	}
	return "", false, errors.Join(resolutionErrors...)
}

func (a Authorizer) leaseMatches(ctx context.Context, lease model.Lease, info model.ProcInfo, now time.Time) bool {
	matched, _ := a.leaseMatchesChecked(ctx, lease, info, now)
	return matched
}

func (a Authorizer) leaseMatchesChecked(ctx context.Context, lease model.Lease, info model.ProcInfo, now time.Time) (bool, error) {
	if !lease.Active || !now.Before(lease.ExpiresAt) {
		return false, nil
	}
	return a.leaseScopeMatchesForEviction(ctx, lease, info)
}

func (a Authorizer) leaseScopeMatches(ctx context.Context, lease model.Lease, info model.ProcInfo) bool {
	matched, _ := a.leaseScopeMatchesForEviction(ctx, lease, info)
	return matched
}

func (a Authorizer) leaseScopeMatchesForEviction(ctx context.Context, lease model.Lease, info model.ProcInfo) (bool, error) {
	switch lease.Mode {
	case model.ModeBare:
		return bareCgroupMatches(info.Cgroup, lease.CgroupRel, lease.CgroupPath), nil
	case model.ModeDocker:
		return sameContainer(info.ContainerID, lease.ContainerID), nil
	case model.ModeK8s:
		if info.ContainerID == "" {
			return false, nil
		}
		if a.Runtime == nil {
			return false, fmt.Errorf("resolve k8s scope for lease %s: runtime resolver is unavailable", lease.ID)
		}
		ns, err := a.Runtime.NamespaceForContainer(ctx, info.ContainerID)
		if err != nil {
			if errors.Is(err, runtime.ErrNotFound) {
				return false, nil
			}
			return false, fmt.Errorf("resolve k8s scope for lease %s: %w", lease.ID, err)
		}
		return ns == lease.Namespace, nil
	default:
		return false, nil
	}
}

func bareCgroupMatches(raw, relative, absolute string) bool {
	targets := make([]string, 0, 2)
	if target := normalizeCgroupPath(relative); target != "" {
		targets = append(targets, target)
	}
	if target := normalizeCgroupMountPath(absolute); target != "" {
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return false
	}
	for _, line := range strings.Split(raw, "\n") {
		candidate := strings.TrimSpace(line)
		if fields := strings.SplitN(candidate, ":", 3); len(fields) == 3 {
			candidate = fields[2]
		}
		actual := normalizeCgroupPath(candidate)
		if actual == "" {
			continue
		}
		for _, target := range targets {
			if actual == target || strings.HasPrefix(actual, target+"/") || target == "/" {
				return true
			}
		}
	}
	return false
}

func normalizeCgroupMountPath(value string) string {
	value = strings.TrimSpace(value)
	const mount = "/sys/fs/cgroup"
	if value == mount {
		return "/"
	}
	if strings.HasPrefix(value, mount+"/") {
		value = strings.TrimPrefix(value, mount)
	}
	return normalizeCgroupPath(value)
}

func normalizeCgroupPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return path.Clean(value)
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
		if token, ok := activeTokenByHash(state.Tokens, claim.TokenHash, now); ok &&
			(normalizeTokenMode(token.Mode) == model.TokenModeClaimed || normalizeTokenMode(token.Mode) == model.TokenModeManaged) {
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

func BypassMatch(rules []model.BypassRule, info model.ProcInfo, now time.Time, bootID string) bool {
	for _, rule := range rules {
		if rule.Revoked || !now.Before(rule.ExpiresAt) {
			continue
		}
		switch rule.Type {
		case model.BypassPID:
			if bootID != "" && rule.BootID == bootID && rule.PID == info.PID && rule.StartTime != 0 && rule.StartTime == info.StartTime {
				return true
			}
		case model.BypassCommand:
			// Path-only executable identity can be spoofed by an unprivileged mount
			// namespace. Legacy non-root command rules therefore never match.
			if rule.UID == 0 && info.UID == 0 && rule.Command != "" && rule.Command == info.CommandPath {
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
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	return len(shorter) >= 12 && strings.HasPrefix(longer, shorter)
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
