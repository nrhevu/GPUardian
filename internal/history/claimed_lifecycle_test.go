package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gpuardian/internal/telemetry"
)

func TestClaimedJobMovedToReservationFinalizesLinkedSession(t *testing.T) {
	for _, sibling := range []bool{false, true} {
		name := "only-job"
		if sibling {
			name = "unfinished-sibling"
		}
		t.Run(name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "history.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			start := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
			finish := start.Add(time.Minute)
			seq := uint64(0)
			apply := func(kind string, at time.Time, payload any) {
				t.Helper()
				seq++
				if err := store.ApplyPage(ctx, "server-a", "node", telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "next", Events: []telemetry.Event{event(t, seq, kind, at, payload)}}); err != nil {
					t.Fatal(err)
				}
			}
			job := telemetry.JobEvent{Source: "authorized_process", Mode: "user", ExecutionID: "job-a", AuthorizationID: "shared", TokenMode: "managed", Holder: "alice", GPUs: []int{0}, StartedAt: &start}
			apply(telemetry.EventJobStarted, start, job)
			if sibling {
				other := job
				other.ExecutionID = "job-b"
				apply(telemetry.EventJobStarted, start, other)
			}
			apply(telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{GroupID: "reserved", Holder: "alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour), Members: []telemetry.ReservationMember{{ReservationID: "r0", GPU: 0}}})
			job.GroupID = "reserved"
			apply(telemetry.EventJobUpdated, start.Add(time.Second), job)
			job.FinishedAt = &finish
			apply(telemetry.EventJobFinished, finish, job)
			claim, err := store.GetSession(ctx, sessionID("node-a", "claimed-auth:shared"))
			if err != nil {
				t.Fatal(err)
			}
			if sibling {
				if claim.Status != "active" {
					t.Fatalf("unfinished sibling closed: %+v", claim)
				}
				otherFinish := finish.Add(time.Second)
				other := telemetry.JobEvent{Source: "authorized_process", Mode: "user", ExecutionID: "job-b", AuthorizationID: "shared", TokenMode: "managed", Holder: "alice", GPUs: []int{0}, StartedAt: &start, FinishedAt: &otherFinish}
				apply(telemetry.EventJobFinished, otherFinish, other)
				claim, err = store.GetSession(ctx, claim.ID)
				if err != nil {
					t.Fatal(err)
				}
				finish = otherFinish
			}
			if claim.Status != "completed" || claim.FinalizedAt == nil || !claim.FinalizedAt.Equal(finish) {
				t.Fatalf("linked claim not finalized: %+v", claim)
			}
		})
	}
}

