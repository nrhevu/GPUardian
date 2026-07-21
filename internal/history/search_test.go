package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestSearchExpressionFiltersSessionFactsAndJobs(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	createSearchSession(t, store, "alpha", "node-a", "server-a", "alice", "training alpha", now.Add(-time.Hour), now.Add(time.Hour), []int{0, 1})
	createSearchSession(t, store, "beta", "node-b", "server-b", "bob", "evaluation", now.Add(-3*time.Hour), now.Add(-2*time.Hour), []int{2})
	createSearchSession(t, store, "revoked", "node-a", "server-a", "alice", "cancelled", now.Add(-time.Hour), now.Add(time.Hour), []int{3})
	if _, err := store.DB().Exec("UPDATE reservation_sessions SET revoked_at_ms=? WHERE session_id='revoked'", now.Add(-30*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE session_gpu_summaries SET observed_ms=1000,busy_ms=500,utilization_integral=8000,
		memory_integral=4096000,memory_observed_ms=1000,peak_memory_bytes=8192 WHERE session_id='alpha'`); err != nil {
		t.Fatal(err)
	}
	insertSearchJob(t, store, "node-a", "job-alpha", "alpha", "gpuardian_run", "run", []string{"python", "train.py"}, 0, []int{0, 1}, now.Add(-20*time.Minute))
	insertSearchJob(t, store, "node-b", "job-beta", "beta", "authorized_process", "user", []string{"bash", "evaluate.sh"}, 2, []int{2}, now.Add(-150*time.Minute))

	t.Run("and groups with or rules", func(t *testing.T) {
		expression := SearchExpression{Groups: []SearchGroup{
			{Rules: []SearchRule{searchRule("purpose", "contains", "alpha"), searchRule("status", "equals", "revoked")}},
			{Rules: []SearchRule{searchRule("job.command", "contains", "train.py"), searchRule("job.exit_code", "equals", 99)}},
		}}
		summary, sessions, _, err := store.Search(ctx, expression, SearchSort{}, 50, SearchCursor{})
		if err != nil {
			t.Fatal(err)
		}
		if summary.Sessions != 1 || summary.Jobs != 1 || len(sessions) != 1 || sessions[0].ID != "alpha" {
			t.Fatalf("unexpected search result: summary=%+v sessions=%+v", summary, sessions)
		}
	})

	t.Run("metrics gpu and job details", func(t *testing.T) {
		expression := SearchExpression{Groups: []SearchGroup{
			{Rules: []SearchRule{searchRule("average_utilization_percent", "gt", 7)}},
			{Rules: []SearchRule{searchRule("gpu", "equals", 1)}},
			{Rules: []SearchRule{searchRule("job.gpu", "equals", 0)}},
			{Rules: []SearchRule{searchRule("job.source", "equals", "gpuardian_run")}},
		}}
		_, sessions, _, err := store.Search(ctx, expression, SearchSort{}, 50, SearchCursor{})
		if err != nil || len(sessions) != 1 || sessions[0].ID != "alpha" {
			t.Fatalf("metric/job result=%+v err=%v", sessions, err)
		}
	})

	t.Run("time overlap", func(t *testing.T) {
		expression := SearchExpression{Groups: []SearchGroup{{Rules: []SearchRule{
			searchRule("session_window", "overlaps", []string{now.Add(-40 * time.Minute).Format(time.RFC3339), now.Add(10 * time.Minute).Format(time.RFC3339)}),
		}}}}
		_, sessions, _, err := store.Search(ctx, expression, SearchSort{}, 50, SearchCursor{})
		if err != nil || len(sessions) != 2 || sessions[0].ID != "revoked" || sessions[1].ID != "alpha" {
			t.Fatalf("time overlap result=%+v err=%v", sessions, err)
		}
	})

	t.Run("null metric and negative job rule", func(t *testing.T) {
		expression := SearchExpression{Groups: []SearchGroup{
			{Rules: []SearchRule{searchRuleWithoutValue("average_utilization_percent", "is_empty")}},
			{Rules: []SearchRule{searchRule("job.command", "not_contains", "train.py")}},
		}}
		_, sessions, _, err := store.Search(ctx, expression, SearchSort{}, 50, SearchCursor{})
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 2 || sessions[0].ID != "revoked" || sessions[1].ID != "beta" {
			t.Fatalf("unexpected null/negative result: %+v", sessions)
		}
	})

	t.Run("literal injection-like text", func(t *testing.T) {
		_, sessions, _, err := store.Search(ctx, SearchExpression{Groups: []SearchGroup{{Rules: []SearchRule{
			searchRule("purpose", "contains", `alpha%' OR 1=1 --`),
		}}}}, SearchSort{}, 50, SearchCursor{})
		if err != nil || len(sessions) != 0 {
			t.Fatalf("literal text result=%+v err=%v", sessions, err)
		}
	})
}

