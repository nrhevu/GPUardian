package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuardian/internal/telemetry"
)

func TestApplyPageAggregatesAndDeduplicates(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Minute).Add(-2*time.Minute - 10*time.Second)
	end := start.Add(time.Minute)
	util := 5.0
	memory := uint64(1024)
	missingUtilMemory := uint64(2048)
	exitCode := 0
	rootUID := 0
	jobStarted := start.Add(2 * time.Second)
	jobFinished := start.Add(12 * time.Second)
	page := telemetry.Page{
		NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-5",
		Events: []telemetry.Event{
			event(t, 1, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{
				HistoryQuality: "partial", GroupID: "group-a", Holder: "alice", Purpose: "training", CreatedAt: start,
				StartsAt: start, ExpiresAt: end, Members: []telemetry.ReservationMember{{ReservationID: "r0", GPU: 0}, {ReservationID: "r1", GPU: 1}},
			}),
			event(t, 2, telemetry.EventAuthorizationUpsert, start, telemetry.AuthorizationUpsert{
				AuthorizationID: "auth-internal", GroupID: "group-a", Mode: "user", Holder: "alice", Username: "alice", Command: []string{"python", "train.py"}, CreatedAt: start,
			}),
			event(t, 3, telemetry.EventGPUSample, start.Add(20*time.Second), telemetry.GPUSample{
				WindowStart: start, WindowEnd: start.Add(20 * time.Second), Status: "ok",
				GPUs: []telemetry.GPUSampleEntry{
					{GPU: 0, GroupID: "group-a", UtilizationPct: &util, MemoryUsedBytes: &memory},
					{GPU: 1, GroupID: "group-a", MemoryUsedBytes: &missingUtilMemory},
				},
			}),
			event(t, 4, telemetry.EventJobStarted, jobStarted, telemetry.JobEvent{
				ExecutionID: "job-internal", AuthorizationID: "auth-internal", GroupID: "group-a", Source: "gpuardian_run", Mode: "user",
				Holder: "alice", Command: []string{"python", "train.py", "--token=possibly-secret"}, GPUs: []int{0, 1}, StartedAt: &jobStarted, StartPrecision: "exact",
				RuntimeContext: &telemetry.RuntimeContext{UID: &rootUID, Username: "root"},
			}),
			event(t, 5, telemetry.EventJobFinished, jobFinished, telemetry.JobEvent{
				ExecutionID: "job-internal", AuthorizationID: "auth-internal", GroupID: "group-a", Source: "gpuardian_run", Mode: "user",
				Holder: "alice", FinishedAt: &jobFinished, FinishPrecision: "exact", ExitCode: &exitCode, Reason: "cgroup_empty",
			}),
		},
	}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", page); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", page); err != nil {
		t.Fatal(err)
	}

	sessions, err := store.ListSessions(ctx, SessionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected one session, got %#v", sessions)
	}
	session := sessions[0]
	if session.HistoryQuality != "partial" || session.JobCount != 1 || len(session.GPUSummaries) != 2 {
		t.Fatalf("unexpected session: %#v", session)
	}
	if got := session.GPUSummaries[0]; got.ObservedMS != 20_000 || got.BusyMS != 20_000 || got.AverageUtilization == nil || *got.AverageUtilization != 5 || got.PeakMemoryBytes == nil || *got.PeakMemoryBytes != 1024 {
		t.Fatalf("unexpected GPU summary: %#v", got)
	}
	if got := session.GPUSummaries[1]; got.ObservedMS != 0 || got.BusyMS != 0 || got.AverageUtilization != nil || got.AverageMemoryBytes == nil || *got.AverageMemoryBytes != 2048 || got.PeakMemoryBytes == nil || *got.PeakMemoryBytes != 2048 {
		t.Fatalf("missing utilization was treated as idle or lost VRAM: %#v", got)
	}
	detail, err := store.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Timeline) != 4 {
		t.Fatalf("sample crossing minute boundary should produce two rollups: %#v", detail.Timeline)
	}
	if len(detail.Authorizations) != 1 || len(detail.Authorizations[0].Command) != 2 {
		t.Fatalf("unexpected authorization scopes: %#v", detail.Authorizations)
	}
	if !strings.HasPrefix(detail.Authorizations[0].ID, "scope_") {
		t.Fatalf("authorization scope has no public ID: %#v", detail.Authorizations[0])
	}
	summary, err := store.Summary(ctx, SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Sessions != 1 || summary.Jobs != 1 || !closeFloat(summary.TelemetryCoverage, 1.0/6.0) || summary.BusyRatio != 1 || summary.AverageUtilization == nil || *summary.AverageUtilization != 5 {
		t.Fatalf("unexpected dashboard summary: %#v", summary)
	}
	jobs, err := store.ListJobs(ctx, session.ID, 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || len(jobs[0].Command) != 3 || jobs[0].ExitCode == nil || *jobs[0].ExitCode != 0 || len(jobs[0].GPUs) != 2 {
		t.Fatalf("unexpected jobs: %#v", jobs)
	}
	if jobs[0].RuntimeContext == nil || jobs[0].RuntimeContext.UID == nil || *jobs[0].RuntimeContext.UID != 0 ||
		jobs[0].RuntimeContext.Username != "root" || jobs[0].AuthorizationSelector != "alice" {
		t.Fatalf("runtime context or authorization fallback was lost: %#v", jobs[0])
	}
	if jobs[0].AuthorizationScopeID != detail.Authorizations[0].ID {
		t.Fatalf("job scope ID %q does not match authorization scope %q", jobs[0].AuthorizationScopeID, detail.Authorizations[0].ID)
	}
	publicJSON, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicJSON), "job-internal") || strings.Contains(string(publicJSON), "auth-internal") {
		t.Fatalf("public job JSON leaked internal identity: %s", publicJSON)
	}
}

