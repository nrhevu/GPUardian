package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gpuardian/internal/enforce"
	"gpuardian/internal/gpusmi"
	"gpuardian/internal/model"
	"gpuardian/internal/proc"
	"gpuardian/internal/runtime"
	"gpuardian/internal/telemetry"
)

const telemetryMetricInterval = 5 * time.Second

type observedTelemetryJob struct {
	event    telemetry.JobEvent
	lastSeen time.Time
	missed   int
}

func (s *Server) initializeTelemetry() {
	nodeIDPath := strings.TrimSpace(s.Cfg.NodeIDPath)
	telemetryDir := strings.TrimSpace(s.Cfg.TelemetryDir)
	stateDir := filepath.Dir(s.Cfg.StatePath)
	if nodeIDPath == "" {
		nodeIDPath = filepath.Join(stateDir, "node.id")
	}
	if telemetryDir == "" {
		telemetryDir = filepath.Join(stateDir, "telemetry")
	}
	box, err := telemetry.Open(nodeIDPath, telemetryDir, s.bootID)
	if err != nil {
		_ = s.Store.AppendAudit(model.AuditEvent{Time: time.Now().UTC(), Kind: "telemetry_error", Message: "initialize telemetry: " + err.Error()})
		return
	}
	s.Telemetry = box
	s.emitTelemetry(telemetry.EventDaemonStarted, map[string]any{"started_at": time.Now().UTC()}, time.Now())
	s.bootstrapTelemetry()
}

func (s *Server) bootstrapTelemetry() {
	status, err := s.Store.Status(time.Now())
	if err != nil {
		return
	}
	var events []telemetry.BatchEvent
	defer func() { s.emitTelemetryBatch(events) }()
	groups := make(map[string][]model.ReservationView)
	for _, reservation := range status.Reservations {
		groups[reservation.GroupID] = append(groups[reservation.GroupID], reservation)
	}
	for groupID, reservations := range groups {
		if groupID == "" || len(reservations) == 0 {
			continue
		}
		first := reservations[0]
		payload := telemetry.ReservationUpsert{
			ExternalSessionID: first.ExternalSessionID,
			HistoryQuality:    "partial",
			GroupID:           groupID,
			Holder:            first.Holder,
			Purpose:           first.Purpose,
			CreatedAt:         first.CreatedAt,
			StartsAt:          first.StartsAt,
			ExpiresAt:         first.ExpiresAt,
		}
		for _, reservation := range reservations {
			payload.Members = append(payload.Members, telemetry.ReservationMember{ReservationID: reservation.ID, GPU: reservation.GPU})
		}
		events = append(events, telemetry.BatchEvent{Type: telemetry.EventReservationUpsert, Payload: payload, OccurredAt: time.Now()})
	}
	for _, authorization := range status.Authorizations {
		events = append(events, authorizationViewEvent(authorization, status.Reservations))
	}
}

func (s *Server) emitReservation(token model.Token, reservations []model.Reservation) {
	if len(reservations) == 0 {
		return
	}
	first := reservations[0]
	payload := telemetry.ReservationUpsert{
		ExternalSessionID: first.ExternalSessionID,
		HistoryQuality:    "complete",
		GroupID:           reservationTelemetryGroup(first, token.ID),
		Holder:            first.Holder,
		Purpose:           first.Purpose,
		CreatedAt:         first.CreatedAt,
		StartsAt:          model.ReservationStartsAt(first),
		ExpiresAt:         first.ExpiresAt,
	}
	for _, reservation := range reservations {
		payload.Members = append(payload.Members, telemetry.ReservationMember{ReservationID: reservation.ID, GPU: reservation.GPU})
	}
	s.emitTelemetry(telemetry.EventReservationUpsert, payload, time.Now())
}

