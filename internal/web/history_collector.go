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

const historyPollInterval = 2 * time.Second

type historySyncState struct {
	failures int
	nextTry  time.Time
}

func (s *Server) runHistoryCollector(ctx context.Context) {
	ticker := time.NewTicker(historyPollInterval)
	defer ticker.Stop()
	refreshed := make(map[string]string)
	retryAfter := make(map[string]time.Time)
	for {
		s.collectHistory(ctx)
		s.refreshHistorySummaries(ctx, refreshed, retryAfter)
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
	owners := make(map[string]bool)
	if users, err := s.Users.List(); err == nil {
		for _, user := range users {
			owners[strings.ToLower(strings.TrimSpace(user.Username))] = true
		}
	}
	var wg sync.WaitGroup
	for _, record := range records {
		record := record
		if !s.historyNodeReady(record.ID, time.Now()) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.collectNodeHistory(ctx, client, record, owners)
			s.recordHistoryAttempt(record.ID, err, time.Now())
		}()
	}
	wg.Wait()
}

func (s *Server) collectNodeHistory(ctx context.Context, client TelemetryNodeAPI, record ServerRecord, knownOwners map[string]bool) error {
	nodeCtx, cancel := context.WithTimeout(ctx, fleetNodeTimeout)
	defer cancel()
	info, err := client.Info(nodeCtx, record)
	if err != nil {
		return fmt.Errorf("node info: %w", err)
	}
	if info.NodeID == "" {
		return errors.New("node info is missing node_id")
	}
	if info.TelemetrySchema < 1 || !telemetryCapability(info, "telemetry_v1") {
		return errors.New("node does not support telemetry history")
	}
	cursor, err := s.History.SyncCursor(nodeCtx, record.ID)
	if err != nil {
		return err
	}
	for pages := 0; pages < 32; pages++ {
		page, err := client.Telemetry(nodeCtx, record, cursor, telemetry.MaxPageLimit)
		if err != nil {
			var gap *TelemetryGapError
			if !errors.As(err, &gap) {
				return err
			}
			if err := s.History.MarkGap(nodeCtx, record.ID, gap.Gap.Reason); err != nil {
				return err
			}
			cursor = gap.Gap.ResumeCursor
			continue
		}
		if page.NodeID != info.NodeID || page.StreamID == "" {
			_ = s.History.MarkGap(nodeCtx, record.ID, "node identity changed")
			return errors.New("telemetry node identity changed")
		}
		if err := s.History.ApplyPageWithOwners(nodeCtx, record.ID, record.Name, page, knownOwners); err != nil {
			return err
		}
		cursor = page.NextCursor
		if !page.HasMore {
			return nil
		}
	}
	return nil
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
