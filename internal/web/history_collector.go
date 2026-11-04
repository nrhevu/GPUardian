package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gpuardian/internal/telemetry"
)

const (
	// historyPollInterval paces the collector's check for added, removed or
	// archived nodes, the retry backoff, and a node that answers a long poll
	// with an empty page too quickly.
	historyPollInterval = 2 * time.Second
	// historyLongPollWait is how long a node may hold an empty telemetry page.
	historyLongPollWait = telemetry.MaxPageWait
	// historyMinLongPoll is the shortest held request treated as a real wait;
	// an empty page returned sooner waits historyPollInterval before the next.
	historyMinLongPoll = time.Second
	// Node identity and capabilities rarely change; the collector asks every
	// node for telemetry several times a minute, so reuse Info briefly and
	// refetch it on any failure.
	nodeInfoCacheTTL = 30 * time.Second
	// Retention pruning runs in bounded batches outside the ingest path.
	historyPruneInterval = time.Hour
	historyPruneTimeout  = 5 * time.Minute
)

type nodeInfoEntry struct {
	endpoint string
	info     telemetry.Info
	at       time.Time
}

type historySyncState struct {
	failures int
	nextTry  time.Time
}

type historyWorker struct {
	endpoint string
	cancel   context.CancelFunc
	done     chan struct{}
}

