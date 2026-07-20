package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNotFound        = errors.New("history session not found")
	ErrForbidden       = errors.New("history result can only be edited by its owner")
	ErrVersionConflict = errors.New("history result version conflict")
)

func (s *Store) ListSessions(ctx context.Context, filter SessionFilter) ([]Session, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	now := time.Now().UTC().UnixMilli()
	query := `SELECT r.session_id,r.server_id,r.server_name,r.node_id,r.owner_username,r.owner_editable,r.purpose,r.source,r.created_at_ms,r.starts_at_ms,
		r.expires_at_ms,r.revoked_at_ms,r.finalized_at_ms,r.history_quality,
		COUNT(DISTINCT j.job_id),MIN(j.started_at_ms),MAX(COALESCE(j.finished_at_ms,j.started_at_ms))
		FROM reservation_sessions r LEFT JOIN jobs j ON j.session_id=r.session_id WHERE r.provisioning=0`
	args := []any{}
	if filter.ServerID != "" {
		query += " AND r.server_id=?"
		args = append(args, filter.ServerID)
	}
	if filter.Owner != "" {
		query += " AND r.owner_username=? COLLATE NOCASE"
		args = append(args, filter.Owner)
	}
	if filter.From != nil {
		query += " AND r.expires_at_ms>=?"
		args = append(args, millis(*filter.From))
	}
	if filter.To != nil {
		query += " AND r.starts_at_ms<?"
		args = append(args, millis(*filter.To))
	}
	if filter.BeforeMS > 0 {
		query += " AND (r.starts_at_ms<? OR (r.starts_at_ms=? AND r.session_id<?))"
		args = append(args, filter.BeforeMS, filter.BeforeMS, filter.BeforeID)
	}
	switch filter.Status {
	case "scheduled":
		query += " AND r.revoked_at_ms IS NULL AND r.starts_at_ms>?"
		args = append(args, now)
	case "active":
		query += " AND r.revoked_at_ms IS NULL AND r.starts_at_ms<=? AND r.expires_at_ms>?"
		args = append(args, now, now)
	case "completed":
		query += " AND r.revoked_at_ms IS NULL AND r.expires_at_ms<=?"
		args = append(args, now)
	case "revoked":
		query += " AND r.revoked_at_ms IS NOT NULL"
	}
	query += ` GROUP BY r.session_id ORDER BY r.starts_at_ms DESC,r.session_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for rows.Next() {
		item, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		sessions = append(sessions, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range sessions {
		item := &sessions[index]
		item.GPUs, err = s.sessionGPUs(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		item.GPUSummaries, err = s.gpuSummaries(ctx, *item)
		if err != nil {
			return nil, err
		}
		item.Result, err = s.result(ctx, item.ID)
		if err != nil {
			return nil, err
		}
	}
	return sessions, nil
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT r.session_id,r.server_id,r.server_name,r.node_id,r.owner_username,r.owner_editable,r.purpose,r.source,r.created_at_ms,r.starts_at_ms,
		r.expires_at_ms,r.revoked_at_ms,r.finalized_at_ms,r.history_quality,
		COUNT(DISTINCT j.job_id),MIN(j.started_at_ms),MAX(COALESCE(j.finished_at_ms,j.started_at_ms))
		FROM reservation_sessions r LEFT JOIN jobs j ON j.session_id=r.session_id WHERE r.session_id=? AND r.provisioning=0 GROUP BY r.session_id`, id)
	item, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	item.GPUs, err = s.sessionGPUs(ctx, id)
	if err != nil {
		return Session{}, err
	}
	item.GPUSummaries, err = s.gpuSummaries(ctx, item)
	if err != nil {
		return Session{}, err
	}
	item.Timeline, err = s.timeline(ctx, id)
	if err != nil {
		return Session{}, err
	}
	item.Authorizations, err = s.authorizationScopes(ctx, id)
	if err != nil {
		return Session{}, err
	}
	item.Result, err = s.result(ctx, id)
	return item, err
}

