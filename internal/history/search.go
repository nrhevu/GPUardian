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

const (
	maxSearchGroups      = 8
	maxSearchRulesGroup  = 8
	maxSearchRules       = 32
	maxSearchStringBytes = 1024
)

var ErrInvalidSearchFilter = errors.New("invalid history filter")

const sessionFactsCTE = `WITH session_facts AS (
	SELECT r.*,
		MIN(r.expires_at_ms,COALESCE(r.revoked_at_ms,r.expires_at_ms)) AS effective_end_ms,
		MAX(0,MIN(r.expires_at_ms,COALESCE(r.revoked_at_ms,r.expires_at_ms))-r.starts_at_ms) AS duration_ms,
		CASE WHEN r.revoked_at_ms IS NOT NULL THEN 'revoked'
			WHEN r.starts_at_ms>? THEN 'scheduled'
			WHEN r.expires_at_ms>? THEN 'active' ELSE 'completed' END AS derived_status,
		(SELECT COUNT(*) FROM session_gpus g WHERE g.session_id=r.session_id) AS gpu_count,
		(SELECT COUNT(*) FROM jobs j WHERE j.session_id=r.session_id) AS job_count,
		(SELECT MIN(j.started_at_ms) FROM jobs j WHERE j.session_id=r.session_id) AS first_job_at_ms,
		(SELECT MAX(COALESCE(j.finished_at_ms,j.started_at_ms)) FROM jobs j WHERE j.session_id=r.session_id) AS last_job_at_ms,
		COALESCE((SELECT SUM(s.observed_ms) FROM session_gpu_summaries s WHERE s.session_id=r.session_id),0) AS observed_ms,
		COALESCE((SELECT SUM(s.busy_ms) FROM session_gpu_summaries s WHERE s.session_id=r.session_id),0) AS busy_ms,
		(SELECT SUM(s.utilization_integral) FROM session_gpu_summaries s WHERE s.session_id=r.session_id) AS utilization_integral,
		(SELECT SUM(s.memory_integral) FROM session_gpu_summaries s WHERE s.session_id=r.session_id) AS memory_integral,
		COALESCE((SELECT SUM(s.memory_observed_ms) FROM session_gpu_summaries s WHERE s.session_id=r.session_id),0) AS memory_observed_ms,
		(SELECT MAX(s.peak_memory_bytes) FROM session_gpu_summaries s WHERE s.session_id=r.session_id) AS peak_memory_bytes,
		(SELECT sr.outcome FROM session_results sr WHERE sr.session_id=r.session_id) AS result_outcome
	FROM reservation_sessions r WHERE r.provisioning=0
)`