func (s *Server) emitAuthorization(tokenID string, authorization model.Authorization) {
	var groupIDs []string
	if state, err := s.Store.EnforcementSnapshot(); err == nil {
		groupIDs = reservationGroupsForToken(state.Reservations, authorization.TokenHash, -1, authorization.CreatedAt)
	}
	if len(groupIDs) == 0 && authorization.TokenMode == model.TokenModeReserved {
		groupIDs = []string{tokenID}
	}
	payload := telemetry.AuthorizationUpsert{
		AuthorizationID:  authorization.ID,
		GroupIDs:         groupIDs,
		Mode:             authorization.Mode,
		Holder:           authorization.Holder,
		Command:          boundedTelemetryCommand(authorization.Command),
		ContainerID:      authorization.ContainerID,
		ContainerPattern: authorization.ContainerPattern,
		Namespace:        authorization.Namespace,
		Username:         authorization.Username,
		CreatedAt:        authorization.CreatedAt,
		ExpiresAt:        authorization.ExpiresAt,
	}
	if len(groupIDs) > 0 {
		payload.GroupID = groupIDs[0]
	}
	s.emitTelemetry(telemetry.EventAuthorizationUpsert, payload, authorization.CreatedAt)
}

func authorizationViewEvent(authorization model.AuthorizationView, reservations []model.ReservationView) telemetry.BatchEvent {
	var groupIDs []string
	for _, reservation := range reservations {
		if reservation.GroupID == "" || !strings.EqualFold(strings.TrimSpace(reservation.Holder), strings.TrimSpace(authorization.Holder)) {
			continue
		}
		if authorization.TokenMode == model.TokenModeReserved && reservation.GroupID != authorization.TokenID {
			continue
		}
		addString(&groupIDs, reservation.GroupID)
	}
	sort.Strings(groupIDs)
	payload := telemetry.AuthorizationUpsert{
		AuthorizationID:  authorization.ID,
		GroupIDs:         groupIDs,
		Mode:             authorization.Mode,
		Holder:           authorization.Holder,
		Command:          boundedTelemetryCommand(authorization.Command),
		ContainerID:      authorization.ContainerID,
		ContainerPattern: authorization.ContainerPattern,
		Namespace:        authorization.Namespace,
		Username:         authorization.Username,
		CreatedAt:        authorization.CreatedAt,
		ExpiresAt:        dereferenceTime(authorization.ExpiresAt),
	}
	if len(groupIDs) > 0 {
		payload.GroupID = groupIDs[0]
	}
	return telemetry.BatchEvent{Type: telemetry.EventAuthorizationUpsert, Payload: payload, OccurredAt: authorization.CreatedAt}
}

func (s *Server) emitAuthorizationEnded(id, reason string, at time.Time) {
	s.emitTelemetryBatch([]telemetry.BatchEvent{authorizationEndedEvent(id, reason, at)})
}

func authorizationEndedEvent(id, reason string, at time.Time) telemetry.BatchEvent {
	return telemetry.BatchEvent{Type: telemetry.EventAuthorizationEnded, Payload: telemetry.AuthorizationEnded{AuthorizationID: id, EndedAt: at.UTC(), Reason: reason}, OccurredAt: at}
}

func (s *Server) emitTelemetry(kind string, payload any, at time.Time) {
	s.emitTelemetryBatch([]telemetry.BatchEvent{{Type: kind, Payload: payload, OccurredAt: at}})
}