func (s *Store) ListJobs(ctx context.Context, sessionID string, limit int, afterMS int64, afterID string) ([]Job, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	query := `SELECT node_id,job_id,source,mode,holder,command_json,started_at_ms,root_exited_at_ms,finished_at_ms,
		start_precision,finish_precision,exit_code,end_reason FROM jobs WHERE session_id=?`
	args := []any{sessionID}
	if afterMS > 0 {
		query += " AND (COALESCE(started_at_ms,0)>? OR (COALESCE(started_at_ms,0)=? AND job_id>?))"
		args = append(args, afterMS, afterMS, afterID)
	}
	query += " ORDER BY COALESCE(started_at_ms,0),job_id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	type pendingJob struct {
		job        Job
		nodeID     string
		internalID string
	}
	var pending []pendingJob
	for rows.Next() {
		var nodeID, commandJSON string
		var internalID string
		var started, rootExited, finished sql.NullInt64
		var exitCode sql.NullInt64
		var job Job
		if err := rows.Scan(&nodeID, &internalID, &job.Source, &job.Mode, &job.Holder, &commandJSON, &started, &rootExited, &finished,
			&job.StartPrecision, &job.FinishPrecision, &exitCode, &job.Reason); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(commandJSON), &job.Command)
		job.ID = publicJobID(nodeID, internalID)
		job.CursorID = internalID
		job.StartedAt = timePtrFromNull(started)
		job.RootExitedAt = timePtrFromNull(rootExited)
		job.FinishedAt = timePtrFromNull(finished)
		if exitCode.Valid {
			value := int(exitCode.Int64)
			job.ExitCode = &value
		}
		pending = append(pending, pendingJob{job: job, nodeID: nodeID, internalID: internalID})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(pending))
	for _, value := range pending {
		job := value.job
		gpuRows, err := s.db.QueryContext(ctx, "SELECT gpu FROM job_gpus WHERE node_id=? AND job_id=? ORDER BY gpu", value.nodeID, value.internalID)
		if err != nil {
			return nil, err
		}
		for gpuRows.Next() {
			var gpu int
			if err := gpuRows.Scan(&gpu); err != nil {
				gpuRows.Close()
				return nil, err
			}
			job.GPUs = append(job.GPUs, gpu)
		}
		if err := gpuRows.Err(); err != nil {
			gpuRows.Close()
			return nil, err
		}
		if err := gpuRows.Close(); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *Store) Summary(ctx context.Context, filter SessionFilter) (DashboardSummary, error) {
	where := " WHERE r.provisioning=0"
	args := []any{}
	if filter.ServerID != "" {
		where += " AND r.server_id=?"
		args = append(args, filter.ServerID)
	}
	if filter.Owner != "" {
		where += " AND r.owner_username=? COLLATE NOCASE"
		args = append(args, filter.Owner)
	}
	if filter.From != nil {
		where += " AND r.expires_at_ms>=?"
		args = append(args, millis(*filter.From))
	}
	if filter.To != nil {
		where += " AND r.starts_at_ms<?"
		args = append(args, millis(*filter.To))
	}
	var out DashboardSummary
	var reservedMS sql.NullFloat64
	query := `SELECT COUNT(DISTINCT r.session_id),SUM(MAX(0,MIN(r.expires_at_ms,COALESCE(r.revoked_at_ms,r.expires_at_ms))-r.starts_at_ms))
		FROM reservation_sessions r JOIN session_gpus g ON g.session_id=r.session_id` + where
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&out.Sessions, &reservedMS); err != nil {
		return out, err
	}
	var observed, busy int64
	var integral sql.NullFloat64
	metricQuery := `SELECT COALESCE(SUM(s.observed_ms),0),COALESCE(SUM(s.busy_ms),0),SUM(s.utilization_integral)
		FROM session_gpu_summaries s JOIN reservation_sessions r ON r.session_id=s.session_id` + where
	if err := s.db.QueryRowContext(ctx, metricQuery, args...).Scan(&observed, &busy, &integral); err != nil {
		return out, err
	}
	jobQuery := `SELECT COUNT(*) FROM jobs j JOIN reservation_sessions r ON r.session_id=j.session_id` + where
	if err := s.db.QueryRowContext(ctx, jobQuery, args...).Scan(&out.Jobs); err != nil {
		return out, err
	}
	if reservedMS.Valid && reservedMS.Float64 > 0 {
		out.ReservedGPUHours = reservedMS.Float64 / float64(time.Hour/time.Millisecond)
		out.TelemetryCoverage = float64(observed) / reservedMS.Float64
	}
	out.BusyGPUHours = float64(busy) / float64(time.Hour/time.Millisecond)
	if observed > 0 {
		out.BusyRatio = float64(busy) / float64(observed)
		value := integral.Float64 / float64(observed)
		out.AverageUtilization = &value
	}
	return out, nil
}

