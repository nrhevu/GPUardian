package telemetry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOutboxAppendBatchPersistsInOrder(t *testing.T) {
	root := t.TempDir()
	nodePath := filepath.Join(root, "node.id")
	dir := filepath.Join(root, "outbox")
	box, err := Open(nodePath, dir, "boot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Append(EventDaemonStarted, struct{}{}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	events, err := box.AppendBatch([]BatchEvent{
		{Type: EventJobStarted, Payload: map[string]int{"n": 1}, OccurredAt: time.Unix(2, 0)},
		{Type: EventJobUpdated, Payload: map[string]int{"n": 2}, OccurredAt: time.Unix(3, 0)},
		{Type: EventJobFinished, Payload: map[string]int{"n": 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Seq != 2 || events[2].Seq != 4 || events[2].OccurredAt.IsZero() {
		t.Fatalf("batch events = %+v", events)
	}
	if _, err := box.AppendBatch([]BatchEvent{{Type: EventJobStarted, Payload: func() {}}}); err == nil {
		t.Fatal("unencodable payload accepted")
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(nodePath, dir, "boot")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page, err := reopened.Page("", 10)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{EventDaemonStarted, EventJobStarted, EventJobUpdated, EventJobFinished}
	if len(page.Events) != len(wantTypes) {
		t.Fatalf("persisted events = %+v", page.Events)
	}
	for i, event := range page.Events {
		if event.Seq != uint64(i+1) || event.Type != wantTypes[i] {
			t.Fatalf("event %d = %+v", i, event)
		}
	}
}

func TestOutboxPageSeeksToCursor(t *testing.T) {
	root := t.TempDir()
	box, err := Open(filepath.Join(root, "node.id"), filepath.Join(root, "outbox"), "boot")
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	batch := make([]BatchEvent, 6)
	for i := range batch {
		batch[i] = BatchEvent{Type: EventDaemonStarted, Payload: i, OccurredAt: time.Now()}
	}
	if _, err := box.AppendBatch(batch); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		cursor   uint64
		limit    int
		wantSeqs []uint64
		wantMore bool
	}{
		{name: "from start", cursor: 0, limit: 2, wantSeqs: []uint64{1, 2}, wantMore: true},
		{name: "middle", cursor: 3, limit: 2, wantSeqs: []uint64{4, 5}, wantMore: true},
		{name: "tail", cursor: 5, limit: 10, wantSeqs: []uint64{6}},
		{name: "caught up", cursor: 6, limit: 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, err := box.Page(encodeCursor(cursor{Version: SchemaVersion, StreamID: box.Info().StreamID, Seq: tc.cursor}), tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Events) != len(tc.wantSeqs) || page.HasMore != tc.wantMore {
				t.Fatalf("page = %+v", page)
			}
			for i, event := range page.Events {
				if event.Seq != tc.wantSeqs[i] {
					t.Fatalf("seqs = %+v, want %v", page.Events, tc.wantSeqs)
				}
			}
		})
	}
}

func TestOutboxPrunesExpiredSegmentsByPrefix(t *testing.T) {
	root := t.TempDir()
	box, err := Open(filepath.Join(root, "node.id"), filepath.Join(root, "outbox"), "boot")
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	old := time.Now().Add(-48 * time.Hour)
	if _, err := box.AppendBatch([]BatchEvent{
		{Type: EventDaemonStarted, Payload: 1, OccurredAt: old},
		{Type: EventDaemonStarted, Payload: 2, OccurredAt: old},
	}); err != nil {
		t.Fatal(err)
	}
	box.mu.Lock()
	firstPath := box.segments[0].path
	firstSize := box.segments[0].size
	// Age the open segment so the next append rotates into a new one.
	box.segments[0].created = time.Now().Add(-2 * maxSegmentAge)
	box.mu.Unlock()
	if _, err := box.Append(EventDaemonStarted, 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	box.mu.Lock()
	defer box.mu.Unlock()
	if len(box.segments) != 1 || box.segments[0].first != 3 {
		t.Fatalf("segments = %+v, want only the live segment", box.segments)
	}
	if len(box.events) != 1 || box.events[0].Seq != 3 {
		t.Fatalf("events = %+v, want the expired segment's events dropped", box.events)
	}
	if box.totalBytes != box.segments[0].size || firstSize == 0 {
		t.Fatalf("total bytes = %d, segment size = %d", box.totalBytes, box.segments[0].size)
	}
	if _, err := os.Stat(firstPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired segment file still present: %v", err)
	}
}
