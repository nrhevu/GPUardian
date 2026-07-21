package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"gpuardian/internal/telemetry"
)

func (s *Store) ApplyPage(ctx context.Context, serverID, serverName string, page telemetry.Page) error {
	return s.ApplyPageWithOwners(ctx, serverID, serverName, page, nil)
}

func (s *Store) ApplyPageWithOwners(ctx context.Context, serverID, serverName string, page telemetry.Page, knownOwners map[string]bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if strings.TrimSpace(page.NodeID) == "" || strings.TrimSpace(page.StreamID) == "" {
		return errors.New("telemetry page is missing node or stream identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO nodes(node_id,last_server_id,first_seen_at_ms,last_seen_at_ms)
		VALUES(?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET last_server_id=excluded.last_server_id,last_seen_at_ms=excluded.last_seen_at_ms`,
		page.NodeID, serverID, now, now); err != nil {
		return err
	}
	for _, event := range page.Events {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ingested_event_ids(node_id,stream_id,seq,ingested_at_ms) VALUES(?,?,?,?)`, page.NodeID, page.StreamID, event.Seq, now)
		if err != nil {
			return err
		}
		inserted, _ := result.RowsAffected()
		if inserted == 0 {
			continue
		}
		if err := applyEvent(ctx, tx, page.NodeID, serverID, serverName, event, knownOwners); err != nil {
			return fmt.Errorf("apply telemetry event %d (%s): %w", event.Seq, event.Type, err)
		}
	}
	lastSeq := uint64(0)
	if len(page.Events) > 0 {
		lastSeq = page.Events[len(page.Events)-1].Seq
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO node_sync_state(server_id,node_id,stream_id,cursor,last_seq,last_sync_at_ms,sync_error)
		VALUES(?,?,?,?,?,?, '') ON CONFLICT(server_id) DO UPDATE SET node_id=excluded.node_id,stream_id=excluded.stream_id,
		cursor=excluded.cursor,last_seq=MAX(node_sync_state.last_seq,excluded.last_seq),last_sync_at_ms=excluded.last_sync_at_ms,sync_error=''`,
		serverID, page.NodeID, page.StreamID, page.NextCursor, lastSeq, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM ingested_event_ids WHERE ingested_at_ms < ?", time.Now().Add(-48*time.Hour).UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM gpu_minute_rollups WHERE minute_ms < ?", time.Now().Add(-90*24*time.Hour).UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SyncCursor(ctx context.Context, serverID string) (string, error) {
	var cursor string
	err := s.db.QueryRowContext(ctx, "SELECT cursor FROM node_sync_state WHERE server_id=?", serverID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

func (s *Store) MarkGap(ctx context.Context, serverID, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cutoff := int64(0)
	var lastSync sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT last_sync_at_ms FROM node_sync_state WHERE server_id=?", serverID).Scan(&lastSync); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if lastSync.Valid {
		cutoff = lastSync.Int64
	}
	if _, err := tx.ExecContext(ctx, "UPDATE node_sync_state SET gap_at_ms=?,sync_error=? WHERE server_id=?", now, reason, serverID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE reservation_sessions SET history_quality='partial',updated_at_ms=?
		WHERE server_id=? AND expires_at_ms>=?`, now, serverID, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

func applyEvent(ctx context.Context, tx *sql.Tx, nodeID, serverID, serverName string, event telemetry.Event, knownOwners map[string]bool) error {
	switch event.Type {
	case telemetry.EventReservationUpsert:
		var payload telemetry.ReservationUpsert
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		return applyReservation(ctx, tx, nodeID, serverID, serverName, payload, event.OccurredAt, knownOwners)
	case telemetry.EventReservationEnded:
		var payload telemetry.ReservationEnded
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		if payload.Reason == "revoked" {
			if _, err := tx.ExecContext(ctx, "UPDATE reservation_sessions SET revoked_at_ms=?,finalized_at_ms=?,updated_at_ms=? WHERE node_id=? AND group_id=?", millis(payload.EndedAt), millis(payload.EndedAt), millis(event.OccurredAt), nodeID, payload.GroupID); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, "UPDATE reservation_sessions SET finalized_at_ms=?,updated_at_ms=? WHERE node_id=? AND group_id=?", millis(payload.EndedAt), millis(event.OccurredAt), nodeID, payload.GroupID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET
			finished_at_ms=CASE WHEN finished_at_ms IS NULL OR finished_at_ms>? THEN ? ELSE finished_at_ms END,
			root_exited_at_ms=CASE WHEN root_exited_at_ms>? THEN ? ELSE root_exited_at_ms END
			WHERE session_id=(SELECT session_id FROM reservation_sessions WHERE node_id=? AND group_id=?)`,
			millis(payload.EndedAt), millis(payload.EndedAt), millis(payload.EndedAt), millis(payload.EndedAt), nodeID, payload.GroupID)
		return err
	case telemetry.EventAuthorizationUpsert:
		var payload telemetry.AuthorizationUpsert
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		return applyAuthorization(ctx, tx, nodeID, payload)
	case telemetry.EventAuthorizationEnded:
		var payload telemetry.AuthorizationEnded
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE authorization_scopes SET ended_at_ms=?,end_reason=? WHERE node_id=? AND authorization_id=?", millis(payload.EndedAt), payload.Reason, nodeID, payload.AuthorizationID)
		return err
	case telemetry.EventJobStarted, telemetry.EventJobUpdated, telemetry.EventJobFinished:
		var payload telemetry.JobEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		return applyJob(ctx, tx, nodeID, payload, event.OccurredAt)
	case telemetry.EventGPUSample:
		var payload telemetry.GPUSample
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		return applyGPUSample(ctx, tx, nodeID, payload)
	case telemetry.EventGap:
		var payload telemetry.Gap
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		from := payload.From
		if from.IsZero() {
			from = event.OccurredAt.Add(-24 * time.Hour)
		}
		_, err := tx.ExecContext(ctx, "UPDATE reservation_sessions SET history_quality='partial',updated_at_ms=? WHERE node_id=? AND expires_at_ms>=?", millis(event.OccurredAt), nodeID, millis(from))
		return err
	default:
		return nil
	}
}

func applyReservation(ctx context.Context, tx *sql.Tx, nodeID, serverID, serverName string, payload telemetry.ReservationUpsert, occurredAt time.Time, knownOwners map[string]bool) error {
	if payload.GroupID == "" || payload.Holder == "" || !payload.ExpiresAt.After(payload.StartsAt) {
		return errors.New("invalid reservation telemetry")
	}
	id := strings.TrimSpace(payload.ExternalSessionID)
	if !strings.HasPrefix(id, "sess_") || len(id) > 128 {
		id = sessionID(nodeID, payload.GroupID)
	}
	source := "cli"
	ownerEditable := knownOwners[strings.ToLower(strings.TrimSpace(payload.Holder))]
	quality := "complete"
	if payload.HistoryQuality == "partial" {
		quality = "partial"
	}
	if payload.ExternalSessionID != "" {
		source = "web"
		ownerEditable = true
		result, err := tx.ExecContext(ctx, `UPDATE reservation_sessions SET node_id=?,server_id=?,server_name=?,group_id=?,owner_username=?,purpose=?,
			starts_at_ms=?,expires_at_ms=?,history_quality=CASE WHEN history_quality='partial' OR ?='partial' THEN 'partial' ELSE 'complete' END,
			owner_editable=1,provisioning=0,updated_at_ms=? WHERE session_id=?`, nodeID, serverID, serverName, payload.GroupID,
			payload.Holder, payload.Purpose, millis(payload.StartsAt), millis(payload.ExpiresAt), quality, millis(occurredAt), id)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed > 0 {
			for _, member := range payload.Members {
				if _, err := tx.ExecContext(ctx, `INSERT INTO session_gpus(session_id,gpu,reservation_id) VALUES(?,?,?)
					ON CONFLICT(session_id,gpu) DO UPDATE SET reservation_id=excluded.reservation_id`, id, member.GPU, member.ReservationID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO session_gpu_summaries(session_id,gpu) VALUES(?,?)", id, member.GPU); err != nil {
					return err
				}
			}
			return nil
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO reservation_sessions(session_id,node_id,server_id,server_name,group_id,owner_username,owner_editable,purpose,source,
		created_at_ms,starts_at_ms,expires_at_ms,history_quality,provisioning,updated_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,0,?)
		ON CONFLICT(node_id,group_id) DO UPDATE SET server_id=excluded.server_id,server_name=excluded.server_name,
		owner_username=excluded.owner_username,purpose=excluded.purpose,starts_at_ms=excluded.starts_at_ms,expires_at_ms=excluded.expires_at_ms,
		owner_editable=MAX(reservation_sessions.owner_editable,excluded.owner_editable),provisioning=0,updated_at_ms=excluded.updated_at_ms`,
		id, nodeID, serverID, serverName, payload.GroupID, payload.Holder, ownerEditable, payload.Purpose, source,
		millis(payload.CreatedAt), millis(payload.StartsAt), millis(payload.ExpiresAt), quality, millis(occurredAt))
	if err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT session_id FROM reservation_sessions WHERE node_id=? AND group_id=?", nodeID, payload.GroupID).Scan(&id); err != nil {
		return err
	}
	for _, member := range payload.Members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_gpus(session_id,gpu,reservation_id) VALUES(?,?,?)
			ON CONFLICT(session_id,gpu) DO UPDATE SET reservation_id=excluded.reservation_id`, id, member.GPU, member.ReservationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO session_gpu_summaries(session_id,gpu) VALUES(?,?)", id, member.GPU); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO session_results(session_id) VALUES(?)", id)
	return err
}

func applyAuthorization(ctx context.Context, tx *sql.Tx, nodeID string, payload telemetry.AuthorizationUpsert) error {
	var sessionID string
	groups := telemetryGroups(payload.GroupID, payload.GroupIDs)
	for _, groupID := range groups {
		var candidate string
		if err := tx.QueryRowContext(ctx, "SELECT session_id FROM reservation_sessions WHERE node_id=? AND group_id=?", nodeID, groupID).Scan(&candidate); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		} else if err == nil && sessionID == "" {
			sessionID = candidate
		}
	}
	command, _ := json.Marshal(payload.Command)
	selector := firstNonempty(payload.ContainerID, payload.ContainerPattern, payload.Namespace, payload.Username)
	_, err := tx.ExecContext(ctx, `INSERT INTO authorization_scopes(node_id,authorization_id,session_id,mode,holder,selector,command_json,created_at_ms,expires_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(node_id,authorization_id) DO UPDATE SET session_id=excluded.session_id,mode=excluded.mode,
		holder=excluded.holder,selector=excluded.selector,command_json=excluded.command_json,expires_at_ms=excluded.expires_at_ms`,
		nodeID, payload.AuthorizationID, nullableString(sessionID), payload.Mode, payload.Holder, selector, string(command), millis(payload.CreatedAt), nullableTimeValue(payload.ExpiresAt))
	if err != nil {
		return err
	}
	for _, groupID := range groups {
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO authorization_sessions(node_id,authorization_id,session_id)
			SELECT ?,?,session_id FROM reservation_sessions WHERE node_id=? AND group_id=?`, nodeID, payload.AuthorizationID, nodeID, groupID)
		if err != nil {
			return err
		}
	}
	return nil
}

func applyJob(ctx context.Context, tx *sql.Tx, nodeID string, payload telemetry.JobEvent, occurredAt time.Time) error {
	var session string
	var expiresMS int64
	var revoked sql.NullInt64
	groups := telemetryGroups(payload.GroupID, payload.GroupIDs)
	var sessions []string
	for _, groupID := range groups {
		var candidate string
		var candidateExpires int64
		var candidateRevoked sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT session_id,expires_at_ms,revoked_at_ms FROM reservation_sessions WHERE node_id=? AND group_id=?", nodeID, groupID).Scan(&candidate, &candidateExpires, &candidateRevoked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		sessions = append(sessions, candidate)
		if session == "" {
			session, expiresMS, revoked = candidate, candidateExpires, candidateRevoked
		}
	}
	if session == "" {
		return nil
	}
	effectiveEnd := timeFromMillis(expiresMS)
	if revoked.Valid && timeFromMillis(revoked.Int64).Before(effectiveEnd) {
		effectiveEnd = timeFromMillis(revoked.Int64)
	}
	payload.RootExitedAt = clampTime(payload.RootExitedAt, effectiveEnd)
	payload.FinishedAt = clampTime(payload.FinishedAt, effectiveEnd)
	command, _ := json.Marshal(payload.Command)
	_, err := tx.ExecContext(ctx, `INSERT INTO jobs(node_id,job_id,session_id,authorization_id,source,mode,holder,command_json,started_at_ms,
		root_exited_at_ms,finished_at_ms,start_precision,finish_precision,exit_code,end_reason,updated_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(node_id,job_id) DO UPDATE SET command_json=CASE WHEN excluded.command_json IN ('[]','null') THEN jobs.command_json ELSE excluded.command_json END,
		started_at_ms=COALESCE(jobs.started_at_ms,excluded.started_at_ms),root_exited_at_ms=COALESCE(excluded.root_exited_at_ms,jobs.root_exited_at_ms),
		finished_at_ms=COALESCE(excluded.finished_at_ms,jobs.finished_at_ms),start_precision=CASE WHEN excluded.start_precision='' THEN jobs.start_precision ELSE excluded.start_precision END,
		finish_precision=CASE WHEN excluded.finish_precision='' THEN jobs.finish_precision ELSE excluded.finish_precision END,
		exit_code=COALESCE(excluded.exit_code,jobs.exit_code),end_reason=CASE WHEN excluded.end_reason='' THEN jobs.end_reason ELSE excluded.end_reason END,
		updated_at_ms=excluded.updated_at_ms`, nodeID, payload.ExecutionID, session, payload.AuthorizationID, payload.Source, payload.Mode,
		payload.Holder, string(command), nullableMillis(payload.StartedAt), nullableMillis(payload.RootExitedAt), nullableMillis(payload.FinishedAt),
		payload.StartPrecision, payload.FinishPrecision, nullableInt(payload.ExitCode), payload.Reason, millis(occurredAt))
	if err != nil {
		return err
	}
	for _, linkedSession := range sessions {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO job_sessions(node_id,job_id,session_id) VALUES(?,?,?)", nodeID, payload.ExecutionID, linkedSession); err != nil {
			return err
		}
		if payload.AuthorizationID != "" {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO authorization_sessions(node_id,authorization_id,session_id) VALUES(?,?,?)", nodeID, payload.AuthorizationID, linkedSession); err != nil {
				return err
			}
		}
	}
	for _, gpu := range payload.GPUs {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO job_gpus(node_id,job_id,gpu) VALUES(?,?,?)", nodeID, payload.ExecutionID, gpu); err != nil {
			return err
		}
	}
	return nil
}