// emitTelemetryBatch appends events with one outbox fsync. A pending
// write-failure gap is recorded first. If the gap itself cannot be written the
// batch is dropped and the gap stays pending; if the batch fails part-way, a
// new gap starts at the first event that did not make it.
func (s *Server) emitTelemetryBatch(events []telemetry.BatchEvent) {
	if s.Telemetry == nil || len(events) == 0 {
		return
	}
	s.telemetryWriteMu.Lock()
	defer s.telemetryWriteMu.Unlock()
	batch := events
	gapPending := !s.telemetryGapFrom.IsZero()
	if gapPending {
		at := events[0].OccurredAt
		gap := telemetry.Gap{From: s.telemetryGapFrom, To: at.UTC(), Reason: "outbox_write_failure"}
		batch = append([]telemetry.BatchEvent{{Type: telemetry.EventGap, Payload: gap, OccurredAt: at}}, events...)
	}
	appended, err := s.Telemetry.AppendBatch(batch)
	if err == nil {
		s.telemetryGapFrom = time.Time{}
		return
	}
	if gapPending && len(appended) == 0 {
		_ = s.Store.AppendAudit(model.AuditEvent{Time: time.Now().UTC(), Kind: "telemetry_error", Message: "append telemetry recovery gap: " + err.Error()})
		return
	}
	failed := len(appended)
	if gapPending {
		failed--
	}
	// Every event was written but not synced: treat the whole batch as at risk.
	if failed >= len(events) {
		failed = 0
	}
	s.telemetryGapFrom = events[failed].OccurredAt.UTC()
	_ = s.Store.AppendAudit(model.AuditEvent{Time: time.Now().UTC(), Kind: "telemetry_error", Message: "append telemetry: " + err.Error()})
}

func (s *Server) metricMonitor(ctx context.Context) {
	last := time.Now().UTC()
	s.sampleTelemetryMetrics(ctx, last, last)
	ticker := time.NewTicker(telemetryMetricInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case end := <-ticker.C:
			end = end.UTC()
			s.sampleTelemetryMetrics(ctx, last, end)
			last = end
		}
	}
}

func (s *Server) sampleTelemetryMetrics(ctx context.Context, start, end time.Time) {
	provider, ok := s.GPU.(gpusmi.MetricsProvider)
	if !ok {
		return
	}
	metrics, err := provider.Metrics(ctx)
	s.metricsReadMu.Lock()
	s.metricsReadAt = time.Now()
	s.metricsReadRows = append(s.metricsReadRows[:0], metrics...)
	s.metricsReadErr = err
	s.metricsReadMu.Unlock()
	state, statusErr := s.Store.EnforcementSnapshot()
	if statusErr != nil {
		return
	}
	groupByGPU := make(map[int]string)
	groupsByGPU := make(map[int][]string)
	addGroup := func(gpu int, group string) {
		if group == "" {
			return
		}
		groups := groupsByGPU[gpu]
		if addString(&groups, group) {
			groupsByGPU[gpu] = groups
		}
	}
	for _, reservation := range state.Reservations {
		startsAt := model.ReservationStartsAt(reservation)
		if reservation.Revoked || !startsAt.Before(end) || !reservation.ExpiresAt.After(start) {
			continue
		}
		group := reservationTelemetryGroup(reservation, "")
		groupByGPU[reservation.GPU] = group
		addGroup(reservation.GPU, group)
	}
	s.telemetryJobsMu.Lock()
	for _, job := range s.observedJobs {
		group := claimedTelemetryGroup(job.event)
		for _, gpu := range job.event.GPUs {
			addGroup(gpu, group)
		}
	}
	for _, job := range s.runJobs {
		group := claimedTelemetryGroup(job)
		for _, gpu := range job.GPUs {
			addGroup(gpu, group)
		}
	}
	s.telemetryJobsMu.Unlock()
	metricByGPU := make(map[int]model.GPUMetric, len(metrics))
	gpuSet := make(map[int]bool, len(metrics)+len(groupsByGPU))
	for _, metric := range metrics {
		metricByGPU[metric.GPU] = metric
		gpuSet[metric.GPU] = true
	}
	for gpu := range groupsByGPU {
		gpuSet[gpu] = true
	}
	if len(gpuSet) == 0 {
		return
	}
	gpus := make([]int, 0, len(gpuSet))
	for gpu := range gpuSet {
		gpus = append(gpus, gpu)
	}
	sort.Ints(gpus)
	payload := telemetry.GPUSample{WindowStart: start, WindowEnd: end, Status: "ok"}
	if err != nil {
		payload.Status = "error"
	}
	for _, gpu := range gpus {
		groups := append([]string(nil), groupsByGPU[gpu]...)
		sort.Strings(groups)
		primaryGroup := groupByGPU[gpu]
		if primaryGroup == "" && len(groups) > 0 {
			primaryGroup = groups[0]
		}
		entry := telemetry.GPUSampleEntry{GPU: gpu, GroupID: primaryGroup, GroupIDs: groups}
		if err == nil {
			metric := metricByGPU[gpu]
			entry.UtilizationPct = metric.UtilizationPct
			entry.MemoryUsedBytes = metric.MemoryUsedBytes
			entry.MemoryTotalBytes = metric.MemoryTotalBytes
		}
		payload.GPUs = append(payload.GPUs, entry)
	}
	s.emitTelemetry(telemetry.EventGPUSample, payload, end)
}

