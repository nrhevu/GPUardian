package history

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gpuardian/internal/telemetry"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestPruneRetentionDeletesExpiredRowsInBatches(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	eventCutoff := now.Add(-ingestedEventRetention)
	rollupCutoff := now.Add(-minuteRollupRetention)
	for _, test := range []struct {
		name      string
		batchSize int
	}{
		{name: "one row per batch", batchSize: 1},
		{name: "partial last batch", batchSize: 3},
		{name: "exact multiple", batchSize: 7},
		{name: "default batch", batchSize: retentionBatchSize},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			ctx := context.Background()
			createSearchSession(t, store, "sess", "node-a", "server-a", "alice", "long", now.Add(-100*24*time.Hour), now, []int{0})
			// Seven expired rows per table; the cutoff itself is retained.
			for index := 0; index < 7; index++ {
				if _, err := store.DB().Exec("INSERT INTO ingested_event_ids(node_id,stream_id,seq,ingested_at_ms) VALUES('node-a','stream',?,?)",
					index, millis(eventCutoff.Add(-time.Duration(index+1)*time.Minute))); err != nil {
					t.Fatal(err)
				}
				minute := millis(rollupCutoff.Add(-time.Duration(index+1) * time.Minute))
				if _, err := store.DB().Exec("INSERT INTO gpu_minute_rollups(session_id,gpu,minute_ms) VALUES('sess',0,?)", minute); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec("INSERT INTO node_gpu_minute_rollups(node_id,gpu,minute_ms) VALUES('node-a',0,?)", minute); err != nil {
					t.Fatal(err)
				}
			}
			for index, at := range []time.Time{eventCutoff, now} {
				if _, err := store.DB().Exec("INSERT INTO ingested_event_ids(node_id,stream_id,seq,ingested_at_ms) VALUES('node-a','stream',?,?)", 100+index, millis(at)); err != nil {
					t.Fatal(err)
				}
			}
			for _, at := range []time.Time{rollupCutoff, now} {
				if _, err := store.DB().Exec("INSERT INTO gpu_minute_rollups(session_id,gpu,minute_ms) VALUES('sess',0,?)", millis(at)); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec("INSERT INTO node_gpu_minute_rollups(node_id,gpu,minute_ms) VALUES('node-a',0,?)", millis(at)); err != nil {
					t.Fatal(err)
				}
			}

			// Ingest no longer prunes on every commit.
			page := telemetry.Page{NodeID: "node-a", StreamID: "stream-live", NextCursor: "cursor-1", Events: []telemetry.Event{
				event(t, 1, telemetry.EventGap, now, telemetry.Gap{From: now}),
			}}
			if err := store.ApplyPage(ctx, "server-a", "node", page); err != nil {
				t.Fatal(err)
			}
			assertRetentionCounts(t, store, map[string]int{"ingested_event_ids": 10, "gpu_minute_rollups": 9, "node_gpu_minute_rollups": 9})

			if err := store.pruneRetention(ctx, now, test.batchSize); err != nil {
				t.Fatal(err)
			}
			// The page's own event row is new, so three dedupe keys remain.
			assertRetentionCounts(t, store, map[string]int{"ingested_event_ids": 3, "gpu_minute_rollups": 2, "node_gpu_minute_rollups": 2})
			if err := store.PruneRetention(ctx, now); err != nil {
				t.Fatal(err)
			}
			assertRetentionCounts(t, store, map[string]int{"ingested_event_ids": 3, "gpu_minute_rollups": 2, "node_gpu_minute_rollups": 2})
		})
	}
}

