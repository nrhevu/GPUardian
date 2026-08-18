package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const dailySummaryWindow = 30 * 24 * time.Hour

// DailySummary returns the persisted 30-day summary for the UTC day containing
// at. A missing cache is computed once; normal operation prewarms it from the
// history collector before users open the dashboard.
func (s *Store) DailySummary(ctx context.Context, serverID string, at time.Time) (DashboardSummary, error) {
	day := utcDayStart(at)
	summary, err := s.cachedDailySummary(ctx, serverID, day)
	if err == nil {
		return summary, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DashboardSummary{}, err
	}
	return s.RefreshDailySummary(ctx, serverID, at)
}

// RefreshDailySummary creates at most one cache entry per server and UTC day.
func (s *Store) RefreshDailySummary(ctx context.Context, serverID string, at time.Time) (DashboardSummary, error) {
	s.summaryMu.Lock()
	defer s.summaryMu.Unlock()

	at = at.UTC()
	day := utcDayStart(at)
	if summary, err := s.cachedDailySummary(ctx, serverID, day); err == nil {
		return summary, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return DashboardSummary{}, err
	}

	windowStart := at.Add(-dailySummaryWindow).UTC()
	windowEnd := at.UTC()
	summary, err := s.computeDailySummary(ctx, serverID, windowStart, windowEnd)
	if err != nil {
		return DashboardSummary{}, err
	}
	updatedAt := at.UTC()
	summary.WindowStart = &windowStart
	summary.WindowEnd = &windowEnd
	summary.UpdatedAt = &updatedAt
	encoded, err := json.Marshal(summary)
	if err != nil {
		return DashboardSummary{}, err
	}

	s.writeMu.Lock()
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO history_daily_summaries(
		server_id,day_ms,window_start_ms,window_end_ms,summary_json,computed_at_ms
	) VALUES(?,?,?,?,?,?)`, serverID, millis(day), millis(windowStart), millis(windowEnd), string(encoded), millis(updatedAt))
	if err == nil {
		_, err = s.db.ExecContext(ctx, "DELETE FROM history_daily_summaries WHERE day_ms<?", millis(day.Add(-7*24*time.Hour)))
	}
	s.writeMu.Unlock()
	if err != nil {
		return DashboardSummary{}, err
	}
	return s.cachedDailySummary(ctx, serverID, day)
}

func (s *Store) cachedDailySummary(ctx context.Context, serverID string, day time.Time) (DashboardSummary, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT summary_json FROM history_daily_summaries
		WHERE server_id=? AND day_ms=?`, serverID, millis(day)).Scan(&encoded)
	if err != nil {
		return DashboardSummary{}, err
	}
	var summary DashboardSummary
	if err := json.Unmarshal([]byte(encoded), &summary); err != nil {
		return DashboardSummary{}, err
	}
	return summary, nil
}

func utcDayStart(value time.Time) time.Time {
	utc := value.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *Store) computeDailySummary(ctx context.Context, serverID string, from, to time.Time) (DashboardSummary, error) {
	baseWhere := " WHERE r.provisioning=0"
	var baseArgs []any
	if serverID != "" {
		baseWhere += " AND r.server_id=?"
		baseArgs = append(baseArgs, serverID)
	}
	cte := `WITH base AS (
		SELECT r.session_id,r.kind,r.starts_at_ms,
			CASE WHEN r.kind='claimed_run' THEN COALESCE(r.finalized_at_ms,r.updated_at_ms)
				ELSE MIN(r.expires_at_ms,COALESCE(r.revoked_at_ms,r.expires_at_ms)) END AS effective_end_ms,
			(SELECT COUNT(*) FROM session_gpus g WHERE g.session_id=r.session_id) AS gpu_count
		FROM reservation_sessions r` + baseWhere + `
	), matched AS (
		SELECT *,MAX(0,MIN(effective_end_ms,?)-MAX(starts_at_ms,?)) AS duration_ms
		FROM base WHERE effective_end_ms>=? AND starts_at_ms<?
	)`
	cteArgs := append(append([]any{}, baseArgs...), millis(to), millis(from), millis(from), millis(to))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DashboardSummary{}, err
	}
	defer tx.Rollback()

	var summary DashboardSummary
	var reserved int64
	if err := tx.QueryRowContext(ctx, cte+` SELECT COUNT(*),
		COUNT(CASE WHEN kind='reservation' THEN 1 END),COUNT(CASE WHEN kind='claimed_run' THEN 1 END),
		COALESCE(SUM(CASE WHEN kind='reservation' THEN duration_ms*gpu_count ELSE 0 END),0) FROM matched`, cteArgs...).
		Scan(&summary.Sessions, &summary.Reservations, &summary.ClaimedRuns, &reserved); err != nil {
		return DashboardSummary{}, err
	}
	var reservationObserved int64
	metricArgs := append(append([]any{}, cteArgs...), millis(from), millis(to))
	if err := tx.QueryRowContext(ctx, cte+` SELECT COALESCE(SUM(g.observed_ms),0)
		FROM gpu_minute_rollups g JOIN matched m ON m.session_id=g.session_id
		WHERE m.kind='reservation' AND g.minute_ms>=? AND g.minute_ms<?`, metricArgs...).Scan(&reservationObserved); err != nil {
		return DashboardSummary{}, err
	}
	jobArgs := append(append([]any{}, cteArgs...), millis(from), millis(to))
	if err := tx.QueryRowContext(ctx, cte+`, session_jobs AS (
		SELECT m.session_id,j.node_id,j.job_id,j.started_at_ms,j.finished_at_ms,j.updated_at_ms
		FROM matched m JOIN jobs j ON j.session_id=m.session_id
		UNION
		SELECT m.session_id,j.node_id,j.job_id,j.started_at_ms,j.finished_at_ms,j.updated_at_ms
		FROM matched m JOIN job_sessions js ON js.session_id=m.session_id
		JOIN jobs j ON j.node_id=js.node_id AND j.job_id=js.job_id
	) SELECT COUNT(DISTINCT node_id||':'||job_id) FROM session_jobs
		WHERE COALESCE(finished_at_ms,started_at_ms,updated_at_ms)>=?
		AND COALESCE(started_at_ms,updated_at_ms)<?`, jobArgs...).Scan(&summary.Jobs); err != nil {
		return DashboardSummary{}, err
	}
	observed, busy, integral, _, err := nodeWideGPUMetricsBetween(ctx, tx, serverID, from, to)
	if err != nil {
		return DashboardSummary{}, err
	}
	if reserved > 0 {
		summary.ReservedGPUHours = float64(reserved) / float64(time.Hour/time.Millisecond)
		summary.TelemetryCoverage = float64(reservationObserved) / float64(reserved)
	}
	summary.BusyGPUHours = float64(busy) / float64(time.Hour/time.Millisecond)
	if observed > 0 {
		summary.BusyRatio = float64(busy) / float64(observed)
		value := integral.Float64 / float64(observed)
		summary.AverageUtilization = &value
	}
	if err := tx.Commit(); err != nil {
		return DashboardSummary{}, err
	}
	return summary, nil
}
