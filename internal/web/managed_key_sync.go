package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"gpuardian/internal/protocol"
)

const (
	managedKeyRetryMin       = 2 * time.Second
	managedKeyRetryMax       = 60 * time.Second
	managedKeyFullSync       = 5 * time.Minute
	maxManagedKeySyncWorkers = 8
)

func deriveManagedKeyAuthority(master []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("gpuardian-managed-key-authority-v1\x00"))
	_, _ = digest.Write(master)
	return "mka_" + hex.EncodeToString(digest.Sum(nil)[:16])
}

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

type managedKeySnapshotCache struct {
	generation uint64
	authority  string
	snapshot   protocol.ManagedUserKeySnapshot
	ok         bool
}

// managedKeySnapshot is read on every key listing, sync and status check; the
// key set only changes when the user store does, so reuse the digest until then.
func (s *Server) managedKeySnapshot() (protocol.ManagedUserKeySnapshot, error) {
	generation := s.Users.Generation()
	s.managedKeySnapMu.Lock()
	cached := s.managedKeySnap
	s.managedKeySnapMu.Unlock()
	if cached.ok && cached.generation == generation && cached.authority == s.managedKeyAuthority {
		return cached.snapshot, nil
	}
	snapshot, err := s.buildManagedKeySnapshot()
	if err != nil {
		return protocol.ManagedUserKeySnapshot{}, err
	}
	s.managedKeySnapMu.Lock()
	s.managedKeySnap = managedKeySnapshotCache{generation: generation, authority: s.managedKeyAuthority, snapshot: snapshot, ok: true}
	s.managedKeySnapMu.Unlock()
	return snapshot, nil
}

func (s *Server) buildManagedKeySnapshot() (protocol.ManagedUserKeySnapshot, error) {
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
	return protocol.ManagedUserKeySnapshot{
		SnapshotID:  "sha256:" + hex.EncodeToString(digest[:]),
		AuthorityID: s.managedKeyAuthority,
		Keys:        keys,
	}, nil
}

func (s *Server) syncManagedKeysToNode(ctx context.Context, record ServerRecord) error {
	snapshotID, err := s.syncManagedKeysToNodeOnce(ctx, record)
	recordErr := err
	if err == nil && snapshotID == "" {
		// Adapters without managed-key support record the current snapshot.
		snapshot, snapshotErr := s.managedKeySnapshot()
		snapshotID, recordErr = snapshot.SnapshotID, snapshotErr
	}
	s.recordManagedKeySync(record.ID, snapshotID, recordErr)
	return err
}

// ensureManagedKeysOnNode syncs before a reservation or authorization unless
// the node already confirmed the current key snapshot within the periodic
// full-sync window, which saves two node round trips on the request path.
func (s *Server) ensureManagedKeysOnNode(ctx context.Context, record ServerRecord) error {
	if snapshot, err := s.managedKeySnapshot(); err == nil {
		s.managedKeyMu.Lock()
		state, ok := s.managedKeyNode[record.ID]
		s.managedKeyMu.Unlock()
		if ok && state.LastError == "" && state.SnapshotID == snapshot.SnapshotID &&
			time.Since(state.SyncedAt) < managedKeyFullSync {
			return nil
		}
	}
	return s.syncManagedKeysToNode(ctx, record)
}

func (s *Server) syncManagedKeysToNodeOnce(ctx context.Context, record ServerRecord) (string, error) {
	client, ok := s.Client.(ManagedKeyNodeAPI)
	if !ok {
		return "", nil // Test/legacy adapters opt out; the production client always implements this API.
	}
	infoClient, ok := s.Client.(TelemetryNodeAPI)
	if !ok {
		return "", errors.New("node client cannot verify managed user-key capability")
	}
	info, err := infoClient.Info(ctx, record)
	if err != nil {
		return "", fmt.Errorf("query managed user-key capability: %w", err)
	}
	if !telemetryCapability(info, "managed_user_keys_v1") {
		return "", errors.New("node daemon must be upgraded for fixed user keys")
	}
	snapshot, err := s.managedKeySnapshot()
	if err != nil {
		return "", err
	}
	if !telemetryCapability(info, "managed_key_authority_v1") {
		snapshot.AuthorityID = ""
	}
	result, err := client.SyncManagedUserKeys(ctx, record, snapshot)
	if err != nil {
		return "", fmt.Errorf("sync fixed user keys to %s: %w", record.Name, err)
	}
	if !result.Managed || result.SnapshotID != snapshot.SnapshotID {
		return "", errors.New("node did not confirm the current fixed-key snapshot")
	}
	return snapshot.SnapshotID, nil
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
	due := records[:0]
	for _, record := range records {
		if !record.Archived && (force || s.managedKeySyncDue(record.ID)) {
			due = append(due, record)
		}
	}
	// Unreachable nodes each hold a worker for the full timeout; syncing in
	// parallel keeps one dead node from delaying key delivery to the rest.
	work := make(chan ServerRecord)
	var wg sync.WaitGroup
	for range min(maxManagedKeySyncWorkers, len(due)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for record := range work {
				nodeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				_ = s.syncManagedKeysToNode(nodeCtx, record)
				cancel()
			}
		}()
	}
	for _, record := range due {
		if ctx.Err() != nil {
			break
		}
		work <- record
	}
	close(work)
	wg.Wait()
}

func (s *Server) recordManagedKeySync(nodeID, snapshotID string, syncErr error) {
	now := time.Now().UTC()
	s.managedKeyMu.Lock()
	if s.managedKeyNode == nil {
		s.managedKeyNode = make(map[string]managedKeyNodeState)
	}
	state := s.managedKeyNode[nodeID]
	if syncErr == nil {
		state.SnapshotID = snapshotID
		state.SyncedAt = now
		state.LastError = ""
		state.Backoff = 0
		state.NextTry = now.Add(managedKeyFullSync)
	} else {
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
		if record.Archived {
			item.Status = "archived"
		} else if ok {
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