func TestSearchExpressionValidationAndCursor(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	createSearchSession(t, store, "first", "node", "server", "alice", "first", now.Add(-time.Hour), now.Add(time.Hour), []int{0})
	createSearchSession(t, store, "second", "node", "server", "alice", "second", now.Add(-2*time.Hour), now.Add(-time.Hour), []int{0})
	_, firstPage, cursor, err := store.Search(context.Background(), SearchExpression{}, SearchSort{}, 1, SearchCursor{})
	if err != nil || len(firstPage) != 1 || firstPage[0].ID != "first" {
		t.Fatalf("first page=%+v err=%v", firstPage, err)
	}
	_, secondPage, _, err := store.Search(context.Background(), SearchExpression{}, SearchSort{}, 1, cursor)
	if err != nil || len(secondPage) != 1 || secondPage[0].ID != "second" {
		t.Fatalf("second page=%+v err=%v", secondPage, err)
	}
	_, sorted, sortCursor, err := store.Search(context.Background(), SearchExpression{}, SearchSort{Field: "purpose", Direction: "asc"}, 1, SearchCursor{})
	if err != nil || len(sorted) != 1 || sorted[0].ID != "first" {
		t.Fatalf("sorted first page=%+v err=%v", sorted, err)
	}
	_, sortedNext, _, err := store.Search(context.Background(), SearchExpression{}, SearchSort{Field: "purpose", Direction: "asc"}, 1, sortCursor)
	if err != nil || len(sortedNext) != 1 || sortedNext[0].ID != "second" {
		t.Fatalf("sorted second page=%+v err=%v", sortedNext, err)
	}
	_, _, _, err = store.Search(context.Background(), SearchExpression{Groups: []SearchGroup{{Rules: []SearchRule{searchRule("unknown", "equals", "x")}}}}, SearchSort{}, 10, SearchCursor{})
	if !errors.Is(err, ErrInvalidSearchFilter) {
		t.Fatalf("unknown field error=%v", err)
	}
	tooMany := SearchExpression{Groups: make([]SearchGroup, maxSearchGroups+1)}
	if _, _, _, err := store.Search(context.Background(), tooMany, SearchSort{}, 10, SearchCursor{}); !errors.Is(err, ErrInvalidSearchFilter) {
		t.Fatalf("group limit error=%v", err)
	}
	if _, _, _, err := store.Search(context.Background(), SearchExpression{}, SearchSort{Field: "private_sql", Direction: "asc"}, 10, SearchCursor{}); !errors.Is(err, ErrInvalidSearchFilter) {
		t.Fatalf("sort validation error=%v", err)
	}
	revokedSummary, err := store.Summary(context.Background(), SessionFilter{Status: "revoked"})
	if err != nil || revokedSummary.Sessions != 0 {
		t.Fatalf("legacy status summary=%+v err=%v", revokedSummary, err)
	}
}

func createSearchSession(t *testing.T, store *Store, id, node, server, owner, purpose string, starts, expires time.Time, gpus []int) {
	t.Helper()
	if err := store.PrepareSession(context.Background(), id, node, server, server, owner, purpose, starts, expires, gpus); err != nil {
		t.Fatal(err)
	}
	reservations := make([]string, len(gpus))
	for index := range reservations {
		reservations[index] = fmt.Sprintf("%s-reservation-%d", id, index)
	}
	if err := store.ConfirmSession(context.Background(), id, id+"-group", reservations, gpus); err != nil {
		t.Fatal(err)
	}
}

func insertSearchJob(t *testing.T, store *Store, node, id, session, source, mode string, command []string, exitCode int, gpus []int, started time.Time) {
	t.Helper()
	commandJSON, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO jobs(node_id,job_id,session_id,authorization_id,source,mode,holder,command_json,
		started_at_ms,finished_at_ms,start_precision,finish_precision,exit_code,end_reason,updated_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, node, id, session, "auth-"+id, source, mode, "holder", string(commandJSON),
		started.UnixMilli(), started.Add(time.Minute).UnixMilli(), "exact", "exact", exitCode, "cgroup_empty", started.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	for _, gpu := range gpus {
		if _, err := store.DB().Exec("INSERT INTO job_gpus(node_id,job_id,gpu) VALUES(?,?,?)", node, id, gpu); err != nil {
			t.Fatal(err)
		}
	}
}

func searchRule(field, operator string, value any) SearchRule {
	data, _ := json.Marshal(value)
	return SearchRule{Field: field, Operator: operator, Value: data}
}

func searchRuleWithoutValue(field, operator string) SearchRule {
	return SearchRule{Field: field, Operator: operator}
}