func TestSummaryCountsOnlyElapsedReservedGPUTime(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	activeStart := now.Add(-time.Hour)
	activeEnd := now.Add(9 * time.Hour)
	revokedStart := now.Add(-3 * time.Hour)
	revokedAt := revokedStart.Add(30 * time.Minute)
	scheduledStart := now.Add(2 * time.Hour)
	utilization := 10.0
	page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-5", Events: []telemetry.Event{
		event(t, 1, telemetry.EventReservationUpsert, activeStart, telemetry.ReservationUpsert{
			GroupID: "group-active", Holder: "alice", Purpose: "active", CreatedAt: activeStart, StartsAt: activeStart, ExpiresAt: activeEnd,
			Members: []telemetry.ReservationMember{{ReservationID: "active-0", GPU: 0}, {ReservationID: "active-1", GPU: 1}},
		}),
		event(t, 2, telemetry.EventGPUSample, activeStart.Add(30*time.Minute), telemetry.GPUSample{
			WindowStart: activeStart, WindowEnd: activeStart.Add(30 * time.Minute), Status: "ok",
			GPUs: []telemetry.GPUSampleEntry{
				{GPU: 0, GroupID: "group-active", UtilizationPct: &utilization},
				{GPU: 1, GroupID: "group-active", UtilizationPct: &utilization},
			},
		}),
		event(t, 3, telemetry.EventReservationUpsert, revokedStart, telemetry.ReservationUpsert{
			GroupID: "group-revoked", Holder: "bob", Purpose: "revoked", CreatedAt: revokedStart, StartsAt: revokedStart, ExpiresAt: now.Add(7 * time.Hour),
			Members: []telemetry.ReservationMember{{ReservationID: "revoked-0", GPU: 2}},
		}),
		event(t, 4, telemetry.EventReservationEnded, revokedAt, telemetry.ReservationEnded{
			GroupID: "group-revoked", EndedAt: revokedAt, Reason: "revoked",
		}),
		event(t, 5, telemetry.EventReservationUpsert, now, telemetry.ReservationUpsert{
			GroupID: "group-scheduled", Holder: "carol", Purpose: "scheduled", CreatedAt: now, StartsAt: scheduledStart, ExpiresAt: scheduledStart.Add(2 * time.Hour),
			Members: []telemetry.ReservationMember{{ReservationID: "scheduled-0", GPU: 3}},
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", page); err != nil {
		t.Fatal(err)
	}

	searchSummary, sessions, _, err := store.Search(ctx, SearchExpression{}, SearchSort{}, 10, SearchCursor{})
	if err != nil {
		t.Fatal(err)
	}
	legacySummary, err := store.Summary(ctx, SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for name, summary := range map[string]DashboardSummary{"search": searchSummary, "legacy": legacySummary} {
		if summary.Reservations != 3 || math.Abs(summary.ReservedGPUHours-2.5) > 0.01 {
			t.Fatalf("%s summary counted scheduled or post-revoke time: %+v", name, summary)
		}
		if math.Abs(summary.TelemetryCoverage-0.4) > 0.01 {
			t.Fatalf("%s telemetry coverage did not use elapsed reserved time: %+v", name, summary)
		}
	}
	for _, session := range sessions {
		if session.Purpose != "active" {
			continue
		}
		if len(session.GPUSummaries) != 2 {
			t.Fatalf("active GPU summaries = %+v", session.GPUSummaries)
		}
		for _, gpu := range session.GPUSummaries {
			if math.Abs(gpu.Coverage-0.5) > 0.01 {
				t.Fatalf("active GPU %d coverage = %v, want about 0.5", gpu.GPU, gpu.Coverage)
			}
		}
		return
	}
	t.Fatal("active reservation was not returned")
}

func TestSummaryIncludesBusyGPUTimeOutsideReservations(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Minute)
	reservationEnd := start.Add(time.Minute)
	reservedUtilization := 10.0
	unreservedUtilization := 20.0
	idleUtilization := 0.0
	page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-4", Events: []telemetry.Event{
		event(t, 1, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{
			GroupID: "group-a", Holder: "alice", Purpose: "training", CreatedAt: start, StartsAt: start, ExpiresAt: reservationEnd,
			Members: []telemetry.ReservationMember{{ReservationID: "r0", GPU: 0}},
		}),
		event(t, 2, telemetry.EventGPUSample, start.Add(10*time.Second), telemetry.GPUSample{
			WindowStart: start, WindowEnd: start.Add(10 * time.Second), Status: "ok",
			GPUs: []telemetry.GPUSampleEntry{{GPU: 0, GroupID: "group-a", UtilizationPct: &reservedUtilization}},
		}),
		event(t, 3, telemetry.EventGPUSample, start.Add(30*time.Second), telemetry.GPUSample{
			WindowStart: start.Add(10 * time.Second), WindowEnd: start.Add(30 * time.Second), Status: "ok",
			GPUs: []telemetry.GPUSampleEntry{{GPU: 1, UtilizationPct: &unreservedUtilization}},
		}),
		event(t, 4, telemetry.EventGPUSample, start.Add(40*time.Second), telemetry.GPUSample{
			WindowStart: start.Add(30 * time.Second), WindowEnd: start.Add(40 * time.Second), Status: "ok",
			GPUs: []telemetry.GPUSampleEntry{{GPU: 1, UtilizationPct: &idleUtilization}},
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", page); err != nil {
		t.Fatal(err)
	}
	otherNodeUtilization := 100.0
	otherPage := telemetry.Page{NodeID: "node-b", StreamID: "stream-b", NextCursor: "cursor-1", Events: []telemetry.Event{
		event(t, 1, telemetry.EventGPUSample, start.Add(time.Minute), telemetry.GPUSample{
			WindowStart: start, WindowEnd: start.Add(time.Minute), Status: "ok",
			GPUs: []telemetry.GPUSampleEntry{{GPU: 0, UtilizationPct: &otherNodeUtilization}},
		}),
	}}
	if err := store.ApplyPage(ctx, "server-b", "Other GPU node", otherPage); err != nil {
		t.Fatal(err)
	}

	searchSummary, sessions, _, err := store.Search(ctx, SearchExpression{ServerID: "server-a"}, SearchSort{}, 10, SearchCursor{})
	if err != nil {
		t.Fatal(err)
	}
	legacySummary, err := store.Summary(ctx, SessionFilter{ServerID: "server-a"})
	if err != nil {
		t.Fatal(err)
	}
	for name, summary := range map[string]DashboardSummary{"search": searchSummary, "legacy": legacySummary} {
		if math.Abs(summary.ReservedGPUHours-1.0/60.0) > 0.0001 ||
			math.Abs(summary.TelemetryCoverage-1.0/6.0) > 0.0001 {
			t.Fatalf("%s reservation metrics changed: %+v", name, summary)
		}
		if math.Abs(summary.BusyGPUHours-30.0/3600.0) > 0.0001 ||
			math.Abs(summary.BusyRatio-0.75) > 0.0001 ||
			summary.AverageUtilization == nil || math.Abs(*summary.AverageUtilization-12.5) > 0.0001 {
			t.Fatalf("%s node-wide busy metrics = %+v", name, summary)
		}
	}
	if len(sessions) != 1 || sessions[0].Purpose != "training" {
		t.Fatalf("unreserved samples created a session: %+v", sessions)
	}

	filteredSummary, filteredSessions, _, err := store.Search(ctx, SearchExpression{ServerID: "server-a", Groups: []SearchGroup{{Rules: []SearchRule{
		searchRule("purpose", "equals", "training"),
	}}}}, SearchSort{}, 10, SearchCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(filteredSessions) != 1 || filteredSessions[0].Purpose != "training" {
		t.Fatalf("filtered sessions = %+v", filteredSessions)
	}
	if math.Abs(filteredSummary.BusyGPUHours-10.0/3600.0) > 0.0001 ||
		filteredSummary.BusyRatio != 1 ||
		filteredSummary.AverageUtilization == nil || *filteredSummary.AverageUtilization != reservedUtilization {
		t.Fatalf("filtered busy metrics did not follow matched sessions: %+v", filteredSummary)
	}
}

func TestNodeGPURollupMigrationPreservesReservationMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	utilization := 25.0
	page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-2", Events: []telemetry.Event{
		event(t, 1, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{
			GroupID: "group-a", Holder: "alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Minute),
			Members: []telemetry.ReservationMember{{ReservationID: "r0", GPU: 0}},
		}),
		event(t, 2, telemetry.EventGPUSample, start.Add(10*time.Second), telemetry.GPUSample{
			WindowStart: start, WindowEnd: start.Add(10 * time.Second), Status: "ok",
			GPUs: []telemetry.GPUSampleEntry{{GPU: 0, GroupID: "group-a", UtilizationPct: &utilization}},
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", page); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("DROP TABLE node_gpu_minute_rollups"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("DELETE FROM schema_migrations WHERE version=4"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	summary, _, _, err := store.Search(ctx, SearchExpression{ServerID: "server-a"}, SearchSort{}, 10, SearchCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(summary.BusyGPUHours-10.0/3600.0) > 0.0001 || summary.BusyRatio != 1 ||
		summary.AverageUtilization == nil || *summary.AverageUtilization != utilization {
		t.Fatalf("migrated node metrics = %+v", summary)
	}
}

func TestRuntimeContextMigrationUpgradesV4Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{
		"runtime_kubernetes_container_name", "runtime_kubernetes_pod_name", "runtime_kubernetes_namespace",
		"runtime_docker_container_name", "runtime_container_id", "runtime_username", "runtime_uid",
	} {
		if _, err := store.DB().Exec("ALTER TABLE jobs DROP COLUMN " + column); err != nil {
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if _, err := store.DB().Exec("DELETE FROM schema_migrations WHERE version=5"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.DB().Query("PRAGMA table_info(jobs)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		found[name] = true
	}
	for _, column := range []string{"runtime_uid", "runtime_username", "runtime_container_id", "runtime_docker_container_name", "runtime_kubernetes_namespace", "runtime_kubernetes_pod_name", "runtime_kubernetes_container_name"} {
		if !found[column] {
			t.Fatalf("migration did not add %s", column)
		}
	}
}

func TestOneJobCanBelongToMultipleReservationSessionsWithoutInflatingSummary(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Minute)
	jobStart := start.Add(time.Second)
	page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-3", Events: []telemetry.Event{
		event(t, 1, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{GroupID: "group-a", Holder: "alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour), Members: []telemetry.ReservationMember{{ReservationID: "r0", GPU: 0}}}),
		event(t, 2, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{GroupID: "group-b", Holder: "alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour), Members: []telemetry.ReservationMember{{ReservationID: "r1", GPU: 1}}}),
		event(t, 3, telemetry.EventJobStarted, jobStart, telemetry.JobEvent{ExecutionID: "job-shared", AuthorizationID: "auth-shared", GroupID: "group-a", GroupIDs: []string{"group-a", "group-b"}, Source: "gpuardian_run", Mode: "run", Holder: "alice", StartedAt: &jobStart}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", page); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx, SessionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].JobCount != 1 || sessions[1].JobCount != 1 {
		t.Fatalf("shared job was not attached to both sessions: %+v", sessions)
	}
	summary, _, _, err := store.Search(ctx, SearchExpression{}, SearchSort{}, 10, SearchCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Sessions != 2 || summary.Jobs != 1 {
		t.Fatalf("shared job inflated search summary: %+v", summary)
	}
}

func TestClaimedJobCreatesDashboardActivityWithoutReservationHours(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	started := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	finished := started.Add(10 * time.Second)
	utilization := 42.0
	startPage := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-2", Events: []telemetry.Event{
		event(t, 1, telemetry.EventJobStarted, started, telemetry.JobEvent{
			ExecutionID: "job-claimed", AuthorizationID: "auth-claimed", Source: "authorized_process",
			RunName: "GLM TP4 benchmark", TokenMode: "claimed", Mode: "user", Holder: "alice", Command: []string{"python", "train.py"}, GPUs: []int{3}, StartedAt: &started, StartPrecision: "observed",
		}),
		event(t, 2, telemetry.EventGPUSample, started.Add(5*time.Second), telemetry.GPUSample{
			WindowStart: started, WindowEnd: started.Add(5 * time.Second), Status: "ok",
			GPUs: []telemetry.GPUSampleEntry{{GPU: 3, GroupIDs: []string{"claimed-auth:auth-claimed"}, UtilizationPct: &utilization}},
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", startPage); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileOpenSessions(ctx, "server-a", nil, started.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	active, err := store.ListSessions(ctx, SessionFilter{Status: "active", Limit: 10})
	if err != nil || len(active) != 1 || active[0].Kind != "claimed_run" {
		t.Fatalf("active claimed activity = %+v, error = %v", active, err)
	}

	finishPage := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-3", Events: []telemetry.Event{
		event(t, 3, telemetry.EventJobFinished, finished, telemetry.JobEvent{
			ExecutionID: "job-claimed", AuthorizationID: "auth-claimed", Source: "authorized_process",
			TokenMode: "claimed", Mode: "user", Holder: "alice", Command: []string{"python", "train.py"}, GPUs: []int{3}, StartedAt: &started,
			FinishedAt: &finished, StartPrecision: "observed", FinishPrecision: "observed", Reason: "process_gone",
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", finishPage); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx, SessionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Kind != "claimed_run" || sessions[0].Status != "completed" ||
		sessions[0].Purpose != "GLM TP4 benchmark" || sessions[0].JobCount != 1 || len(sessions[0].GPUs) != 1 || sessions[0].GPUs[0] != 3 {
		t.Fatalf("claimed activity = %+v", sessions)
	}
	if len(sessions[0].GPUSummaries) != 1 || sessions[0].GPUSummaries[0].ObservedMS != 5_000 ||
		sessions[0].GPUSummaries[0].BusyMS != 5_000 || sessions[0].GPUSummaries[0].AverageUtilization == nil ||
		*sessions[0].GPUSummaries[0].AverageUtilization != utilization {
		t.Fatalf("claimed utilization = %+v", sessions[0].GPUSummaries)
	}
	summary, _, _, err := store.Search(ctx, SearchExpression{}, SearchSort{}, 10, SearchCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Sessions != 1 || summary.Reservations != 0 || summary.ClaimedRuns != 1 ||
		summary.ReservedGPUHours != 0 || summary.Jobs != 1 || summary.AverageUtilization == nil ||
		*summary.AverageUtilization != utilization {
		t.Fatalf("claimed summary = %+v", summary)
	}
}

func TestClaimedJobsWithSameAuthorizationShareOneSession(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	started := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	firstFinished := started.Add(10 * time.Second)
	secondFinished := started.Add(20 * time.Second)
	startPage := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-2", Events: []telemetry.Event{
		event(t, 1, telemetry.EventJobStarted, started, telemetry.JobEvent{
			ExecutionID: "job-gpu-0", AuthorizationID: "auth-shared", Source: "authorized_process", RunName: "Shared training",
			TokenMode: "claimed", Mode: "docker", Holder: "alice", Command: []string{"python", "worker.py", "0"}, GPUs: []int{0}, StartedAt: &started,
		}),
		event(t, 2, telemetry.EventJobStarted, started, telemetry.JobEvent{
			ExecutionID: "job-gpu-1", AuthorizationID: "auth-shared", Source: "authorized_process", RunName: "Shared training",
			TokenMode: "claimed", Mode: "docker", Holder: "alice", Command: []string{"python", "worker.py", "1"}, GPUs: []int{1}, StartedAt: &started,
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", startPage); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx, SessionFilter{Limit: 10})
	if err != nil || len(sessions) != 1 || sessions[0].JobCount != 2 || !reflect.DeepEqual(sessions[0].GPUs, []int{0, 1}) || sessions[0].Status != "active" {
		t.Fatalf("grouped claimed session = %+v, error = %v", sessions, err)
	}

	finishFirst := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-3", Events: []telemetry.Event{
		event(t, 3, telemetry.EventJobFinished, firstFinished, telemetry.JobEvent{
			ExecutionID: "job-gpu-0", AuthorizationID: "auth-shared", Source: "authorized_process", RunName: "Shared training",
			TokenMode: "claimed", Mode: "docker", Holder: "alice", GPUs: []int{0}, StartedAt: &started, FinishedAt: &firstFinished,
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", finishFirst); err != nil {
		t.Fatal(err)
	}
	active, err := store.ListSessions(ctx, SessionFilter{Status: "active", Limit: 10})
	if err != nil || len(active) != 1 {
		t.Fatalf("session finalized while a sibling job was active: %+v, error = %v", active, err)
	}

	finishSecond := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-4", Events: []telemetry.Event{
		event(t, 4, telemetry.EventJobFinished, secondFinished, telemetry.JobEvent{
			ExecutionID: "job-gpu-1", AuthorizationID: "auth-shared", Source: "authorized_process", RunName: "Shared training",
			TokenMode: "claimed", Mode: "docker", Holder: "alice", GPUs: []int{1}, StartedAt: &started, FinishedAt: &secondFinished,
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", finishSecond); err != nil {
		t.Fatal(err)
	}
	completed, err := store.ListSessions(ctx, SessionFilter{Status: "completed", Limit: 10})
	if err != nil || len(completed) != 1 || completed[0].JobCount != 2 || completed[0].FinalizedAt == nil || !completed[0].FinalizedAt.Equal(secondFinished) {
		t.Fatalf("completed grouped claimed session = %+v, error = %v", completed, err)
	}
	jobs, err := store.ListJobs(ctx, completed[0].ID, 10, 0, "")
	if err != nil || len(jobs) != 2 {
		t.Fatalf("grouped claimed jobs = %+v, error = %v", jobs, err)
	}
}

func TestClaimedSessionMigrationGroupsLegacyGPUJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	finished := started.Add(20 * time.Second)
	if _, err := store.DB().Exec(`INSERT INTO nodes(node_id,last_server_id,first_seen_at_ms,last_seen_at_ms) VALUES('node-a','server-a',?,?)`, millis(started), millis(finished)); err != nil {
		t.Fatal(err)
	}
	for index, id := range []string{"legacy-a", "legacy-b"} {
		if _, err := store.DB().Exec(`INSERT INTO reservation_sessions(session_id,node_id,server_id,server_name,group_id,kind,owner_username,purpose,source,
			created_at_ms,starts_at_ms,expires_at_ms,finalized_at_ms,updated_at_ms)
			VALUES(?,'node-a','server-a','GPU node',?,'claimed_run','alice','Shared training','cli',?,?,?,?,?)`,
			id, fmt.Sprintf("claimed:job-%d", index), millis(started), millis(started), millis(finished), millis(finished), millis(finished)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO session_gpus(session_id,gpu,reservation_id) VALUES(?,?,?)`, id, index, fmt.Sprintf("claim-%d", index)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO session_gpu_summaries(session_id,gpu,observed_ms,busy_ms,utilization_integral,valid_samples)
			VALUES(?,?,1000,1000,50000,1)`, id, index); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO jobs(node_id,job_id,session_id,authorization_id,source,mode,holder,started_at_ms,finished_at_ms,updated_at_ms)
			VALUES('node-a',?,?, 'auth-shared','authorized_process','docker','alice',?,?,?)`,
			fmt.Sprintf("job-%d", index), id, millis(started), millis(finished), millis(finished)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO job_gpus(node_id,job_id,gpu) VALUES('node-a',?,?)`, fmt.Sprintf("job-%d", index), index); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec("DELETE FROM schema_migrations WHERE version=7"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sessions, err := store.ListSessions(context.Background(), SessionFilter{Limit: 10})
	if err != nil || len(sessions) != 1 || sessions[0].JobCount != 2 || !reflect.DeepEqual(sessions[0].GPUs, []int{0, 1}) {
		t.Fatalf("migrated claimed sessions = %+v, error = %v", sessions, err)
	}
	var groupID string
	if err := store.DB().QueryRow("SELECT group_id FROM reservation_sessions WHERE session_id=?", sessions[0].ID).Scan(&groupID); err != nil || groupID != "claimed-auth:auth-shared" {
		t.Fatalf("canonical claimed group = %q, error = %v", groupID, err)
	}
	jobs, err := store.ListJobs(context.Background(), sessions[0].ID, 10, 0, "")
	if err != nil || len(jobs) != 2 {
		t.Fatalf("migrated claimed jobs = %+v, error = %v", jobs, err)
	}
	utilization := 25.0
	legacySample := telemetry.Page{NodeID: "node-a", StreamID: "legacy-stream", NextCursor: "cursor-1", Events: []telemetry.Event{
		event(t, 1, telemetry.EventGPUSample, started.Add(2*time.Second), telemetry.GPUSample{
			WindowStart: started.Add(time.Second), WindowEnd: started.Add(2 * time.Second), Status: "ok",
			GPUs: []telemetry.GPUSampleEntry{{GPU: 1, GroupID: "claimed:job-1", UtilizationPct: &utilization}},
		}),
	}}
	if err := store.ApplyPage(context.Background(), "server-a", "GPU node", legacySample); err != nil {
		t.Fatal(err)
	}
	migrated, err := store.GetSession(context.Background(), sessions[0].ID)
	if err != nil || len(migrated.GPUSummaries) != 2 || migrated.GPUSummaries[1].ObservedMS != 2_000 {
		t.Fatalf("legacy claimed sample was not routed to the grouped session: %+v, error = %v", migrated.GPUSummaries, err)
	}
}

func TestReconcileOpenSessionsClosesReservationsMissingFromNodeSnapshot(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Millisecond).Add(-5 * time.Minute)
	page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-2", Events: []telemetry.Event{
		event(t, 1, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{
			GroupID: "group-live", Holder: "alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour),
			Members: []telemetry.ReservationMember{{ReservationID: "r0", GPU: 0}},
		}),
		event(t, 2, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{
			GroupID: "group-missing", Holder: "bob", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour),
			Members: []telemetry.ReservationMember{{ReservationID: "r1", GPU: 1}},
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "GPU node", page); err != nil {
		t.Fatal(err)
	}
	observedAt := start.Add(10 * time.Minute)
	if err := store.ReconcileOpenSessions(ctx, "server-a", []string{"group-live"}, observedAt); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx, SessionFilter{ServerID: "server-a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v", sessions)
	}
	for _, session := range sessions {
		switch session.Owner {
		case "alice":
			if session.Status != "active" || session.RevokedAt != nil {
				t.Fatalf("live reservation was closed: %+v", session)
			}
		case "bob":
			if session.Status != "revoked" || session.RevokedAt == nil || !session.RevokedAt.Equal(observedAt) ||
				session.FinalizedAt == nil || !session.FinalizedAt.Equal(observedAt) || session.HistoryQuality != "partial" {
				t.Fatalf("missing reservation was not reconciled: %+v", session)
			}
		default:
			t.Fatalf("unexpected session: %+v", session)
		}
	}
	active, err := store.ListSessions(ctx, SessionFilter{ServerID: "server-a", Status: "active", Limit: 10})
	if err != nil || len(active) != 1 || active[0].Owner != "alice" {
		t.Fatalf("active sessions = %+v, error = %v", active, err)
	}
}

func TestResultOwnershipAndOptimisticVersion(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	start := time.Now().UTC()
	page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor", Events: []telemetry.Event{
		event(t, 1, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{
			ExternalSessionID: "sess_owned_a", GroupID: "group-a", Holder: "Alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour),
			Members: []telemetry.ReservationMember{{ReservationID: "r0", GPU: 0}},
		}),
		event(t, 2, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{
			ExternalSessionID: "sess_owned_b", GroupID: "group-b", Holder: "Alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour),
			Members: []telemetry.ReservationMember{{ReservationID: "r1", GPU: 1}},
		}),
	}}
	if err := store.ApplyPage(ctx, "server-a", "node", page); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx, SessionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	id := sessions[0].ID
	firstPage, err := store.ListSessions(ctx, SessionFilter{Limit: 1})
	if err != nil || len(firstPage) != 1 {
		t.Fatalf("first page = %#v, %v", firstPage, err)
	}
	secondPage, err := store.ListSessions(ctx, SessionFilter{Limit: 1, BeforeMS: firstPage[0].StartsAt.UnixMilli(), BeforeID: firstPage[0].ID})
	if err != nil || len(secondPage) != 1 || secondPage[0].ID == firstPage[0].ID {
		t.Fatalf("second page = %#v, %v", secondPage, err)
	}
	if _, err := store.PutResult(ctx, id, "bob", OutcomeSuccess, "no", nil, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected forbidden, got %v", err)
	}
	result, err := store.PutResult(ctx, id, "alice", OutcomeSuccess, "finished", []Artifact{{Label: "report", URL: "https://example.test/report"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != 1 || result.Outcome != OutcomeSuccess || len(result.Artifacts) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := store.PutResult(ctx, id, "Alice", OutcomeFailed, "stale", nil, 0); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected version conflict, got %v", err)
	}
}

func TestCLIResultEditableOnlyForKnownOwner(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	start := time.Now().UTC()
	page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor", Events: []telemetry.Event{
		event(t, 1, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{GroupID: "known", Holder: "Alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour), Members: []telemetry.ReservationMember{{ReservationID: "r0", GPU: 0}}}),
		event(t, 2, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{GroupID: "unknown", Holder: "ghost", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour), Members: []telemetry.ReservationMember{{ReservationID: "r1", GPU: 1}}}),
	}}
	if err := store.ApplyPageWithOwners(ctx, "server", "node", page, map[string]bool{"alice": true}); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx, SessionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		switch session.Owner {
		case "Alice":
			if !session.ResultEditable {
				t.Fatal("known CLI holder is read-only")
			}
		case "ghost":
			if session.ResultEditable {
				t.Fatal("unknown CLI holder is editable")
			}
			if _, err := store.PutResult(ctx, session.ID, "ghost", OutcomeSuccess, "", nil, 0); !errors.Is(err, ErrForbidden) {
				t.Fatalf("unknown holder result error = %v", err)
			}
		}
	}
}

func TestMigrationReopenWALAndRejectNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := store.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode = %q, %v", mode, err)
	}
	var conns []*sql.Conn
	for index := 0; index < 4; index++ {
		conn, err := store.DB().Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		var foreignKeys, trustedSchema int
		if err := conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(context.Background(), "PRAGMA trusted_schema").Scan(&trustedSchema); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || trustedSchema != 0 {
			t.Fatalf("connection %d pragmas: foreign_keys=%d trusted_schema=%d", index, foreignKeys, trustedSchema)
		}
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := store.DB().Exec("INSERT INTO schema_migrations(version,checksum,applied_at_ms) VALUES(8,'future',?)", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("newer schema error = %v", err)
	}
}

func event(t *testing.T, seq uint64, kind string, at time.Time, payload any) telemetry.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return telemetry.Event{Seq: seq, Type: kind, OccurredAt: at, Payload: raw}
}

func closeFloat(left, right float64) bool { return math.Abs(left-right) < 1e-9 }