func (s *Store) PutResult(ctx context.Context, sessionID, user, outcome, note string, artifacts []Artifact, expectedVersion int) (Result, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	var owner string
	var ownerEditable bool
	if err := tx.QueryRowContext(ctx, "SELECT owner_username,owner_editable FROM reservation_sessions WHERE session_id=? AND provisioning=0", sessionID).Scan(&owner, &ownerEditable); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, ErrNotFound
		}
		return Result{}, err
	}
	if !ownerEditable || !strings.EqualFold(strings.TrimSpace(owner), strings.TrimSpace(user)) {
		return Result{}, ErrForbidden
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE session_results SET outcome=?,note=?,version=version+1,updated_at_ms=?
		WHERE session_id=? AND version=?`, nullableString(outcome), note, millis(now), sessionID, expectedVersion)
	if err != nil {
		return Result{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return Result{}, ErrVersionConflict
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM session_artifacts WHERE session_id=?", sessionID); err != nil {
		return Result{}, err
	}
	for index, artifact := range artifacts {
		if _, err := tx.ExecContext(ctx, "INSERT INTO session_artifacts(session_id,position,label,url) VALUES(?,?,?,?)", sessionID, index, artifact.Label, artifact.URL); err != nil {
			return Result{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return s.result(ctx, sessionID)
}

func (s *Store) PrepareSession(ctx context.Context, id, nodeID, serverID, serverName, owner, purpose string, startsAt, expiresAt time.Time, gpus []int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO nodes(node_id,last_server_id,first_seen_at_ms,last_seen_at_ms) VALUES(?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET last_server_id=excluded.last_server_id,last_seen_at_ms=excluded.last_seen_at_ms`, nodeID, serverID, millis(now), millis(now)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO reservation_sessions(session_id,node_id,server_id,server_name,group_id,owner_username,owner_editable,purpose,source,
		created_at_ms,starts_at_ms,expires_at_ms,history_quality,provisioning,updated_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,1,?)`, id, nodeID, serverID, serverName, "pending:"+id, owner, true, purpose, "web", millis(now), millis(startsAt), millis(expiresAt), "complete", millis(now))
	if err != nil {
		return err
	}
	for _, gpu := range gpus {
		if _, err := tx.ExecContext(ctx, "INSERT INTO session_gpus(session_id,gpu,reservation_id) VALUES(?,?,?)", id, gpu, fmt.Sprintf("pending-%d", gpu)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO session_results(session_id) VALUES(?)", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConfirmSession(ctx context.Context, id, groupID string, reservationIDs []string, gpus []int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE reservation_sessions SET group_id=?,provisioning=0,updated_at_ms=? WHERE session_id=? AND provisioning=1", groupID, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	for index, gpu := range gpus {
		reservationID := fmt.Sprintf("reservation-%d", gpu)
		if index < len(reservationIDs) {
			reservationID = reservationIDs[index]
		}
		if _, err := tx.ExecContext(ctx, "UPDATE session_gpus SET reservation_id=? WHERE session_id=? AND gpu=?", reservationID, id, gpu); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO session_gpu_summaries(session_id,gpu) VALUES(?,?)", id, gpu); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DropProvisioningSession(ctx context.Context, id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, "DELETE FROM reservation_sessions WHERE session_id=? AND provisioning=1", id)
	return err
}

func scanSession(scanner interface{ Scan(...any) error }) (Session, error) {
	var item Session
	var ownerEditable bool
	var created, starts, expires int64
	var revoked, finalized, firstJob, lastJob sql.NullInt64
	if err := scanner.Scan(&item.ID, &item.ServerID, &item.ServerName, &item.NodeID, &item.Owner, &ownerEditable, &item.Purpose, &item.Source,
		&created, &starts, &expires, &revoked, &finalized, &item.HistoryQuality, &item.JobCount, &firstJob, &lastJob); err != nil {
		return Session{}, err
	}
	item.CreatedAt = timeFromMillis(created)
	item.ResultEditable = ownerEditable
	item.StartsAt = timeFromMillis(starts)
	item.ExpiresAt = timeFromMillis(expires)
	item.RevokedAt = timePtrFromNull(revoked)
	item.FinalizedAt = timePtrFromNull(finalized)
	item.FirstJobAt = timePtrFromNull(firstJob)
	item.LastJobAt = timePtrFromNull(lastJob)
	item.Status = sessionStatus(item, time.Now().UTC())
	return item, nil
}

func sessionStatus(item Session, now time.Time) string {
	if item.RevokedAt != nil {
		return "revoked"
	}
	if now.Before(item.StartsAt) {
		return "scheduled"
	}
	if now.Before(item.ExpiresAt) {
		return "active"
	}
	return "completed"
}

func (s *Store) sessionGPUs(ctx context.Context, id string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT gpu FROM session_gpus WHERE session_id=? ORDER BY gpu", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []int
	for rows.Next() {
		var gpu int
		if err := rows.Scan(&gpu); err != nil {
			return nil, err
		}
		values = append(values, gpu)
	}
	return values, rows.Err()
}

func (s *Store) gpuSummaries(ctx context.Context, session Session) ([]GPUSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT gpu,observed_ms,busy_ms,utilization_integral,memory_integral,memory_observed_ms,
		peak_memory_bytes,valid_samples,missing_samples FROM session_gpu_summaries WHERE session_id=? ORDER BY gpu`, session.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	effectiveEnd := session.ExpiresAt
	if session.RevokedAt != nil && session.RevokedAt.Before(effectiveEnd) {
		effectiveEnd = *session.RevokedAt
	}
	reserved := max(int64(0), effectiveEnd.Sub(session.StartsAt).Milliseconds())
	var values []GPUSummary
	for rows.Next() {
		var item GPUSummary
		var utilizationIntegral, memoryIntegral float64
		var memoryObserved int64
		var peak sql.NullInt64
		if err := rows.Scan(&item.GPU, &item.ObservedMS, &item.BusyMS, &utilizationIntegral, &memoryIntegral, &memoryObserved,
			&peak, &item.ValidSamples, &item.MissingSamples); err != nil {
			return nil, err
		}
		if item.ObservedMS > 0 {
			value := utilizationIntegral / float64(item.ObservedMS)
			item.AverageUtilization = &value
		}
		if reserved > 0 {
			item.Coverage = float64(item.ObservedMS) / float64(reserved)
		}
		if memoryObserved > 0 {
			value := memoryIntegral / float64(memoryObserved)
			item.AverageMemoryBytes = &value
		}
		if peak.Valid {
			value := uint64(peak.Int64)
			item.PeakMemoryBytes = &value
		}
		values = append(values, item)
	}
	return values, rows.Err()
}

