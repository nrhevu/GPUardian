package web

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuardian/internal/history"
	"gpuardian/internal/telemetry"
)

// readEvent returns the next `event:` name on the stream, skipping comments.
func readEvent(t *testing.T, lines *bufio.Scanner, within time.Duration) string {
	t.Helper()
	found := make(chan string, 1)
	go func() {
		for lines.Scan() {
			if name, ok := strings.CutPrefix(lines.Text(), "event: "); ok {
				found <- name
				return
			}
		}
		close(found)
	}()
	select {
	case name, ok := <-found:
		if !ok {
			t.Fatalf("event stream ended: %v", lines.Err())
		}
		return name
	case <-time.After(within):
		t.Fatalf("no event within %s", within)
		return ""
	}
}

func TestEventStreamSignalsFleetChanges(t *testing.T) {
	server, _, _ := newAuthzServer(t)
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()
	defer server.events.close()

	unauthenticated, err := http.Get(httpServer.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticated.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(testSessionCookie(t, server, "alice", RoleUser))
	// Event streams must bypass the gzip middleware's buffering.
	req.Header.Set("Accept-Encoding", "gzip")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Content-Encoding") != "" {
		t.Fatalf("stream status=%d type=%q encoding=%q", response.StatusCode, response.Header.Get("Content-Type"), response.Header.Get("Content-Encoding"))
	}
	lines := bufio.NewScanner(response.Body)
	if name := readEvent(t, lines, 2*time.Second); name != "ready" {
		t.Fatalf("first event = %q, want ready", name)
	}
	// A burst of changes arrives as one event.
	for range 5 {
		server.clearFleetCache()
	}
	if name := readEvent(t, lines, 3*time.Second); name != "fleet" {
		t.Fatalf("event after change = %q, want fleet", name)
	}
}

func TestFleetEventsCapsStreamsPerUserAndCloses(t *testing.T) {
	events := newFleetEvents()
	var releases []func()
	for range maxEventStreamsUser {
		_, release, ok := events.subscribe("Alice")
		if !ok {
			t.Fatal("stream under the per-user cap was refused")
		}
		releases = append(releases, release)
	}
	if _, _, ok := events.subscribe("alice"); ok {
		t.Fatal("stream over the per-user cap was accepted")
	}
	if _, release, ok := events.subscribe("bob"); !ok {
		t.Fatal("another user's stream was refused")
	} else {
		release()
	}
	releases[0]()
	releases[0]()
	sub, release, ok := events.subscribe("alice")
	if !ok {
		t.Fatal("stream after a release was refused")
	}
	defer release()
	events.publish()
	events.publish()
	if len(sub) != 1 {
		t.Fatalf("pending notifications = %d, want 1", len(sub))
	}
	events.close()
	if _, _, ok := events.subscribe("carol"); ok {
		t.Fatal("stream accepted after close")
	}
	select {
	case <-events.done:
	default:
		t.Fatal("done not closed")
	}
}

// waitTestNodeClient reports whether collection long-polled or polled.
type waitTestNodeClient struct {
	*authzNodeClient
	capabilities []string
	waits        chan time.Duration
	polls        chan struct{}
	infoErr      error
}

func (c *waitTestNodeClient) Info(context.Context, ServerRecord) (telemetry.Info, error) {
	if c.infoErr != nil {
		return telemetry.Info{}, c.infoErr
	}
	return telemetry.Info{NodeID: "node_wait", StreamID: "stream_wait", TelemetrySchema: telemetry.SchemaVersion, Capabilities: c.capabilities}, nil
}

func (c *waitTestNodeClient) Telemetry(context.Context, ServerRecord, string, int) (telemetry.Page, error) {
	c.polls <- struct{}{}
	return telemetry.Page{SchemaVersion: telemetry.SchemaVersion, NodeID: "node_wait", StreamID: "stream_wait"}, nil
}

func (c *waitTestNodeClient) TelemetryWait(_ context.Context, _ ServerRecord, _ string, _ int, wait time.Duration) (telemetry.Page, error) {
	c.waits <- wait
	return telemetry.Page{SchemaVersion: telemetry.SchemaVersion, NodeID: "node_wait", StreamID: "stream_wait"}, nil
}

func TestCollectNodeHistoryLongPollsOnlyCapableNodes(t *testing.T) {
	server, base, serverID := newAuthzServer(t)
	historyStore, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = historyStore.Close() })
	server.History = historyStore
	record, _, err := server.Registry.Get(serverID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name         string
		capabilities []string
		wantWait     bool
	}{
		{name: "capable node", capabilities: []string{"telemetry_v1", telemetry.CapabilityTelemetryWait}, wantWait: true},
		{name: "older node", capabilities: []string{"telemetry_v1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &waitTestNodeClient{authzNodeClient: base, capabilities: tt.capabilities, waits: make(chan time.Duration, 1), polls: make(chan struct{}, 1)}
			server.forgetNodeInfo(record.ID)
			result, err := server.collectNodeHistory(context.Background(), client, record, nil, historyLongPollWait)
			if err != nil {
				t.Fatal(err)
			}
			waited, polled := len(client.waits) == 1, len(client.polls) == 1
			if result.longPolled != tt.wantWait || waited != tt.wantWait || polled == tt.wantWait {
				t.Fatalf("longPolled=%v waits=%d polls=%d, want long poll %v", result.longPolled, len(client.waits), len(client.polls), tt.wantWait)
			}
			if tt.wantWait {
				if wait := <-client.waits; wait != historyLongPollWait {
					t.Fatalf("wait = %s, want %s", wait, historyLongPollWait)
				}
			}
		})
	}
}

func TestPageChangesFleetIgnoresGPUSamples(t *testing.T) {
	samples := telemetry.Page{Events: []telemetry.Event{{Type: telemetry.EventGPUSample}, {Type: telemetry.EventGPUSample}}}
	if pageChangesFleet(samples) {
		t.Fatal("GPU samples alone changed the fleet")
	}
	samples.Events = append(samples.Events, telemetry.Event{Type: telemetry.EventJobStarted})
	if !pageChangesFleet(samples) {
		t.Fatal("a job event did not change the fleet")
	}
}

func TestHistoryWorkersFollowActiveNodes(t *testing.T) {
	server, base, liveID := newAuthzServer(t)
	historyStore, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = historyStore.Close() })
	server.History = historyStore
	server.Client = &waitTestNodeClient{authzNodeClient: base, infoErr: errors.New("node unreachable")}
	retired, err := server.Registry.Upsert(ServerRecord{Name: "retired", Endpoint: "https://retired:8192", RootKey: "rk_retired"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Registry.SetArchived(retired.ID, true); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workers := make(map[string]historyWorker)
	server.superviseHistoryWorkers(ctx, workers)
	if _, ok := workers[liveID]; !ok || len(workers) != 1 {
		t.Fatalf("workers = %v, want only the live node", workers)
	}
	live := workers[liveID]

	if _, err := server.Registry.SetArchived(liveID, true); err != nil {
		t.Fatal(err)
	}
	server.superviseHistoryWorkers(ctx, workers)
	if len(workers) != 0 {
		t.Fatalf("workers after archiving = %v, want none", workers)
	}
	select {
	case <-live.done:
	case <-time.After(3 * time.Second):
		t.Fatal("archived node's worker did not stop")
	}
}
