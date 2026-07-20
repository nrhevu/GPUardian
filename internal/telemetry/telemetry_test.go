package telemetry

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOutboxReplayAndCursor(t *testing.T) {
	root := t.TempDir()
	nodePath := filepath.Join(root, "node.id")
	dir := filepath.Join(root, "outbox")
	box, err := Open(nodePath, dir, "boot-a")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := box.Append(EventDaemonStarted, map[string]int{"index": i}, time.Unix(int64(i+1), 0)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := box.Page("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 2 || !first.HasMore || first.Events[0].Seq != 1 || first.Events[1].Seq != 2 {
		t.Fatalf("unexpected first page: %#v", first)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(nodePath, dir, "boot-b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Info().NodeID != first.NodeID || reopened.Info().StreamID != first.StreamID {
		t.Fatalf("identity changed after reopen: %#v", reopened.Info())
	}
	second, err := reopened.Page(first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].Seq != 3 || second.HasMore {
		t.Fatalf("unexpected replay page: %#v", second)
	}
	if _, err := reopened.Append(EventDaemonStarted, struct{}{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	latest, err := reopened.Page(second.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Events) != 1 || latest.Events[0].Seq != 4 {
		t.Fatalf("sequence did not continue: %#v", latest)
	}
}

func TestOutboxRejectsCursorFromAnotherStream(t *testing.T) {
	root := t.TempDir()
	first, err := Open(filepath.Join(root, "node.id"), filepath.Join(root, "one"), "boot")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Append(EventDaemonStarted, struct{}{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	page, err := first.Page("", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(filepath.Join(root, "node.id"), filepath.Join(root, "two"), "boot")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Append(EventDaemonStarted, struct{}{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err = second.Page(page.NextCursor, 1)
	var gap *CursorGap
	if !errors.As(err, &gap) || gap.Code != "telemetry_cursor_gap" || gap.Reason != "stream_reset" || gap.ResumeCursor == "" {
		t.Fatalf("expected stream reset gap, got %v", err)
	}
}

func TestOutboxReportsRetentionGap(t *testing.T) {
	root := t.TempDir()
	box, err := Open(filepath.Join(root, "node.id"), filepath.Join(root, "outbox"), "boot")
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	for index := 0; index < 5; index++ {
		if _, err := box.Append(EventDaemonStarted, map[string]int{"index": index}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	box.mu.Lock()
	box.events = box.events[3:]
	box.mu.Unlock()
	oldCursor := encodeCursor(cursor{Version: SchemaVersion, StreamID: box.Info().StreamID, Seq: 1})
	_, err = box.Page(oldCursor, 1)
	var gap *CursorGap
	if !errors.As(err, &gap) || gap.Reason != "retention" || gap.EarliestSeq != 4 || gap.ResumeCursor == "" {
		t.Fatalf("expected retention gap, got %#v, %v", gap, err)
	}
}