func (s *Store) timeline(ctx context.Context, id string) ([]MinuteRollup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT gpu,minute_ms,observed_ms,busy_ms,utilization_integral
		FROM gpu_minute_rollups WHERE session_id=? ORDER BY minute_ms,gpu`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []MinuteRollup
	for rows.Next() {
		var item MinuteRollup
		var minute int64
		var integral float64
		if err := rows.Scan(&item.GPU, &minute, &item.ObservedMS, &item.BusyMS, &integral); err != nil {
			return nil, err
		}
		item.Minute = timeFromMillis(minute)
		if item.ObservedMS > 0 {
			value := integral / float64(item.ObservedMS)
			item.AverageUtilization = &value
		}
		values = append(values, item)
	}
	return values, rows.Err()
}

func (s *Store) authorizationScopes(ctx context.Context, id string) ([]AuthorizationScope, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT mode,holder,selector,command_json,created_at_ms,expires_at_ms,ended_at_ms,end_reason
		FROM authorization_scopes WHERE session_id=? ORDER BY created_at_ms`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []AuthorizationScope
	for rows.Next() {
		var item AuthorizationScope
		var commandJSON string
		var created int64
		var expires, ended sql.NullInt64
		if err := rows.Scan(&item.Mode, &item.Holder, &item.Selector, &commandJSON, &created, &expires, &ended, &item.EndReason); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(commandJSON), &item.Command)
		item.CreatedAt = timeFromMillis(created)
		item.ExpiresAt = timePtrFromNull(expires)
		item.EndedAt = timePtrFromNull(ended)
		values = append(values, item)
	}
	return values, rows.Err()
}

func (s *Store) result(ctx context.Context, id string) (Result, error) {
	var out Result
	var outcome sql.NullString
	var updated sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT outcome,note,version,updated_at_ms FROM session_results WHERE session_id=?", id).Scan(&outcome, &out.Note, &out.Version, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if outcome.Valid {
		out.Outcome = outcome.String
	}
	out.UpdatedAt = timePtrFromNull(updated)
	rows, err := s.db.QueryContext(ctx, "SELECT label,url FROM session_artifacts WHERE session_id=? ORDER BY position", id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var artifact Artifact
		if err := rows.Scan(&artifact.Label, &artifact.URL); err != nil {
			return out, err
		}
		out.Artifacts = append(out.Artifacts, artifact)
	}
	return out, rows.Err()
}

func validateOutcome(value string) bool {
	switch value {
	case "", OutcomeSuccess, OutcomePartial, OutcomeFailed, OutcomeAborted:
		return true
	default:
		return false
	}
}

func ValidateResult(outcome, note string, artifacts []Artifact) error {
	if !validateOutcome(outcome) {
		return errors.New("outcome must be success, partial, failed, aborted, or empty")
	}
	if len(note) > 16<<10 {
		return errors.New("note must be at most 16384 bytes")
	}
	if len(artifacts) > 20 {
		return errors.New("at most 20 artifacts are allowed")
	}
	for _, artifact := range artifacts {
		if len(artifact.Label) > 128 || len(artifact.URL) > 2048 {
			return errors.New("artifact label or URL is too long")
		}
		if !strings.HasPrefix(artifact.URL, "https://") && !strings.HasPrefix(artifact.URL, "http://") {
			return errors.New("artifact URL must use http or https")
		}
	}
	if strings.ContainsRune(note, 0) {
		return fmt.Errorf("note contains NUL")
	}
	return nil
}