// trackObservedTelemetryJobs runs under enforceMu during the monitor pass. It
// never runs docker or kubectl: a new job's runtime metadata comes from the
// resolver cache (usually warm, since enforcement just resolved the same
// container), and a miss is queued for completePendingRuntimeContexts, which
// emits a job.updated carrying the metadata after the lock is released. All
// events of one call are appended to the outbox with a single fsync.
func (s *Server) trackObservedTelemetryJobs(state model.State, decisions []enforce.Decision, now time.Time) {
	if s.Telemetry == nil {
		return
	}
	tokens := make(map[string]model.Token, len(state.Tokens))
	for _, token := range state.Tokens {
		tokens[token.Hash] = token
	}
	authorizations := make(map[string]model.Authorization, len(state.Authorizations))
	for _, authorization := range state.Authorizations {
		authorizations[authorization.ID] = authorization
	}
	seen := make(map[string]bool)
	var events []telemetry.BatchEvent
	s.telemetryJobsMu.Lock()
	defer s.telemetryJobsMu.Unlock()
	// Runs before the unlock above, keeping events ordered with other job
	// updates exactly as when each was emitted under the lock.
	defer func() { s.emitTelemetryBatch(events) }()
	if s.observedJobs == nil {
		s.observedJobs = make(map[string]*observedTelemetryJob)
	}
	currentGroups := make(map[string][]string)
	for _, decision := range decisions {
		if decision.Action != "allow" || decision.Reason == "bypass" || decision.AuthID == "" || decision.Process.PID <= 0 || decision.Info.StartTime == 0 {
			continue
		}
		authorization, ok := authorizations[decision.AuthID]
		if !ok ||
			(authorization.TokenMode != model.TokenModeReserved &&
				authorization.TokenMode != model.TokenModeManaged &&
				authorization.TokenMode != model.TokenModeClaimed) {
			continue
		}
		if authorization.Mode == model.ModeBare {
			if job, ok := s.runJobs[authorization.ID]; ok && addGPU(&job.GPUs, decision.Process.GPU) {
				s.runJobs[authorization.ID] = job
				events = append(events, jobBatchEvent(telemetry.EventJobUpdated, job, now))
			}
			continue
		}
		token, ok := tokens[authorization.TokenHash]
		if !ok || token.ID == "" {
			continue
		}
		groupIDs := reservationGroupsForToken(state.Reservations, authorization.TokenHash, decision.Process.GPU, now)
		if len(groupIDs) == 0 && authorization.TokenMode == model.TokenModeReserved {
			groupIDs = []string{token.ID}
		}
		if len(groupIDs) == 0 && authorization.TokenMode == model.TokenModeReserved {
			continue
		}
		key := fmt.Sprintf("%s/%d/%d", authorization.ID, decision.Process.PID, decision.Info.StartTime)
		for _, groupID := range groupIDs {
			groups := currentGroups[key]
			if addString(&groups, groupID) {
				currentGroups[key] = groups
			}
		}
		seen[key] = true
		job := s.observedJobs[key]
		if job == nil {
			event, lookupPending := s.newObservedTelemetryJobEvent(authorization, decision, groupIDs, now,
				observedExecutionID(s.bootID, authorization.ID, decision.Process.PID, decision.Info.StartTime))
			job = &observedTelemetryJob{event: event, lastSeen: now}
			s.observedJobs[key] = job
			events = append(events, jobBatchEvent(telemetry.EventJobStarted, event, now))
			if lookupPending && len(s.pendingRuntimeContexts) < maxPendingRuntimeContexts {
				s.pendingRuntimeContexts = append(s.pendingRuntimeContexts, pendingRuntimeContext{
					key: key, mode: authorization.Mode, containerID: event.RuntimeContext.ContainerID,
				})
			}
		} else {
			job.lastSeen = now
			job.missed = 0
			groupsChanged := false
			for _, groupID := range groupIDs {
				groupsChanged = addString(&job.event.GroupIDs, groupID) || groupsChanged
			}
			if addGPU(&job.event.GPUs, decision.Process.GPU) || groupsChanged {
				events = append(events, jobBatchEvent(telemetry.EventJobUpdated, job.event, now))
			}
		}
	}
	for key, job := range s.observedJobs {
		if !seen[key] || len(currentGroups[key]) > 0 || (job.event.GroupID == "" && len(job.event.GroupIDs) == 0) ||
			(job.event.TokenMode != model.TokenModeClaimed && job.event.TokenMode != model.TokenModeManaged) {
			continue
		}
		finished := now.UTC()
		previous := job.event
		previous.FinishedAt = &finished
		previous.FinishPrecision = "observed"
		previous.Reason = "reservation_ended"
		events = append(events, jobBatchEvent(telemetry.EventJobFinished, previous, finished))

		continued := job.event
		continued.ExecutionID = observedContinuationExecutionID(s.bootID, continued.AuthorizationID, continued.PID, continued.ProcStartTicks, now)
		continued.GroupID = ""
		continued.GroupIDs = nil
		continued.StartedAt = &finished
		continued.RootExitedAt = nil
		continued.FinishedAt = nil
		continued.StartPrecision = "observed"
		continued.FinishPrecision = ""
		continued.ExitCode = nil
		continued.Reason = ""
		s.observedJobs[key] = &observedTelemetryJob{event: continued, lastSeen: now}
		events = append(events, jobBatchEvent(telemetry.EventJobStarted, continued, now))
	}
	for key, job := range s.observedJobs {
		if seen[key] {
			continue
		}
		job.missed++
		if job.missed < 2 {
			continue
		}
		finished := job.lastSeen.UTC()
		job.event.FinishedAt = &finished
		job.event.FinishPrecision = "observed"
		job.event.Reason = "process_gone"
		events = append(events, jobBatchEvent(telemetry.EventJobFinished, job.event, finished))
		delete(s.observedJobs, key)
	}
}

