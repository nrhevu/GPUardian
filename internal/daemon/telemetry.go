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

	"rocguard/internal/amdsmi"
	"rocguard/internal/enforce"
	"rocguard/internal/model"
	"rocguard/internal/telemetry"
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
		s.emitTelemetry(telemetry.EventReservationUpsert, payload, time.Now())
	}
	for _, authorization := range status.Authorizations {
		s.emitAuthorizationView(authorization)
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
		GroupID:           token.ID,
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
	s.emitTelemetry(telemetry.EventAuthorizationUpsert, telemetry.AuthorizationUpsert{
		AuthorizationID:  authorization.ID,
		GroupID:          tokenID,
		Mode:             authorization.Mode,
		Holder:           authorization.Holder,
		Command:          boundedTelemetryCommand(authorization.Command),
		ContainerID:      authorization.ContainerID,
		ContainerPattern: authorization.ContainerPattern,
		Namespace:        authorization.Namespace,
		Username:         authorization.Username,
		CreatedAt:        authorization.CreatedAt,
		ExpiresAt:        authorization.ExpiresAt,
	}, authorization.CreatedAt)
}

func (s *Server) emitAuthorizationView(authorization model.AuthorizationView) {
	s.emitTelemetry(telemetry.EventAuthorizationUpsert, telemetry.AuthorizationUpsert{
		AuthorizationID:  authorization.ID,
		GroupID:          authorization.TokenID,
		Mode:             authorization.Mode,
		Holder:           authorization.Holder,
		Command:          boundedTelemetryCommand(authorization.Command),
		ContainerID:      authorization.ContainerID,
		ContainerPattern: authorization.ContainerPattern,
		Namespace:        authorization.Namespace,
		Username:         authorization.Username,
		CreatedAt:        authorization.CreatedAt,
		ExpiresAt:        dereferenceTime(authorization.ExpiresAt),
	}, authorization.CreatedAt)
}

func (s *Server) emitAuthorizationEnded(id, reason string, at time.Time) {
	s.emitTelemetry(telemetry.EventAuthorizationEnded, telemetry.AuthorizationEnded{AuthorizationID: id, EndedAt: at.UTC(), Reason: reason}, at)
}

