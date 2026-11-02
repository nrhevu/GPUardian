package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"gpuardian/internal/history"
	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
	"gpuardian/internal/telemetry"
)

// archiveTestNodeClient records every node call so tests can prove that the
// gateway never contacts an archived node.
type archiveTestNodeClient struct {
	*authzNodeClient
	callMu sync.Mutex
	calls  map[string][]string
}

func (c *archiveTestNodeClient) record(server ServerRecord, method string) {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	if c.calls == nil {
		c.calls = make(map[string][]string)
	}
	c.calls[server.ID] = append(c.calls[server.ID], method)
}

func (c *archiveTestNodeClient) callsFor(id string) []string {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	return slices.Clone(c.calls[id])
}

func (c *archiveTestNodeClient) Snapshot(ctx context.Context, server ServerRecord) (model.NodeSnapshot, error) {
	c.record(server, "snapshot")
	return c.authzNodeClient.Snapshot(ctx, server)
}

func (c *archiveTestNodeClient) Info(_ context.Context, server ServerRecord) (telemetry.Info, error) {
	c.record(server, "info")
	return telemetry.Info{}, errors.New("telemetry unavailable")
}

func (c *archiveTestNodeClient) Telemetry(_ context.Context, server ServerRecord, _ string, _ int) (telemetry.Page, error) {
	c.record(server, "telemetry")
	return telemetry.Page{}, nil
}

func (c *archiveTestNodeClient) SyncManagedUserKeys(_ context.Context, server ServerRecord, snapshot protocol.ManagedUserKeySnapshot) (protocol.ManagedUserKeySyncResult, error) {
	c.record(server, "sync")
	return protocol.ManagedUserKeySyncResult{SnapshotID: snapshot.SnapshotID, Managed: true}, nil
}

func TestRegistrySetArchivedPersistsAndSurvivesRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	registry := NewRegistry(path, false)
	record, err := registry.Upsert(ServerRecord{Name: "node-a", Endpoint: "https://node-a:8192", RootKey: "rk_test"})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := registry.SetArchived(record.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !archived.Archived || !publicServerRecord(archived).Archived {
		t.Fatalf("archived record = %+v", archived)
	}
	if _, err := registry.Rename(record.ID, "node-a-old"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewRegistry(path, false).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 1 || !reloaded[0].Archived || reloaded[0].Name != "node-a-old" {
		t.Fatalf("reloaded records = %+v", reloaded)
	}
	restored, err := registry.SetArchived(record.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Archived {
		t.Fatalf("unarchived record = %+v", restored)
	}
	if _, err := registry.SetArchived("srv_missing", true); err == nil {
		t.Fatal("archiving a missing node succeeded")
	}
}

func TestArchivedNodeIsSkippedByFleetHistoryAndKeySync(t *testing.T) {
	server, base, liveID := newAuthzServer(t)
	historyStore, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = historyStore.Close() })
	server.History = historyStore
	client := &archiveTestNodeClient{authzNodeClient: base}
	server.Client = client
	archived, err := server.Registry.Upsert(ServerRecord{Name: "retired", Endpoint: "https://retired:8192", RootKey: "rk_retired"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Registry.SetArchived(archived.ID, true); err != nil {
		t.Fatal(err)
	}

	fleet, err := server.fetchFleetSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server.collectHistory(context.Background())
	server.syncManagedKeysAcrossFleet(context.Background(), true)

	if calls := client.callsFor(archived.ID); len(calls) != 0 {
		t.Fatalf("archived node was contacted: %v", calls)
	}
	if calls := client.callsFor(liveID); !slices.Contains(calls, "snapshot") || !slices.Contains(calls, "info") {
		t.Fatalf("live node calls = %v, want snapshot and info", calls)
	}
	var entry serverSnapshot
	for _, item := range fleet.Servers {
		if item.Server.ID == archived.ID {
			entry = item
		}
	}
	if !entry.Server.Archived || entry.Online || entry.Error != "" || entry.Snapshot != nil {
		t.Fatalf("archived fleet entry = %+v", entry)
	}
	for _, status := range server.managedKeyStatuses() {
		if status.NodeID == archived.ID && status.Status != "archived" {
			t.Fatalf("archived node key sync status = %q", status.Status)
		}
	}
}

func TestArchiveEndpointIsAdminOnlyAndBlocksNewWork(t *testing.T) {
	server, client, serverID := newAuthzServer(t)
	handler := server.routes()
	userCookie := testSessionCookie(t, server, "alice", RoleUser)
	adminCookie := testSessionCookie(t, server, "admin", RoleAdmin)
	path := "/api/servers/" + serverID

	if response := requestJSON(handler, http.MethodPost, path+"/archive", `{"archived":true}`, userCookie); response.Code != http.StatusForbidden {
		t.Fatalf("user archive status = %d, body=%s", response.Code, response.Body.String())
	}
	response := requestJSON(handler, http.MethodPost, path+"/archive", `{"archived":true}`, adminCookie)
	if response.Code != http.StatusOK {
		t.Fatalf("admin archive status = %d, body=%s", response.Code, response.Body.String())
	}
	var stored PublicServerRecord
	if err := json.Unmarshal(response.Body.Bytes(), &stored); err != nil || !stored.Archived {
		t.Fatalf("archive response = %s (%v)", response.Body.String(), err)
	}

	for _, action := range []string{"reservations", "allow"} {
		response := requestJSON(handler, http.MethodPost, path+"/"+action, `{}`, userCookie)
		if response.Code != http.StatusConflict {
			t.Fatalf("%s on archived node status = %d, body=%s", action, response.Code, response.Body.String())
		}
	}
	if client.lastReservation.Name != "" || len(client.allowed) != 0 {
		t.Fatalf("archived node received work: reservation=%+v allowed=%+v", client.lastReservation, client.allowed)
	}

	response = requestJSON(handler, http.MethodPost, path+"/archive", `{"archived":false}`, adminCookie)
	if response.Code != http.StatusOK {
		t.Fatalf("admin unarchive status = %d, body=%s", response.Code, response.Body.String())
	}
	record, _, err := server.Registry.Get(serverID)
	if err != nil || record.Archived {
		t.Fatalf("registry after unarchive = %+v (%v)", record, err)
	}
}