// maxPendingRuntimeContexts bounds the runtime lookups deferred by one pass.
const maxPendingRuntimeContexts = 256

// pendingRuntimeContext names an observed job by its tracking key (one
// authorization, PID, and start time), so metadata also reaches the job that
// continues it after a reservation ends.
type pendingRuntimeContext struct {
	key         string
	mode        string
	containerID string
}

// jobBatchEvent queues a job event for a batched append. The event is copied
// deeply because the tracked job keeps mutating its slices (addGPU sorts in
// place) before the batch is encoded.
func jobBatchEvent(kind string, event telemetry.JobEvent, at time.Time) telemetry.BatchEvent {
	event.GroupIDs = append([]string(nil), event.GroupIDs...)
	event.GPUs = append([]int(nil), event.GPUs...)
	event.Command = append([]string(nil), event.Command...)
	if event.RuntimeContext != nil {
		runtimeContext := *event.RuntimeContext
		event.RuntimeContext = &runtimeContext
	}
	return telemetry.BatchEvent{Type: kind, Payload: event, OccurredAt: at}
}

// completePendingRuntimeContexts looks up runtime metadata that was not cached
// when observed jobs started, without holding enforceMu or telemetryJobsMu
// during the docker/kubectl calls, and emits a job.updated for each job that
// gained metadata.
func (s *Server) completePendingRuntimeContexts(ctx context.Context) {
	s.telemetryJobsMu.Lock()
	pending := s.pendingRuntimeContexts
	s.pendingRuntimeContexts = nil
	s.telemetryJobsMu.Unlock()
	if len(pending) == 0 || s.Runtime == nil {
		return
	}
	type lookupKey struct{ mode, containerID string }
	resolved := make(map[lookupKey]telemetry.RuntimeContext, len(pending))
	for _, item := range pending {
		key := lookupKey{mode: item.mode, containerID: item.containerID}
		if _, done := resolved[key]; done || ctx.Err() != nil {
			continue
		}
		value := telemetry.RuntimeContext{ContainerID: item.containerID}
		s.resolveRuntimeMetadata(ctx, &value, item.mode)
		resolved[key] = value
	}
	s.telemetryJobsMu.Lock()
	defer s.telemetryJobsMu.Unlock()
	var events []telemetry.BatchEvent
	defer func() { s.emitTelemetryBatch(events) }()
	now := time.Now()
	for _, item := range pending {
		value, ok := resolved[lookupKey{mode: item.mode, containerID: item.containerID}]
		job := s.observedJobs[item.key]
		if !ok || job == nil || job.event.RuntimeContext == nil || job.event.RuntimeContext.ContainerID != item.containerID {
			continue
		}
		merged := *job.event.RuntimeContext
		changed := false
		for _, field := range []struct {
			dst *string
			src string
		}{
			{&merged.DockerContainerName, value.DockerContainerName},
			{&merged.KubernetesNamespace, value.KubernetesNamespace},
			{&merged.KubernetesPodName, value.KubernetesPodName},
			{&merged.KubernetesContainerName, value.KubernetesContainerName},
		} {
			if *field.dst == "" && field.src != "" {
				*field.dst = field.src
				changed = true
			}
		}
		if !changed {
			continue
		}
		job.event.RuntimeContext = &merged
		events = append(events, jobBatchEvent(telemetry.EventJobUpdated, job.event, now))
	}
}