func TestClaimedAuthorizationReuseDoesNotBlockTelemetry(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	finish := start.Add(time.Second)
	job := telemetry.JobEvent{Source: "authorized_process", Mode: "user", ExecutionID: "old", AuthorizationID: "shared", TokenMode: "managed", Holder: "alice", GPUs: []int{0}, StartedAt: &start}
	page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "before"}
	page.Events = append(page.Events, event(t, 1, telemetry.EventJobStarted, start, job))
	page.Events = append(page.Events, event(t, 2, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{GroupID: "reserved", Holder: "alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Minute)}))
	job.GroupID = "reserved"
	job.FinishedAt = &finish
	page.Events = append(page.Events, event(t, 3, telemetry.EventJobFinished, finish, job))
	if err := store.ApplyPage(ctx, "server-a", "node", page); err != nil {
		t.Fatal(err)
	}
	later := start.Add(2 * time.Minute)
	job = telemetry.JobEvent{Source: "authorized_process", Mode: "user", ExecutionID: "new", AuthorizationID: "shared", TokenMode: "managed", Holder: "alice", GPUs: []int{0}, StartedAt: &later}
	page.NextCursor = "after"
	page.Events = []telemetry.Event{event(t, 4, telemetry.EventJobStarted, later, job)}
	if err := store.ApplyPage(ctx, "server-a", "node", page); err != nil {
		t.Fatalf("valid new job blocked ingestion: %v", err)
	}
	if err := store.ApplyPage(ctx, "server-a", "node", page); err != nil {
		t.Fatalf("duplicate page: %v", err)
	}
	cursor, err := store.SyncCursor(ctx, "server-a")
	if err != nil || cursor != "after" {
		t.Fatalf("cursor = %q, %v", cursor, err)
	}
	claim, err := store.GetSession(ctx, sessionID("node-a", "claimed-auth:shared"))
	if err != nil {
		t.Fatal(err)
	}
	if claim.Status != "active" || claim.JobCount != 2 || !claim.StartsAt.Equal(start) || !claim.ExpiresAt.After(claim.StartsAt) {
		t.Fatalf("reused claim: %+v", claim)
	}
}

func TestClaimedSubMillisecondWindow(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Now().UTC().Truncate(time.Millisecond)
	observed := start.Add(time.Microsecond)
	page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", Events: []telemetry.Event{event(t, 1, telemetry.EventJobStarted, observed, telemetry.JobEvent{Source: "authorized_process", Mode: "user", ExecutionID: "tiny", Holder: "alice", TokenMode: "claimed", StartedAt: &start})}}
	if err := store.ApplyPage(context.Background(), "server-a", "node", page); err != nil {
		t.Fatal(err)
	}
}

func TestClaimedLifecycleMigrationRepairsOnlyFinished(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	finish := start.Add(time.Minute)
	seq := uint64(0)
	apply := func(kind string, at time.Time, payload any) {
		t.Helper()
		seq++
		if err := store.ApplyPage(ctx, "server-a", "node", telemetry.Page{NodeID: "node-a", StreamID: "stream-a", Events: []telemetry.Event{event(t, seq, kind, at, payload)}}); err != nil {
			t.Fatal(err)
		}
	}
	apply(telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{GroupID: "reserved", Holder: "alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour)})
	for _, auth := range []string{"finished", "mixed"} {
		job := telemetry.JobEvent{Source: "authorized_process", Mode: "user", ExecutionID: auth, AuthorizationID: auth, Holder: "alice", TokenMode: "managed", StartedAt: &start, GPUs: []int{0}}
		apply(telemetry.EventJobStarted, start, job)
		if auth == "mixed" {
			other := job
			other.ExecutionID = "sibling"
			apply(telemetry.EventJobStarted, start, other)
		}
		job.GroupID = "reserved"
		job.FinishedAt = &finish
		apply(telemetry.EventJobFinished, finish, job)
	}
	// Simulate v8's stale aggregate and force the new data repair on reopen.
	if _, err := store.DB().Exec("UPDATE reservation_sessions SET finalized_at_ms=NULL,expires_at_ms=starts_at_ms+1 WHERE kind='claimed_run'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO reservation_sessions(session_id,node_id,server_id,server_name,group_id,kind,owner_username,source,created_at_ms,starts_at_ms,expires_at_ms,updated_at_ms)
	VALUES('no-jobs','node-a','server-a','node','no-jobs','claimed_run','alice','cli',?,?,?,?)`, millis(start), millis(start), millis(finish), millis(start)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("DELETE FROM schema_migrations WHERE version=9"); err != nil {
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
	for _, auth := range []string{"finished", "mixed", "no-jobs"} {
		id := sessionID("node-a", "claimed-auth:"+auth)
		if auth == "no-jobs" {
			id = auth
		}
		session, err := store.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want := "active"
		if auth == "finished" {
			want = "completed"
		}
		if session.Status != want {
			t.Fatalf("%s status=%s, want %s", auth, session.Status, want)
		}
		if auth == "finished" && (session.FinalizedAt == nil || !session.FinalizedAt.Equal(finish) || session.JobCount != 1) {
			t.Fatalf("repaired session=%+v", session)
		}
	}
}