func TestPruneRetentionStopsWhenCancelled(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC()
	if _, err := store.DB().Exec("INSERT INTO ingested_event_ids(node_id,stream_id,seq,ingested_at_ms) VALUES('node-a','stream',1,?)", millis(now.Add(-72*time.Hour))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.PruneRetention(ctx, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled prune error = %v", err)
	}
	assertRetentionCounts(t, store, map[string]int{"ingested_event_ids": 1})
}

func assertRetentionCounts(t *testing.T, store *Store, want map[string]int) {
	t.Helper()
	for table, count := range want {
		var got int
		if err := store.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != count {
			t.Fatalf("%s rows = %d, want %d", table, got, count)
		}
	}
}

func TestApplyEmptyPageSkipsOnlyRedundantSyncWrites(t *testing.T) {
	const marker = 5 * time.Second
	for _, test := range []struct {
		name      string
		lastSync  time.Duration // offset of the stored last_sync_at_ms from now
		syncError string
		serverID  string
		streamID  string
		cursor    string
		wantWrite bool
	}{
		{name: "recent and unchanged", lastSync: -marker, wantWrite: false},
		{name: "refresh interval elapsed", lastSync: -idleSyncRefreshInterval - marker, wantWrite: true},
		{name: "clock moved backwards", lastSync: time.Minute, wantWrite: true},
		{name: "sync error is cleared", lastSync: -marker, syncError: "telemetry gap", wantWrite: true},
		{name: "cursor moved", lastSync: -marker, cursor: "cursor-2", wantWrite: true},
		{name: "stream restarted", lastSync: -marker, streamID: "stream-b", wantWrite: true},
		{name: "node registered as another server", lastSync: -marker, serverID: "server-b", wantWrite: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			ctx := context.Background()
			start := time.Now().UTC().Add(-time.Minute)
			page := telemetry.Page{NodeID: "node-a", StreamID: "stream-a", NextCursor: "cursor-1", Events: []telemetry.Event{
				event(t, 1, telemetry.EventReservationUpsert, start, telemetry.ReservationUpsert{
					GroupID: "group-a", Holder: "alice", CreatedAt: start, StartsAt: start, ExpiresAt: start.Add(time.Hour),
					Members: []telemetry.ReservationMember{{ReservationID: "r0", GPU: 0}},
				}),
			}}
			if err := store.ApplyPage(ctx, "server-a", "node", page); err != nil {
				t.Fatal(err)
			}
			stored := time.Now().UTC().Add(test.lastSync).UnixMilli()
			if _, err := store.DB().Exec("UPDATE node_sync_state SET last_sync_at_ms=?,sync_error=? WHERE server_id='server-a'", stored, test.syncError); err != nil {
				t.Fatal(err)
			}

			serverID := firstNonempty(test.serverID, "server-a")
			empty := telemetry.Page{NodeID: "node-a", StreamID: firstNonempty(test.streamID, "stream-a"), NextCursor: firstNonempty(test.cursor, "cursor-1")}
			before := time.Now().UTC().UnixMilli()
			if err := store.ApplyPage(ctx, serverID, "node", empty); err != nil {
				t.Fatal(err)
			}
			var lastSync int64
			var cursor, streamID, syncError, lastServer string
			if err := store.DB().QueryRow(`SELECT st.last_sync_at_ms,st.cursor,st.stream_id,st.sync_error,n.last_server_id
				FROM node_sync_state st JOIN nodes n ON n.node_id=st.node_id WHERE st.server_id=?`, serverID).
				Scan(&lastSync, &cursor, &streamID, &syncError, &lastServer); err != nil {
				t.Fatal(err)
			}
			if !test.wantWrite {
				if lastSync != stored {
					t.Fatalf("redundant empty page rewrote last_sync_at_ms: %d, want %d", lastSync, stored)
				}
				return
			}
			if lastSync < before || cursor != empty.NextCursor || streamID != empty.StreamID || syncError != "" || lastServer != serverID {
				t.Fatalf("sync state = last_sync %d (want >= %d) cursor %q stream %q error %q server %q",
					lastSync, before, cursor, streamID, syncError, lastServer)
			}
		})
	}
}

func TestReconcileOpenSessionsWithoutChangesDoesNotTakeWriteLock(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	createSearchSession(t, store, "live", "node-a", "server-a", "alice", "live", now.Add(-time.Hour), now.Add(time.Hour), []int{0})
	createSearchSession(t, store, "gone", "node-a", "server-a", "bob", "gone", now.Add(-time.Hour), now.Add(time.Hour), []int{1})

	store.writeMu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- store.ReconcileOpenSessions(ctx, "server-a", []string{"live-group", "gone-group"}, now)
	}()
	select {
	case err := <-done:
		store.writeMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		store.writeMu.Unlock()
		<-done
		t.Fatal("reconciliation with every session live waited for the write lock")
	}

	if err := store.ReconcileOpenSessions(ctx, "server-a", []string{"live-group"}, now); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"live": "active", "gone": "revoked"} {
		session, err := store.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if session.Status != want {
			t.Fatalf("%s status = %s, want %s", id, session.Status, want)
		}
	}
}