func (s *Server) newObservedTelemetryJobEvent(authorization model.Authorization, decision enforce.Decision, groupIDs []string, at time.Time, executionID string) (telemetry.JobEvent, bool) {
	started := at.UTC()
	runtimeContext, lookupPending := s.buildJobRuntimeContext(decision.Info, authorization.Mode, true)
	event := telemetry.JobEvent{
		ExecutionID:     executionID,
		AuthorizationID: authorization.ID,
		GroupIDs:        append([]string(nil), groupIDs...),
		TokenMode:       authorization.TokenMode,
		Source:          "authorized_process",
		Mode:            authorization.Mode,
		Holder:          authorization.Holder,
		RunName:         authorization.RunName,
		PID:             decision.Process.PID,
		ProcStartTicks:  decision.Info.StartTime,
		Command:         boundedTelemetryCommand(decision.Info.Cmdline),
		GPUs:            []int{decision.Process.GPU},
		StartedAt:       &started,
		StartPrecision:  "observed",
		RuntimeContext:  runtimeContext,
	}
	if len(groupIDs) > 0 {
		event.GroupID = groupIDs[0]
	}
	return event, lookupPending && runtimeContext != nil
}

func (s *Server) rememberRunJob(token model.Token, authorization model.Authorization, pid int, command []string, at time.Time) telemetry.JobEvent {
	started := at.UTC()
	var groupIDs []string
	if state, err := s.Store.EnforcementSnapshot(); err == nil {
		groupIDs = reservationGroupsForToken(state.Reservations, token.Hash, -1, at)
	}
	if len(groupIDs) == 0 && token.Mode == model.TokenModeReserved {
		groupIDs = []string{token.ID}
	}
	event := telemetry.JobEvent{
		ExecutionID:     "exec_" + authorization.ID,
		AuthorizationID: authorization.ID,
		GroupIDs:        groupIDs,
		TokenMode:       authorization.TokenMode,
		Source:          "gpuardian_run",
		Mode:            authorization.Mode,
		Holder:          authorization.Holder,
		RunName:         authorization.RunName,
		PID:             pid,
		Command:         boundedTelemetryCommand(command),
		StartedAt:       &started,
		StartPrecision:  "exact",
		RuntimeContext: s.jobRuntimeContext(model.ProcInfo{
			UID: authorization.UID, Username: authorization.Username, ContainerID: authorization.ContainerID,
		}, authorization.Mode),
	}
	if len(groupIDs) > 0 {
		event.GroupID = groupIDs[0]
	}
	if s.Telemetry == nil {
		return event
	}
	s.telemetryJobsMu.Lock()
	if s.runJobs == nil {
		s.runJobs = make(map[string]telemetry.JobEvent)
	}
	s.runJobs[authorization.ID] = event
	s.telemetryJobsMu.Unlock()
	s.emitTelemetry(telemetry.EventJobStarted, event, at)
	return event
}

