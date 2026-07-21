package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gpuardian/internal/protocol"
)

const (
	managedKeyRetryMin = 2 * time.Second
	managedKeyRetryMax = 60 * time.Second
	managedKeyFullSync = 5 * time.Minute
)

type managedKeyNodeState struct {
	SnapshotID string
	SyncedAt   time.Time
	LastError  string
	NextTry    time.Time
	Backoff    time.Duration
}

type ManagedKeyNodeStatus struct {
	NodeID     string    `json:"node_id"`
	Node       string    `json:"node"`
	Status     string    `json:"status"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	SyncedAt   time.Time `json:"synced_at,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func (s *Server) managedKeySnapshot() (protocol.ManagedUserKeySnapshot, error) {
	verifiers, err := s.Users.FixedKeyVerifiers()
	if err != nil {
		return protocol.ManagedUserKeySnapshot{}, err
	}
	keys := make([]protocol.ManagedUserKey, 0, len(verifiers))
	for _, key := range verifiers {
		keys = append(keys, protocol.ManagedUserKey{ID: key.ID, Owner: key.Owner, Version: key.Version, Hash: key.Hash})
	}
	raw, err := json.Marshal(keys)
	if err != nil {
		return protocol.ManagedUserKeySnapshot{}, err
	}
	digest := sha256.Sum256(raw)
	return protocol.ManagedUserKeySnapshot{SnapshotID: "sha256:" + hex.EncodeToString(digest[:]), Keys: keys}, nil
}

func (s *Server) syncManagedKeysToNode(ctx context.Context, record ServerRecord) error {
	err := s.syncManagedKeysToNodeOnce(ctx, record)
	s.recordManagedKeySync(record.ID, err)
	return err
}

func (s *Server) syncManagedKeysToNodeOnce(ctx context.Context, record ServerRecord) error {
	client, ok := s.Client.(ManagedKeyNodeAPI)
	if !ok {
		return nil // Test/legacy adapters opt out; the production client always implements this API.
	}
	infoClient, ok := s.Client.(TelemetryNodeAPI)
	if !ok {
		return errors.New("node client cannot verify managed user-key capability")
	}
	info, err := infoClient.Info(ctx, record)
	if err != nil {
		return fmt.Errorf("query managed user-key capability: %w", err)
	}
	if !telemetryCapability(info, "managed_user_keys_v1") {
		return errors.New("node daemon must be upgraded for fixed user keys")
	}
	snapshot, err := s.managedKeySnapshot()
	if err != nil {
		return err
	}
	result, err := client.SyncManagedUserKeys(ctx, record, snapshot)
	if err != nil {
		return fmt.Errorf("sync fixed user keys to %s: %w", record.Name, err)
	}
	if !result.Managed || result.SnapshotID != snapshot.SnapshotID {
		return errors.New("node did not confirm the current fixed-key snapshot")
	}
	return nil
}

func (s *Server) requestManagedKeySync() {
	select {
	case s.managedKeySync <- struct{}{}:
	default:
	}
}

func (s *Server) runManagedKeySync(ctx context.Context) {
	ticker := time.NewTicker(managedKeyRetryMin)
	defer ticker.Stop()
	force := true
	for {
		s.syncManagedKeysAcrossFleet(ctx, force)
		force = false
		select {
		case <-ctx.Done():
			return
		case <-s.managedKeySync:
			force = true
		case <-ticker.C:
		}
	}
}

func (s *Server) syncManagedKeysAcrossFleet(ctx context.Context, force bool) {
	records, err := s.Registry.List()
	if err != nil {
		return
	}
	for _, record := range records {
		if ctx.Err() != nil {
			return
		}
		if !force && !s.managedKeySyncDue(record.ID) {
			continue
		}
		nodeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		_ = s.syncManagedKeysToNode(nodeCtx, record)
		cancel()
	}
}

func (s *Server) recordManagedKeySync(nodeID string, syncErr error) {
	now := time.Now().UTC()
	snapshot, snapshotErr := s.managedKeySnapshot()
	s.managedKeyMu.Lock()
	if s.managedKeyNode == nil {
		s.managedKeyNode = make(map[string]managedKeyNodeState)
	}
	state := s.managedKeyNode[nodeID]
	if syncErr == nil && snapshotErr == nil {
		state.SnapshotID = snapshot.SnapshotID
		state.SyncedAt = now
		state.LastError = ""
		state.Backoff = 0
		state.NextTry = now.Add(managedKeyFullSync)
	} else {
		if syncErr == nil {
			syncErr = snapshotErr
		}
		state.LastError = syncErr.Error()
		if state.Backoff < managedKeyRetryMin {
			state.Backoff = managedKeyRetryMin
		} else {
			state.Backoff *= 2
			if state.Backoff > managedKeyRetryMax {
				state.Backoff = managedKeyRetryMax
			}
		}
		state.NextTry = now.Add(state.Backoff)
	}
	s.managedKeyNode[nodeID] = state
	s.managedKeyMu.Unlock()
	if s.History != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.History.RecordManagedKeySync(ctx, nodeID, state.SnapshotID, state.LastError, state.SyncedAt)
		cancel()
	}
}

func (s *Server) managedKeySyncDue(nodeID string) bool {
	s.managedKeyMu.Lock()
	defer s.managedKeyMu.Unlock()
	state, ok := s.managedKeyNode[nodeID]
	return !ok || !time.Now().UTC().Before(state.NextTry)
}

func (s *Server) managedKeyStatuses() []ManagedKeyNodeStatus {
	records, err := s.Registry.List()
	if err != nil {
		return nil
	}
	current := ""
	if snapshot, snapshotErr := s.managedKeySnapshot(); snapshotErr == nil {
		current = snapshot.SnapshotID
	}
	s.managedKeyMu.Lock()
	defer s.managedKeyMu.Unlock()
	statuses := make([]ManagedKeyNodeStatus, 0, len(records))
	for _, record := range records {
		state, ok := s.managedKeyNode[record.ID]
		item := ManagedKeyNodeStatus{NodeID: record.ID, Node: record.Name, Status: "pending"}
		if ok {
			item.SnapshotID, item.SyncedAt, item.Error = state.SnapshotID, state.SyncedAt, state.LastError
			if state.LastError != "" {
				item.Status = "error"
			} else if current != "" && state.SnapshotID == current {
				item.Status = "synced"
			}
		}
		statuses = append(statuses, item)
	}
	return statuses
}