func telemetryGroups(primary string, additional []string) []string {
	seen := make(map[string]bool)
	var groups []string
	for _, group := range append([]string{primary}, additional...) {
		if group != "" && !seen[group] {
			seen[group] = true
			groups = append(groups, group)
		}
	}
	return groups
}

func clampTime(value *time.Time, maximum time.Time) *time.Time {
	if value == nil || !value.After(maximum) {
		return value
	}
	clamped := maximum
	return &clamped
}

func applyGPUSample(ctx context.Context, tx *sql.Tx, nodeID string, payload telemetry.GPUSample) error {
	if !payload.WindowEnd.After(payload.WindowStart) {
		return nil
	}
	for _, gpu := range payload.GPUs {
		if gpu.GroupID == "" {
			continue
		}
		var session string
		var startsMS, expiresMS int64
		var revoked sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT session_id,starts_at_ms,expires_at_ms,revoked_at_ms FROM reservation_sessions
			WHERE node_id=? AND group_id=?`, nodeID, gpu.GroupID).Scan(&session, &startsMS, &expiresMS, &revoked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if payload.Status != "ok" {
			if _, err := tx.ExecContext(ctx, "UPDATE reservation_sessions SET history_quality='partial',updated_at_ms=? WHERE session_id=?", millis(payload.WindowEnd), session); err != nil {
				return err
			}
		}
		start := payload.WindowStart
		if sessionStart := timeFromMillis(startsMS); start.Before(sessionStart) {
			start = sessionStart
		}
		end := payload.WindowEnd
		effectiveEnd := timeFromMillis(expiresMS)
		if revoked.Valid && timeFromMillis(revoked.Int64).Before(effectiveEnd) {
			effectiveEnd = timeFromMillis(revoked.Int64)
		}
		if end.After(effectiveEnd) {
			end = effectiveEnd
		}
		if !end.After(start) {
			continue
		}
		if err := applyGPUIntervals(ctx, tx, session, gpu, start, end); err != nil {
			return err
		}
	}
	return nil
}

func applyGPUIntervals(ctx context.Context, tx *sql.Tx, session string, gpu telemetry.GPUSampleEntry, start, end time.Time) error {
	valid := gpu.UtilizationPct != nil && !math.IsNaN(*gpu.UtilizationPct) && *gpu.UtilizationPct >= 0 && *gpu.UtilizationPct <= 100
	for cursor := start; cursor.Before(end); {
		minute := cursor.Truncate(time.Minute)
		partEnd := minute.Add(time.Minute)
		if partEnd.After(end) {
			partEnd = end
		}
		duration := partEnd.Sub(cursor).Milliseconds()
		if duration <= 0 {
			break
		}
		memoryIntegral, memoryObserved, peak := memoryValues(gpu.MemoryUsedBytes, duration)
		if valid {
			busy := int64(0)
			if *gpu.UtilizationPct >= 5 {
				busy = duration
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO gpu_minute_rollups(session_id,gpu,minute_ms,observed_ms,busy_ms,utilization_integral,
				memory_integral,memory_observed_ms,peak_memory_bytes,valid_samples) VALUES(?,?,?,?,?,?,?,?,?,1)
				ON CONFLICT(session_id,gpu,minute_ms) DO UPDATE SET observed_ms=observed_ms+excluded.observed_ms,busy_ms=busy_ms+excluded.busy_ms,
				utilization_integral=utilization_integral+excluded.utilization_integral,memory_integral=memory_integral+excluded.memory_integral,
				memory_observed_ms=memory_observed_ms+excluded.memory_observed_ms,peak_memory_bytes=CASE WHEN excluded.peak_memory_bytes IS NULL THEN peak_memory_bytes
				WHEN peak_memory_bytes IS NULL THEN excluded.peak_memory_bytes ELSE MAX(peak_memory_bytes,excluded.peak_memory_bytes) END,
				valid_samples=valid_samples+1`, session, gpu.GPU, millis(minute), duration, busy, *gpu.UtilizationPct*float64(duration), memoryIntegral, memoryObserved, peak); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE session_gpu_summaries SET observed_ms=observed_ms+?,busy_ms=busy_ms+?,
				utilization_integral=utilization_integral+?,memory_integral=memory_integral+?,memory_observed_ms=memory_observed_ms+?,
				peak_memory_bytes=CASE WHEN ? IS NULL THEN peak_memory_bytes WHEN peak_memory_bytes IS NULL THEN ? ELSE MAX(peak_memory_bytes,?) END,
				valid_samples=valid_samples+1 WHERE session_id=? AND gpu=?`,
				duration, busy, *gpu.UtilizationPct*float64(duration), memoryIntegral, memoryObserved, peak, peak, peak, session, gpu.GPU); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO gpu_minute_rollups(session_id,gpu,minute_ms,memory_integral,memory_observed_ms,peak_memory_bytes,missing_samples)
				VALUES(?,?,?,?,?,?,1) ON CONFLICT(session_id,gpu,minute_ms) DO UPDATE SET
				memory_integral=memory_integral+excluded.memory_integral,memory_observed_ms=memory_observed_ms+excluded.memory_observed_ms,
				peak_memory_bytes=CASE WHEN excluded.peak_memory_bytes IS NULL THEN peak_memory_bytes WHEN peak_memory_bytes IS NULL THEN excluded.peak_memory_bytes
				ELSE MAX(peak_memory_bytes,excluded.peak_memory_bytes) END,missing_samples=missing_samples+1`,
				session, gpu.GPU, millis(minute), memoryIntegral, memoryObserved, peak); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE session_gpu_summaries SET memory_integral=memory_integral+?,memory_observed_ms=memory_observed_ms+?,
				peak_memory_bytes=CASE WHEN ? IS NULL THEN peak_memory_bytes WHEN peak_memory_bytes IS NULL THEN ? ELSE MAX(peak_memory_bytes,?) END,
				missing_samples=missing_samples+1 WHERE session_id=? AND gpu=?`,
				memoryIntegral, memoryObserved, peak, peak, peak, session, gpu.GPU); err != nil {
				return err
			}
		}
		cursor = partEnd
	}
	return nil
}

func memoryValues(value *uint64, duration int64) (float64, int64, any) {
	if value == nil {
		return 0, 0, nil
	}
	clamped := *value
	const maxSignedInt64 = uint64(1<<63 - 1)
	if clamped > maxSignedInt64 {
		clamped = maxSignedInt64
	}
	return float64(clamped) * float64(duration), duration, int64(clamped)
}

func nullableTimeValue(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return millis(value)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