// Search returns a summary and one keyset-paginated page from the same SQLite
// read transaction. Session detail enrichment happens after the selected IDs
// are fixed so the transaction does not hold a read lock across N+1 queries.
func (s *Store) Search(ctx context.Context, expression SearchExpression, limit int, beforeMS int64, beforeID string) (DashboardSummary, []Session, error) {
	predicate, predicateArgs, err := compileSearchExpression(expression)
	if err != nil {
		return DashboardSummary{}, nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	now := time.Now().UTC().UnixMilli()
	cteArgs := []any{now, now}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DashboardSummary{}, nil, err
	}
	defer tx.Rollback()

	var summary DashboardSummary
	var reserved, observed, busy int64
	var integral sql.NullFloat64
	summaryQuery := sessionFactsCTE + ` SELECT COUNT(*),
		COALESCE(SUM(duration_ms*gpu_count),0),COALESCE(SUM(observed_ms),0),COALESCE(SUM(busy_ms),0),
		SUM(utilization_integral),COALESCE(SUM(job_count),0) FROM session_facts f WHERE ` + predicate
	summaryArgs := append(append([]any{}, cteArgs...), predicateArgs...)
	if err := tx.QueryRowContext(ctx, summaryQuery, summaryArgs...).Scan(&summary.Sessions, &reserved, &observed, &busy, &integral, &summary.Jobs); err != nil {
		return DashboardSummary{}, nil, err
	}
	if reserved > 0 {
		summary.ReservedGPUHours = float64(reserved) / float64(time.Hour/time.Millisecond)
		summary.TelemetryCoverage = float64(observed) / float64(reserved)
	}
	summary.BusyGPUHours = float64(busy) / float64(time.Hour/time.Millisecond)
	if observed > 0 {
		summary.BusyRatio = float64(busy) / float64(observed)
		value := integral.Float64 / float64(observed)
		summary.AverageUtilization = &value
	}

	pagePredicate := predicate
	pageArgs := append([]any{}, predicateArgs...)
	if beforeMS > 0 {
		pagePredicate += " AND (f.starts_at_ms<? OR (f.starts_at_ms=? AND f.session_id<?))"
		pageArgs = append(pageArgs, beforeMS, beforeMS, beforeID)
	}
	listQuery := sessionFactsCTE + ` SELECT f.session_id,f.server_id,f.server_name,f.node_id,f.owner_username,f.owner_editable,f.purpose,f.source,
		f.created_at_ms,f.starts_at_ms,f.expires_at_ms,f.revoked_at_ms,f.finalized_at_ms,f.history_quality,
		f.job_count,f.first_job_at_ms,f.last_job_at_ms FROM session_facts f WHERE ` + pagePredicate +
		" ORDER BY f.starts_at_ms DESC,f.session_id DESC LIMIT ?"
	listArgs := append(append([]any{}, cteArgs...), pageArgs...)
	listArgs = append(listArgs, limit)
	rows, err := tx.QueryContext(ctx, listQuery, listArgs...)
	if err != nil {
		return DashboardSummary{}, nil, err
	}
	var sessions []Session
	for rows.Next() {
		item, scanErr := scanSession(rows)
		if scanErr != nil {
			rows.Close()
			return DashboardSummary{}, nil, scanErr
		}
		sessions = append(sessions, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DashboardSummary{}, nil, err
	}
	if err := rows.Close(); err != nil {
		return DashboardSummary{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return DashboardSummary{}, nil, err
	}
	if err := s.enrichSessions(ctx, sessions); err != nil {
		return DashboardSummary{}, nil, err
	}
	return summary, sessions, nil
}

func compileSearchExpression(expression SearchExpression) (string, []any, error) {
	if len(expression.Groups) > maxSearchGroups {
		return "", nil, invalidFilter("at most %d groups are allowed", maxSearchGroups)
	}
	var groups []string
	var args []any
	total := 0
	for groupIndex, group := range expression.Groups {
		if len(group.Rules) == 0 {
			return "", nil, invalidFilter("group %d must contain a rule", groupIndex+1)
		}
		if len(group.Rules) > maxSearchRulesGroup {
			return "", nil, invalidFilter("group %d exceeds %d rules", groupIndex+1, maxSearchRulesGroup)
		}
		total += len(group.Rules)
		if total > maxSearchRules {
			return "", nil, invalidFilter("at most %d rules are allowed", maxSearchRules)
		}
		var rules []string
		for ruleIndex, rule := range group.Rules {
			sqlText, values, err := compileSearchRule(rule)
			if err != nil {
				return "", nil, invalidFilter("group %d rule %d: %v", groupIndex+1, ruleIndex+1, err)
			}
			rules = append(rules, sqlText)
			args = append(args, values...)
		}
		groups = append(groups, "("+strings.Join(rules, " OR ")+")")
	}
	if len(groups) == 0 {
		return "1=1", nil, nil
	}
	return strings.Join(groups, " AND "), args, nil
}

func compileSearchRule(rule SearchRule) (string, []any, error) {
	field := strings.TrimSpace(rule.Field)
	op := strings.TrimSpace(rule.Operator)
	if field == "session_window" {
		values, err := timestampRange(rule.Value)
		if err != nil || op != "overlaps" {
			return "", nil, errors.New("session_window requires overlaps with two RFC3339 values")
		}
		return "(f.effective_end_ms>=? AND f.starts_at_ms<?)", []any{values[0], values[1]}, nil
	}
	if field == "gpu" {
		if op == "is_empty" {
			return "NOT EXISTS (SELECT 1 FROM session_gpus g WHERE g.session_id=f.session_id)", nil, nil
		}
		if op == "is_not_empty" {
			return "EXISTS (SELECT 1 FROM session_gpus g WHERE g.session_id=f.session_id)", nil, nil
		}
		return compileExistsNumberRule("SELECT 1 FROM session_gpus g WHERE g.session_id=f.session_id AND ", "g.gpu", op, rule.Value)
	}
	if strings.HasPrefix(field, "job.") {
		return compileJobRule(strings.TrimPrefix(field, "job."), op, rule.Value)
	}
	if column, ok := searchTextFields[field]; ok {
		return compileTextRule(column, op, rule.Value)
	}
	if column, ok := searchNumberFields[field]; ok {
		return compileNumberRule(column, op, rule.Value)
	}
	if column, ok := searchTimeFields[field]; ok {
		return compileTimeRule(column, op, rule.Value)
	}
	return "", nil, fmt.Errorf("unsupported field %q", field)
}

var searchTextFields = map[string]string{
	"purpose": "f.purpose", "owner": "f.owner_username", "node": "f.server_id", "source": "f.source",
	"status": "f.derived_status", "history_quality": "f.history_quality", "result_outcome": "f.result_outcome",
}

var searchNumberFields = map[string]string{
	"duration_ms": "f.duration_ms", "gpu_count": "f.gpu_count", "reserved_ms": "(f.duration_ms*f.gpu_count)",
	"busy_ms": "f.busy_ms", "average_utilization_percent": "(f.utilization_integral/NULLIF(f.observed_ms,0))",
	"busy_ratio": "(1.0*f.busy_ms/NULLIF(f.observed_ms,0))", "coverage": "(1.0*f.observed_ms/NULLIF(f.duration_ms*f.gpu_count,0))",
	"average_vram_bytes": "(f.memory_integral/NULLIF(f.memory_observed_ms,0))", "peak_vram_bytes": "f.peak_memory_bytes",
	"job_count": "f.job_count",
}

var searchTimeFields = map[string]string{
	"created_at": "f.created_at_ms", "starts_at": "f.starts_at_ms", "effective_end": "f.effective_end_ms",
}

func compileJobRule(field, op string, raw json.RawMessage) (string, []any, error) {
	textColumns := map[string]string{"source": "j.source", "mode": "j.mode", "holder": "j.holder", "end_reason": "j.end_reason", "start_precision": "j.start_precision", "finish_precision": "j.finish_precision"}
	numberColumns := map[string]string{"exit_code": "j.exit_code"}
	timeColumns := map[string]string{"started_at": "j.started_at_ms", "finished_at": "j.finished_at_ms"}
	prefix := "SELECT 1 FROM jobs j WHERE j.session_id=f.session_id AND "
	if column, ok := textColumns[field]; ok {
		return compileExistsTextRule(prefix, column, op, raw)
	}
	if column, ok := numberColumns[field]; ok {
		return compileExistsNumberRule(prefix, column, op, raw)
	}
	if column, ok := timeColumns[field]; ok {
		return compileExistsTimeRule(prefix, column, op, raw)
	}
	if field == "gpu" {
		if op == "is_empty" {
			return `EXISTS (SELECT 1 FROM jobs j WHERE j.session_id=f.session_id
				AND NOT EXISTS (SELECT 1 FROM job_gpus jg WHERE jg.node_id=j.node_id AND jg.job_id=j.job_id))`, nil, nil
		}
		if op == "is_not_empty" {
			return "EXISTS (SELECT 1 FROM jobs j JOIN job_gpus jg ON jg.node_id=j.node_id AND jg.job_id=j.job_id WHERE j.session_id=f.session_id)", nil, nil
		}
		return compileExistsNumberRule("SELECT 1 FROM jobs j JOIN job_gpus jg ON jg.node_id=j.node_id AND jg.job_id=j.job_id WHERE j.session_id=f.session_id AND ", "jg.gpu", op, raw)
	}
	if field == "command" {
		if op == "is_empty" || op == "is_not_empty" {
			compare := "COALESCE(json_array_length(j.command_json),0)=0"
			if op == "is_not_empty" {
				compare = "COALESCE(json_array_length(j.command_json),0)>0"
			}
			return "EXISTS (SELECT 1 FROM jobs j WHERE j.session_id=f.session_id AND " + compare + ")", nil, nil
		}
		negative := op == "not_contains" || op == "not_equals"
		positiveOp := map[string]string{"not_contains": "contains", "not_equals": "equals"}[op]
		if !negative {
			positiveOp = op
		}
		if positiveOp != "contains" && positiveOp != "equals" {
			return "", nil, errors.New("job.command supports equals, not_equals, contains, or not_contains")
		}
		value, err := stringValue(raw)
		if err != nil {
			return "", nil, err
		}
		compare := "LOWER(CAST(a.value AS TEXT))=LOWER(?)"
		if positiveOp == "contains" {
			compare = "INSTR(LOWER(CAST(a.value AS TEXT)),LOWER(?))>0"
		}
		query := "EXISTS (SELECT 1 FROM jobs j, json_each(j.command_json) a WHERE j.session_id=f.session_id AND " + compare + ")"
		if negative {
			query = "NOT " + query
		}
		return query, []any{value}, nil
	}
	return "", nil, fmt.Errorf("unsupported job field %q", field)
}

func compileTextRule(column, op string, raw json.RawMessage) (string, []any, error) {
	switch op {
	case "is_empty":
		return "(" + column + " IS NULL OR " + column + "='')", nil, nil
	case "is_not_empty":
		return "(" + column + " IS NOT NULL AND " + column + "!='')", nil, nil
	case "in", "not_in":
		values, err := stringValues(raw)
		if err != nil {
			return "", nil, err
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
		verb := " IN "
		if op == "not_in" {
			verb = " NOT IN "
		}
		return "LOWER(" + column + ")" + verb + "(" + placeholders + ")", lowerAny(values), nil
	case "equals", "not_equals", "contains", "not_contains":
		value, err := stringValue(raw)
		if err != nil {
			return "", nil, err
		}
		var expression string
		switch op {
		case "equals":
			expression = "LOWER(" + column + ")=LOWER(?)"
		case "not_equals":
			expression = "(" + column + " IS NULL OR LOWER(" + column + ")!=LOWER(?))"
		case "contains":
			expression = "INSTR(LOWER(COALESCE(" + column + ",'')),LOWER(?))>0"
		case "not_contains":
			expression = "INSTR(LOWER(COALESCE(" + column + ",'')),LOWER(?))=0"
		}
		return expression, []any{value}, nil
	default:
		return "", nil, fmt.Errorf("unsupported text operator %q", op)
	}
}

func compileNumberRule(column, op string, raw json.RawMessage) (string, []any, error) {
	switch op {
	case "is_empty":
		return column + " IS NULL", nil, nil
	case "is_not_empty":
		return column + " IS NOT NULL", nil, nil
	case "between":
		values, err := numberRange(raw)
		if err != nil {
			return "", nil, err
		}
		return column + " BETWEEN ? AND ?", []any{values[0], values[1]}, nil
	}
	symbols := map[string]string{"equals": "=", "not_equals": "!=", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}
	symbol, ok := symbols[op]
	if !ok {
		return "", nil, fmt.Errorf("unsupported number operator %q", op)
	}
	value, err := numberValue(raw)
	if err != nil {
		return "", nil, err
	}
	return column + symbol + "?", []any{value}, nil
}

func compileTimeRule(column, op string, raw json.RawMessage) (string, []any, error) {
	switch op {
	case "is_empty":
		return column + " IS NULL", nil, nil
	case "is_not_empty":
		return column + " IS NOT NULL", nil, nil
	case "between":
		values, err := timestampRange(raw)
		if err != nil {
			return "", nil, err
		}
		return column + " BETWEEN ? AND ?", []any{values[0], values[1]}, nil
	case "before", "after":
		value, err := timestampValue(raw)
		if err != nil {
			return "", nil, err
		}
		symbol := "<"
		if op == "after" {
			symbol = ">"
		}
		return column + symbol + "?", []any{value}, nil
	default:
		return "", nil, fmt.Errorf("unsupported time operator %q", op)
	}
}

func compileExistsTextRule(prefix, column, op string, raw json.RawMessage) (string, []any, error) {
	negative := op == "not_equals" || op == "not_contains" || op == "not_in"
	positive := map[string]string{"not_equals": "equals", "not_contains": "contains", "not_in": "in"}[op]
	if !negative {
		positive = op
	}
	predicate, args, err := compileTextRule(column, positive, raw)
	if err != nil {
		return "", nil, err
	}
	query := "EXISTS (" + prefix + predicate + ")"
	if negative {
		query = "NOT " + query
	}
	return query, args, nil
}

func compileExistsNumberRule(prefix, column, op string, raw json.RawMessage) (string, []any, error) {
	negative := op == "not_equals"
	positive := op
	if negative {
		positive = "equals"
	}
	predicate, args, err := compileNumberRule(column, positive, raw)
	if err != nil {
		return "", nil, err
	}
	query := "EXISTS (" + prefix + predicate + ")"
	if negative {
		query = "NOT " + query
	}
	return query, args, nil
}

func compileExistsTimeRule(prefix, column, op string, raw json.RawMessage) (string, []any, error) {
	predicate, args, err := compileTimeRule(column, op, raw)
	if err != nil {
		return "", nil, err
	}
	return "EXISTS (" + prefix + predicate + ")", args, nil
}

func stringValue(raw json.RawMessage) (string, error) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || len(value) > maxSearchStringBytes {
		return "", errors.New("value must be a string up to 1024 bytes")
	}
	return value, nil
}

func stringValues(raw json.RawMessage) ([]string, error) {
	var values []string
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 || len(values) > 20 {
		return nil, errors.New("value must be an array of 1 to 20 strings")
	}
	for _, value := range values {
		if len(value) > maxSearchStringBytes {
			return nil, errors.New("string value exceeds 1024 bytes")
		}
	}
	return values, nil
}

func numberValue(raw json.RawMessage) (float64, error) {
	var value float64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return 0, errors.New("value must be a number")
	}
	return value, nil
}

func numberRange(raw json.RawMessage) ([2]float64, error) {
	var values []float64
	if json.Unmarshal(raw, &values) != nil || len(values) != 2 || values[0] > values[1] {
		return [2]float64{}, errors.New("value must be an ordered pair of numbers")
	}
	return [2]float64{values[0], values[1]}, nil
}

func timestampValue(raw json.RawMessage) (int64, error) {
	value, err := stringValue(raw)
	if err != nil {
		return 0, errors.New("value must be an RFC3339 timestamp")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, errors.New("value must be an RFC3339 timestamp")
	}
	return parsed.UTC().UnixMilli(), nil
}

func timestampRange(raw json.RawMessage) ([2]int64, error) {
	values, err := stringValues(raw)
	if err != nil || len(values) != 2 {
		return [2]int64{}, errors.New("value must contain two RFC3339 timestamps")
	}
	first, err := time.Parse(time.RFC3339, values[0])
	if err != nil {
		return [2]int64{}, errors.New("value must contain two RFC3339 timestamps")
	}
	second, err := time.Parse(time.RFC3339, values[1])
	if err != nil || first.After(second) {
		return [2]int64{}, errors.New("timestamp range must be ordered")
	}
	return [2]int64{first.UTC().UnixMilli(), second.UTC().UnixMilli()}, nil
}

func lowerAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = strings.ToLower(value)
	}
	return result
}

func invalidFilter(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSearchFilter, fmt.Sprintf(format, args...))
}