func (s *Server) runHistoryCollector(ctx context.Context) {
	ticker := time.NewTicker(historyPollInterval)
	defer ticker.Stop()
	refreshed := make(map[string]string)
	retryAfter := make(map[string]time.Time)
	workers := make(map[string]historyWorker)
	defer func() {
		for _, worker := range workers {
			worker.cancel()
		}
	}()
	for {
		s.superviseHistoryWorkers(ctx, workers)
		s.refreshHistorySummaries(ctx, refreshed, retryAfter)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// superviseHistoryWorkers keeps one collector per active node, so a slow,
// unreachable or long-polling node never delays the others.
func (s *Server) superviseHistoryWorkers(ctx context.Context, workers map[string]historyWorker) {
	if s.History == nil {
		return
	}
	client, ok := s.Client.(TelemetryNodeAPI)
	if !ok {
		return
	}
	records, err := s.Registry.List()
	if err != nil {
		return
	}
	active := make(map[string]ServerRecord, len(records))
	for _, record := range records {
		if !record.Archived {
			active[record.ID] = record
		}
	}
	for id, worker := range workers {
		exited := false
		select {
		case <-worker.done:
			exited = true
		default:
		}
		if record, ok := active[id]; exited || !ok || record.Endpoint != worker.endpoint {
			worker.cancel()
			delete(workers, id)
		}
	}
	for id, record := range active {
		if _, running := workers[id]; running {
			continue
		}
		workerCtx, cancel := context.WithCancel(ctx)
		worker := historyWorker{endpoint: record.Endpoint, cancel: cancel, done: make(chan struct{})}
		workers[id] = worker
		go func() {
			defer close(worker.done)
			s.runNodeHistory(workerCtx, client, id)
		}()
	}
}

// runNodeHistory follows one node's telemetry stream; the node holds each
// request open until events arrive.
func (s *Server) runNodeHistory(ctx context.Context, client TelemetryNodeAPI, id string) {
	for ctx.Err() == nil {
		if delay := s.historyRetryDelay(id, time.Now()); delay > 0 {
			if !sleepContext(ctx, delay) {
				return
			}
			continue
		}
		record, found, err := s.Registry.Get(id)
		if err != nil || !found || record.Archived {
			return
		}
		started := time.Now()
		events, err := s.collectNodeHistory(ctx, client, record, s.historyOwners(), historyLongPollWait)
		if err != nil {
			s.forgetNodeInfo(record.ID)
		}
		s.recordHistoryAttempt(record.ID, err, time.Now())
		if err == nil && events == 0 && time.Since(started) < historyMinLongPoll {
			if !sleepContext(ctx, historyPollInterval) {
				return
			}
		}
	}
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Server) runHistoryRetention(ctx context.Context) {
	if s.History == nil {
		return
	}
	ticker := time.NewTicker(historyPruneInterval)
	defer ticker.Stop()
	for {
		pruneCtx, cancel := context.WithTimeout(ctx, historyPruneTimeout)
		_ = s.History.PruneRetention(pruneCtx, time.Now())
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) refreshHistorySummaries(ctx context.Context, refreshed map[string]string, retryAfter map[string]time.Time) {
	if s.History == nil {
		return
	}
	now := s.now().UTC()
	day := now.Format("2006-01-02")
	records, err := s.Registry.List()
	if err != nil {
		return
	}
	for _, record := range records {
		if refreshed[record.ID] == day || now.Before(retryAfter[record.ID]) {
			continue
		}
		if _, err := s.History.RefreshDailySummary(ctx, record.ID, now); err != nil {
			retryAfter[record.ID] = now.Add(time.Minute)
			continue
		}
		refreshed[record.ID] = day
		delete(retryAfter, record.ID)
	}
}

func (s *Server) collectHistory(ctx context.Context) {
	if s.History == nil {
		return
	}
	client, ok := s.Client.(TelemetryNodeAPI)
	if !ok {
		return
	}
	records, err := s.Registry.List()
	if err != nil {
		return
	}
	owners := s.historyOwners()
	var wg sync.WaitGroup
	for _, record := range records {
		record := record
		if record.Archived || !s.historyNodeReady(record.ID, time.Now()) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			nodeCtx, cancel := context.WithTimeout(ctx, fleetNodeTimeout)
			defer cancel()
			_, err := s.collectNodeHistory(nodeCtx, client, record, owners, 0)
			if err != nil {
				s.forgetNodeInfo(record.ID)
			}
			s.recordHistoryAttempt(record.ID, err, time.Now())
		}()
	}
	wg.Wait()
}

func (s *Server) historyOwners() map[string]bool {
	owners := make(map[string]bool)
	if users, err := s.Users.List(); err == nil {
		for _, user := range users {
			owners[strings.ToLower(strings.TrimSpace(user.Username))] = true
		}
	}
	return owners
}

// errNodeTelemetryOutdated rejects node daemons from before telemetry
// long-polling; the gateway no longer polls their telemetry.
var errNodeTelemetryOutdated = fmt.Errorf("node daemon must be upgraded: it does not support telemetry long-polling (%s)", telemetry.CapabilityTelemetryWait)

// requireNodeTelemetryWait refuses to register a node whose daemon predates
// telemetry long-polling.
func (s *Server) requireNodeTelemetryWait(ctx context.Context, record ServerRecord) error {
	client, ok := s.Client.(TelemetryNodeAPI)
	if !ok {
		return nil // Test adapters opt out; the production client always implements this API.
	}
	info, err := client.Info(ctx, record)
	if err != nil {
		return fmt.Errorf("query node telemetry capability: %w", err)
	}
	if !telemetryCapability(info, telemetry.CapabilityTelemetryWait) {
		return errNodeTelemetryOutdated
	}
	return nil
}

// collectNodeHistory applies the node's pending telemetry pages and returns
// how many events they held. With wait > 0 the first page is long-polled.
// Node calls carry the node client's own timeouts.
func (s *Server) collectNodeHistory(ctx context.Context, client TelemetryNodeAPI, record ServerRecord, knownOwners map[string]bool, wait time.Duration) (int, error) {
	events := 0
	infoCtx, cancel := context.WithTimeout(ctx, fleetNodeTimeout)
	info, infoCached, err := s.cachedNodeInfo(infoCtx, client, record)
	cancel()
	if err != nil {
		return events, fmt.Errorf("node info: %w", err)
	}
	if info.NodeID == "" {
		return events, errors.New("node info is missing node_id")
	}
	if info.TelemetrySchema < 1 || !telemetryCapability(info, "telemetry_v1") {
		return events, errors.New("node does not support telemetry history")
	}
	if !telemetryCapability(info, telemetry.CapabilityTelemetryWait) {
		return events, errNodeTelemetryOutdated
	}
	cursor, err := s.History.SyncCursor(ctx, record.ID)
	if err != nil {
		return events, err
	}
	for pages := 0; pages < 32; pages++ {
		pageWait := wait
		if pages > 0 {
			pageWait = 0
		}
		page, err := client.Telemetry(ctx, record, cursor, telemetry.MaxPageLimit, pageWait)
		if err != nil {
			var gap *TelemetryGapError
			if !errors.As(err, &gap) {
				return events, err
			}
			if err := s.History.MarkGap(ctx, record.ID, gap.Gap.Reason); err != nil {
				return events, err
			}
			cursor = gap.Gap.ResumeCursor
			continue
		}
		if page.NodeID != info.NodeID && page.StreamID != "" && infoCached {
			// A cached identity may predate a node replacement; confirm it
			// before recording a gap.
			s.forgetNodeInfo(record.ID)
			infoCtx, cancel := context.WithTimeout(ctx, fleetNodeTimeout)
			info, infoCached, err = s.cachedNodeInfo(infoCtx, client, record)
			cancel()
			if err != nil {
				return events, fmt.Errorf("node info: %w", err)
			}
		}
		if page.NodeID != info.NodeID || page.StreamID == "" {
			_ = s.History.MarkGap(ctx, record.ID, "node identity changed")
			return events, errors.New("telemetry node identity changed")
		}
		if err := s.History.ApplyPageWithOwners(ctx, record.ID, record.Name, page, knownOwners); err != nil {
			return events, err
		}
		events += len(page.Events)
		if pageChangesFleet(page) {
			// The node's reservations, authorizations or jobs moved on; drop
			// the cached fleet view and tell event-stream clients.
			s.clearFleetCache()
		}
		cursor = page.NextCursor
		if !page.HasMore {
			return events, nil
		}
	}
	return events, nil
}

// pageChangesFleet reports whether a page holds events other than periodic GPU
// samples, which the fleet view shows but which change every few seconds.
func pageChangesFleet(page telemetry.Page) bool {
	for _, event := range page.Events {
		if event.Type != telemetry.EventGPUSample {
			return true
		}
	}
	return false
}

func (s *Server) cachedNodeInfo(ctx context.Context, client TelemetryNodeAPI, record ServerRecord) (telemetry.Info, bool, error) {
	now := time.Now()
	s.nodeInfoMu.Lock()
	entry, ok := s.nodeInfo[record.ID]
	s.nodeInfoMu.Unlock()
	if ok && entry.endpoint == record.Endpoint && now.Sub(entry.at) < nodeInfoCacheTTL {
		return entry.info, true, nil
	}
	info, err := client.Info(ctx, record)
	if err != nil {
		return telemetry.Info{}, false, err
	}
	s.nodeInfoMu.Lock()
	if s.nodeInfo == nil {
		s.nodeInfo = make(map[string]nodeInfoEntry)
	}
	s.nodeInfo[record.ID] = nodeInfoEntry{endpoint: record.Endpoint, info: info, at: now}
	s.nodeInfoMu.Unlock()
	return info, false, nil
}

func (s *Server) forgetNodeInfo(id string) {
	s.nodeInfoMu.Lock()
	delete(s.nodeInfo, id)
	s.nodeInfoMu.Unlock()
}

func telemetryCapability(info telemetry.Info, capability string) bool {
	for _, candidate := range info.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func (s *Server) historyNodeReady(id string, now time.Time) bool {
	s.historySyncMu.Lock()
	defer s.historySyncMu.Unlock()
	return !now.Before(s.historySync[id].nextTry)
}

func (s *Server) historyRetryDelay(id string, now time.Time) time.Duration {
	s.historySyncMu.Lock()
	defer s.historySyncMu.Unlock()
	return s.historySync[id].nextTry.Sub(now)
}

func (s *Server) recordHistoryAttempt(id string, err error, now time.Time) {
	s.historySyncMu.Lock()
	defer s.historySyncMu.Unlock()
	if err == nil {
		s.historySync[id] = historySyncState{}
		return
	}
	state := s.historySync[id]
	state.failures++
	delay := historyPollInterval * time.Duration(1<<min(state.failures-1, 5))
	if delay > time.Minute {
		delay = time.Minute
	}
	// Stable per-node jitter prevents a recovered fleet from reconnecting in lockstep.
	jitter := time.Duration((len(id)*37+state.failures*53)%501-250) * time.Millisecond
	state.nextTry = now.Add(delay + jitter)
	s.historySync[id] = state
}
