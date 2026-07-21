package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
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
	start := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Second)
	end := start.Add(time.Minute)
	util := 5.0
	memory := uint64(1024)
	missingUtilMemory := uint64(2048)
	exitCode := 0
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
				AuthorizationID: "auth-internal", GroupID: "group-a", Mode: "run", Holder: "alice", Command: []string{"python", "train.py"}, CreatedAt: start,
			}),
			event(t, 3, telemetry.EventGPUSample, start.Add(20*time.Second), telemetry.GPUSample{
				WindowStart: start, WindowEnd: start.Add(20 * time.Second), Status: "ok",
				GPUs: []telemetry.GPUSampleEntry{
					{GPU: 0, GroupID: "group-a", UtilizationPct: &util, MemoryUsedBytes: &memory},
					{GPU: 1, GroupID: "group-a", MemoryUsedBytes: &missingUtilMemory},
				},
			}),
			event(t, 4, telemetry.EventJobStarted, jobStarted, telemetry.JobEvent{
				ExecutionID: "job-internal", AuthorizationID: "auth-internal", GroupID: "group-a", Source: "gpuardian_run", Mode: "run",
				Holder: "alice", Command: []string{"python", "train.py", "--token=possibly-secret"}, GPUs: []int{0, 1}, StartedAt: &jobStarted, StartPrecision: "exact",
			}),
			event(t, 5, telemetry.EventJobFinished, jobFinished, telemetry.JobEvent{
				ExecutionID: "job-internal", AuthorizationID: "auth-internal", GroupID: "group-a", Source: "gpuardian_run", Mode: "run",
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
	publicJSON, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicJSON), "job-internal") || strings.Contains(string(publicJSON), "auth-internal") {
		t.Fatalf("public job JSON leaked internal identity: %s", publicJSON)
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
	if _, err := store.DB().Exec("INSERT INTO schema_migrations(version,checksum,applied_at_ms) VALUES(3,'future',?)", time.Now().UnixMilli()); err != nil {
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