func (s *Server) jobRuntimeContext(info model.ProcInfo, mode string) *telemetry.RuntimeContext {
	value, _ := s.buildJobRuntimeContext(info, mode, false)
	return value
}

// buildJobRuntimeContext assembles a job's runtime context. With cachedOnly
// and a resolver that implements runtime.CachedLookup, docker and Kubernetes
// metadata come from the resolver cache only and lookupPending reports a miss
// for the caller to resolve later outside its locks.
func (s *Server) buildJobRuntimeContext(info model.ProcInfo, mode string, cachedOnly bool) (_ *telemetry.RuntimeContext, lookupPending bool) {
	value := telemetry.RuntimeContext{ContainerID: strings.TrimSpace(info.ContainerID)}
	if info.UID >= 0 {
		uid := info.UID
		value.UID = &uid
		value.Username = strings.TrimSpace(info.Username)
		if value.Username == "" {
			value.Username, _ = proc.LookupUsername(info.UID)
		}
	}
	if s.Runtime != nil && value.ContainerID != "" {
		cache, cached := s.Runtime.(runtime.CachedLookup)
		switch {
		case cachedOnly && cached && mode == model.ModeDocker:
			name, ok := cache.CachedDockerContainerName(value.ContainerID)
			value.DockerContainerName = name
			lookupPending = !ok
		case cachedOnly && cached && mode == model.ModeK8s:
			workload, ok := cache.CachedKubernetesWorkload(value.ContainerID)
			value.KubernetesNamespace = workload.Namespace
			value.KubernetesPodName = workload.PodName
			value.KubernetesContainerName = workload.ContainerName
			lookupPending = !ok
		default:
			s.resolveRuntimeMetadata(context.Background(), &value, mode)
		}
	}
	if value.UID == nil && value.Username == "" && value.ContainerID == "" && value.DockerContainerName == "" &&
		value.KubernetesNamespace == "" && value.KubernetesPodName == "" && value.KubernetesContainerName == "" {
		return nil, false
	}
	return &value, lookupPending
}

// resolveRuntimeMetadata fills docker or Kubernetes metadata for
// value.ContainerID through the runtime resolver, bounded to two seconds.
func (s *Server) resolveRuntimeMetadata(ctx context.Context, value *telemetry.RuntimeContext, mode string) {
	if s.Runtime == nil || value.ContainerID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	switch mode {
	case model.ModeDocker:
		value.DockerContainerName, _ = s.Runtime.DockerContainerName(ctx, value.ContainerID)
	case model.ModeK8s:
		if resolver, ok := s.Runtime.(runtime.KubernetesWorkloadResolver); ok {
			workload, err := resolver.KubernetesWorkloadForContainer(ctx, value.ContainerID)
			if err == nil {
				value.KubernetesNamespace = workload.Namespace
				value.KubernetesPodName = workload.PodName
				value.KubernetesContainerName = workload.ContainerName
			}
		} else {
			value.KubernetesNamespace, _ = s.Runtime.NamespaceForContainer(ctx, value.ContainerID)
		}
	}
}

func reservationTelemetryGroup(reservation model.Reservation, fallback string) string {
	if reservation.GroupID != "" {
		return reservation.GroupID
	}
	return fallback
}

