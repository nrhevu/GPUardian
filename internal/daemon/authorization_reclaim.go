package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"gpuardian/internal/model"
	"gpuardian/internal/runtime"
	"gpuardian/internal/store"
	"gpuardian/internal/telemetry"
)

const (
	// staleDockerAuthorizationAge keeps a new Docker authorization even when
	// its container does not exist yet, so `allow docker --container NAME`
	// may precede `docker run --name NAME`.
	staleDockerAuthorizationAge = 10 * time.Minute
	// maxReclaimedDockerAuthorizations bounds how many authorizations one
	// admission at the per-token limit releases, oldest first.
	maxReclaimedDockerAuthorizations = 8
)

// withAuthorizationCapacity runs add and, when the token is at its active
// authorization limit, reclaims the token's stale Docker authorizations and
// runs add once more. add must take enforceMu itself.
func (s *Server) withAuthorizationCapacity(tokenHash string, add func() error) error {
	err := add()
	if errors.Is(err, store.ErrAuthorizationLimit) && s.reclaimStaleDockerAuthorizations(tokenHash) {
		err = add()
	}
	return err
}

// reclaimStaleDockerAuthorizations releases the oldest Docker authorizations
// of tokenHash whose container no longer exists. Authorizations under a fixed
// managed key never expire, so without this every removed container would keep
// counting toward the per-token limit. Wildcard patterns, recent
// authorizations, and authorizations backing a GPU claim are kept. It must be
// called without enforceMu held because the container listing runs the Docker
// CLI. It reports whether anything was released.
func (s *Server) reclaimStaleDockerAuthorizations(tokenHash string) bool {
	lister, ok := s.Runtime.(runtime.DockerContainerLister)
	if !ok || tokenHash == "" {
		return false
	}
	state, err := s.Store.EnforcementSnapshot()
	if err != nil || len(reclaimableDockerAuthorizations(state, tokenHash, time.Now())) == 0 {
		return false
	}
	containers, err := lister.DockerContainers(context.Background())
	if err != nil {
		return false
	}
	now := time.Now()
	released := func() []model.Authorization {
		s.enforceMu.Lock()
		defer s.enforceMu.Unlock()
		state, err := s.Store.EnforcementSnapshot()
		if err != nil {
			return nil
		}
		stale := staleDockerAuthorizations(reclaimableDockerAuthorizations(state, tokenHash, now), containers)
		if len(stale) == 0 || s.Store.ReleaseScopes(nil, authorizationIDs(stale), nil) != nil {
			return nil
		}
		return stale
	}()
	if len(released) == 0 {
		return false
	}
	events := make([]telemetry.BatchEvent, 0, len(released))
	audits := make([]model.AuditEvent, 0, len(released))
	for _, authorization := range released {
		events = append(events, authorizationEndedEvent(authorization.ID, "container_removed", now))
		audits = append(audits, model.AuditEvent{
			Time:    now.UTC(),
			Kind:    "authorization_released",
			Message: "docker authorization released at the per-key limit: container no longer exists",
			LeaseID: authorization.ID,
			User:    authorization.Holder,
		})
	}
	s.emitTelemetryBatch(events)
	_ = s.Store.AppendAudits(audits)
	return true
}

// reclaimableDockerAuthorizations returns the token's active, non-wildcard
// Docker authorizations that are old enough to reclaim and back no GPU claim.
func reclaimableDockerAuthorizations(state model.State, tokenHash string, now time.Time) []model.Authorization {
	claimed := make(map[string]bool, len(state.SoftClaims))
	for _, claim := range state.SoftClaims {
		claimed[claim.AuthorizationID] = true
	}
	var out []model.Authorization
	for _, authorization := range state.Authorizations {
		if authorization.TokenHash != tokenHash || authorization.Mode != model.ModeDocker ||
			!authorization.Active || authorization.Revoked || authorizationExpiredAt(authorization, now) ||
			claimed[authorization.ID] || now.Sub(authorization.CreatedAt) < staleDockerAuthorizationAge {
			continue
		}
		if authorization.ContainerID == "" && (authorization.ContainerPattern == "" || hasWildcard(authorization.ContainerPattern)) {
			continue
		}
		out = append(out, authorization)
	}
	return out
}

// staleDockerAuthorizations keeps the candidates whose container is absent
// from the listing, oldest first, at most maxReclaimedDockerAuthorizations.
// A stored ContainerID is the full ID from docker inspect, and a pattern
// without a wildcard matches one exact container name.
func staleDockerAuthorizations(candidates []model.Authorization, containers []runtime.DockerContainer) []model.Authorization {
	ids := make(map[string]bool, len(containers))
	names := make(map[string]bool, len(containers))
	for _, container := range containers {
		ids[container.ID] = true
		for _, name := range container.Names {
			names[name] = true
		}
	}
	var stale []model.Authorization
	for _, authorization := range candidates {
		if authorization.ContainerID != "" {
			if ids[strings.ToLower(strings.TrimSpace(authorization.ContainerID))] {
				continue
			}
		} else if names[strings.TrimSpace(authorization.ContainerPattern)] {
			continue
		}
		stale = append(stale, authorization)
	}
	slices.SortFunc(stale, func(a, b model.Authorization) int { return a.CreatedAt.Compare(b.CreatedAt) })
	if len(stale) > maxReclaimedDockerAuthorizations {
		stale = stale[:maxReclaimedDockerAuthorizations]
	}
	return stale
}
