package web

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gpuardian/internal/model"
)

// flakySnapshotClient fails snapshots while fail is set.
type flakySnapshotClient struct {
	*authzNodeClient
	fail atomic.Bool
}

func (c *flakySnapshotClient) Snapshot(ctx context.Context, server ServerRecord) (model.NodeSnapshot, error) {
	if c.fail.Load() {
		return model.NodeSnapshot{}, context.DeadlineExceeded
	}
	return c.authzNodeClient.Snapshot(ctx, server)
}

func TestFleetKeepsLastSnapshotThroughBriefFailures(t *testing.T) {
	server, base, serverID := newAuthzServer(t)
	client := &flakySnapshotClient{authzNodeClient: base}
	server.Client = client
	fetchedAt := time.Unix(5000, 0).UTC()
	now := fetchedAt
	server.now = func() time.Time { return now }
	node := func(t *testing.T) serverSnapshot {
		t.Helper()
		out, err := server.fetchFleetSnapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range out.Servers {
			if item.Server.ID == serverID {
				return item
			}
		}
		t.Fatal("node missing from fleet")
		return serverSnapshot{}
	}

	if item := node(t); !item.Online || item.Stale || item.Snapshot == nil {
		t.Fatalf("fresh item = online %v stale %v snapshot %v", item.Online, item.Stale, item.Snapshot != nil)
	}

	client.fail.Store(true)
	now = fetchedAt.Add(fleetNodeStaleGrace - time.Second)
	item := node(t)
	if !item.Online || !item.Stale || item.Snapshot == nil || item.Error == "" || !item.SnapshotAt.Equal(fetchedAt) {
		t.Fatalf("item within grace = online %v stale %v snapshot %v error %q at %s", item.Online, item.Stale, item.Snapshot != nil, item.Error, item.SnapshotAt)
	}

	now = fetchedAt.Add(fleetNodeStaleGrace + time.Second)
	if item := node(t); item.Online || item.Stale || item.Snapshot != nil || item.Error == "" {
		t.Fatalf("item after grace = online %v stale %v snapshot %v error %q", item.Online, item.Stale, item.Snapshot != nil, item.Error)
	}

	// Archiving forgets the kept snapshot.
	client.fail.Store(false)
	node(t)
	if _, err := server.Registry.SetArchived(serverID, true); err != nil {
		t.Fatal(err)
	}
	node(t)
	if _, err := server.Registry.SetArchived(serverID, false); err != nil {
		t.Fatal(err)
	}
	client.fail.Store(true)
	if item := node(t); item.Stale || item.Snapshot != nil {
		t.Fatal("snapshot kept across archiving")
	}
}