func claimedTelemetryGroup(event telemetry.JobEvent) string {
	if event.ExecutionID == "" || event.GroupID != "" || len(event.GroupIDs) > 0 {
		return ""
	}
	if event.TokenMode != model.TokenModeClaimed && event.TokenMode != model.TokenModeManaged {
		return ""
	}
	if event.AuthorizationID != "" {
		return "claimed-auth:" + event.AuthorizationID
	}
	return "claimed:" + event.ExecutionID
}

func reservationGroupsForToken(reservations []model.Reservation, tokenHash string, gpu int, at time.Time) []string {
	var groups []string
	for _, reservation := range reservations {
		if reservation.TokenHash != tokenHash || (gpu >= 0 && reservation.GPU != gpu) || !model.ReservationActiveAt(reservation, at) {
			continue
		}
		if reservation.GroupID != "" {
			addString(&groups, reservation.GroupID)
		}
	}
	sort.Strings(groups)
	return groups
}

func addString(values *[]string, value string) bool {
	for _, existing := range *values {
		if existing == value {
			return false
		}
	}
	*values = append(*values, value)
	return true
}

func (s *Server) updateRunJobRootExit(authorizationID string, exitCode int, at time.Time) {
	if s.Telemetry == nil {
		return
	}
	s.telemetryJobsMu.Lock()
	event, ok := s.runJobs[authorizationID]
	if ok {
		exited := at.UTC()
		event.RootExitedAt = &exited
		event.ExitCode = &exitCode
		s.runJobs[authorizationID] = event
	}
	s.telemetryJobsMu.Unlock()
	if ok {
		s.emitTelemetry(telemetry.EventJobUpdated, event, at)
	}
}

func (s *Server) finishRunJob(authorizationID, reason string, at time.Time) {
	if event, ok := s.takeFinishedRunJob(authorizationID, reason, at); ok {
		s.emitTelemetryBatch([]telemetry.BatchEvent{event})
	}
}

// takeFinishedRunJob ends a tracked run job and returns its job.finished event
// for the caller to emit, possibly batched with other events.
func (s *Server) takeFinishedRunJob(authorizationID, reason string, at time.Time) (telemetry.BatchEvent, bool) {
	if s.Telemetry == nil {
		return telemetry.BatchEvent{}, false
	}
	s.telemetryJobsMu.Lock()
	defer s.telemetryJobsMu.Unlock()
	event, ok := s.runJobs[authorizationID]
	if !ok {
		return telemetry.BatchEvent{}, false
	}
	finished := at.UTC()
	event.FinishedAt = &finished
	event.FinishPrecision = "exact"
	event.Reason = reason
	delete(s.runJobs, authorizationID)
	return jobBatchEvent(telemetry.EventJobFinished, event, at), true
}

func boundedTelemetryCommand(command []string) []string {
	out := make([]string, 0, min(len(command), 128))
	remaining := 16 << 10
	for _, argument := range command {
		if len(out) >= 128 || remaining <= 0 {
			break
		}
		argument = strings.ToValidUTF8(argument, "?")
		if len(argument) > remaining {
			argument = argument[:remaining]
			for !utf8.ValidString(argument) {
				argument = argument[:len(argument)-1]
			}
		}
		out = append(out, argument)
		remaining -= len(argument)
	}
	return out
}

func observedExecutionID(bootID, authID string, pid int, start uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", bootID, authID, pid, start)))
	return "exec_" + hex.EncodeToString(sum[:12])
}

func observedContinuationExecutionID(bootID, authID string, pid int, start uint64, at time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", bootID, authID, pid, start, at.UTC().UnixNano())))
	return "exec_" + hex.EncodeToString(sum[:12])
}

func addGPU(values *[]int, gpu int) bool {
	for _, existing := range *values {
		if existing == gpu {
			return false
		}
	}
	*values = append(*values, gpu)
	sort.Ints(*values)
	return true
}

func dereferenceTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}