func (s *Server) emitTelemetry(kind string, payload any, at time.Time) {
	if s.Telemetry == nil {
		return
	}
	s.telemetryWriteMu.Lock()
	defer s.telemetryWriteMu.Unlock()
	if !s.telemetryGapFrom.IsZero() {
		gap := telemetry.Gap{From: s.telemetryGapFrom, To: at.UTC(), Reason: "outbox_write_failure"}
		if _, err := s.Telemetry.Append(telemetry.EventGap, gap, at); err != nil {
			_ = s.Store.AppendAudit(model.AuditEvent{Time: time.Now().UTC(), Kind: "telemetry_error", Message: "append telemetry recovery gap: " + err.Error()})
			return
		}
		s.telemetryGapFrom = time.Time{}
	}
	if _, err := s.Telemetry.Append(kind, payload, at); err != nil {
		if s.telemetryGapFrom.IsZero() {
			s.telemetryGapFrom = at.UTC()
		}
		_ = s.Store.AppendAudit(model.AuditEvent{Time: time.Now().UTC(), Kind: "telemetry_error", Message: "append telemetry: " + err.Error()})
	}
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
	provider, ok := s.AMD.(amdsmi.MetricsProvider)
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
	tokenIDs := make(map[string]string, len(state.Tokens))
	for _, token := range state.Tokens {
		tokenIDs[token.Hash] = token.ID
	}
	groupByGPU := make(map[int]string)
	for _, reservation := range state.Reservations {
		startsAt := model.ReservationStartsAt(reservation)
		if reservation.Revoked || !startsAt.Before(end) || !reservation.ExpiresAt.After(start) {
			continue
		}
		if groupID := tokenIDs[reservation.TokenHash]; groupID != "" {
			groupByGPU[reservation.GPU] = groupID
		}
	}
	if len(groupByGPU) == 0 {
		return
	}
	metricByGPU := make(map[int]model.GPUMetric, len(metrics))
	for _, metric := range metrics {
		metricByGPU[metric.GPU] = metric
	}
	gpus := make([]int, 0, len(groupByGPU))
	for gpu := range groupByGPU {
		gpus = append(gpus, gpu)
	}
	sort.Ints(gpus)
	payload := telemetry.GPUSample{WindowStart: start, WindowEnd: end, Status: "ok"}
	if err != nil {
		payload.Status = "error"
	}
	for _, gpu := range gpus {
		entry := telemetry.GPUSampleEntry{GPU: gpu, GroupID: groupByGPU[gpu]}
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
	s.telemetryJobsMu.Lock()
	defer s.telemetryJobsMu.Unlock()
	if s.observedJobs == nil {
		s.observedJobs = make(map[string]*observedTelemetryJob)
	}
	for _, decision := range decisions {
		if decision.Action != "allow" || decision.Reason == "bypass" || decision.AuthID == "" || decision.Process.PID <= 0 || decision.Info.StartTime == 0 {
			continue
		}
		authorization, ok := authorizations[decision.AuthID]
		if !ok || authorization.Mode == model.ModeBare || authorization.TokenMode != model.TokenModeReserved {
			continue
		}
		token, ok := tokens[authorization.TokenHash]
		if !ok || token.ID == "" {
			continue
		}
		key := fmt.Sprintf("%s/%d/%d", authorization.ID, decision.Process.PID, decision.Info.StartTime)
		seen[key] = true
		job := s.observedJobs[key]
		if job == nil {
			started := now.UTC()
			event := telemetry.JobEvent{
				ExecutionID:     observedExecutionID(s.bootID, authorization.ID, decision.Process.PID, decision.Info.StartTime),
				AuthorizationID: authorization.ID,
				GroupID:         token.ID,
				Source:          "authorized_process",
				Mode:            authorization.Mode,
				Holder:          authorization.Holder,
				PID:             decision.Process.PID,
				ProcStartTicks:  decision.Info.StartTime,
				Command:         boundedTelemetryCommand(decision.Info.Cmdline),
				GPUs:            []int{decision.Process.GPU},
				StartedAt:       &started,
				StartPrecision:  "observed",
			}
			job = &observedTelemetryJob{event: event, lastSeen: now}
			s.observedJobs[key] = job
			s.emitTelemetry(telemetry.EventJobStarted, event, now)
		} else {
			job.lastSeen = now
			job.missed = 0
			if addGPU(&job.event.GPUs, decision.Process.GPU) {
				s.emitTelemetry(telemetry.EventJobUpdated, job.event, now)
			}
		}
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
		s.emitTelemetry(telemetry.EventJobFinished, job.event, finished)
		delete(s.observedJobs, key)
	}
}

func (s *Server) rememberRunJob(token model.Token, authorization model.Authorization, pid int, command []string, at time.Time) telemetry.JobEvent {
	started := at.UTC()
	event := telemetry.JobEvent{
		ExecutionID:     "exec_" + authorization.ID,
		AuthorizationID: authorization.ID,
		GroupID:         token.ID,
		Source:          "rocguard_run",
		Mode:            authorization.Mode,
		Holder:          authorization.Holder,
		PID:             pid,
		Command:         boundedTelemetryCommand(command),
		StartedAt:       &started,
		StartPrecision:  "exact",
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
	if s.Telemetry == nil {
		return
	}
	s.telemetryJobsMu.Lock()
	event, ok := s.runJobs[authorizationID]
	if ok {
		finished := at.UTC()
		event.FinishedAt = &finished
		event.FinishPrecision = "exact"
		event.Reason = reason
		delete(s.runJobs, authorizationID)
	}
	s.telemetryJobsMu.Unlock()
	if ok {
		s.emitTelemetry(telemetry.EventJobFinished, event, at)
	}
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
