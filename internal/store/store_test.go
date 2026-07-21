package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuardian/internal/config"
	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return New(config.Config{
		StatePath:   filepath.Join(dir, "state.json"),
		RootKeyPath: filepath.Join(dir, "root.key"),
		AuditLog:    filepath.Join(dir, "audit.log"),
	})
}

func TestRootKeyAndTokenLifecycle(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	secret, token, err := st.RegisterToken(key, "alice", "1h", now)
	if err != nil {
		t.Fatal(err)
	}
	if secret == "" || token.Name != "alice" {
		t.Fatalf("unexpected token: secret=%q token=%+v", secret, token)
	}
	if _, _, err := st.ValidateToken(secret, now.Add(30*time.Minute)); err != nil {
		t.Fatalf("token should be valid: %v", err)
	}
	if _, _, err := st.ValidateToken(secret, now.Add(2*time.Hour)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("got %v, want ErrTokenExpired", err)
	}
	if err := st.Revoke(secret); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ValidateToken(secret, now.Add(30*time.Minute)); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("got %v, want ErrTokenRevoked", err)
	}
}

func TestReadOrCreateRootKeyConcurrent(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		StatePath:   filepath.Join(dir, "state.json"),
		RootKeyPath: filepath.Join(dir, "root.key"),
	}

	const workers = 32
	start := make(chan struct{})
	keys := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			key, err := New(cfg).ReadOrCreateRootKey()
			if err != nil {
				errs <- err
				return
			}
			keys <- key
		}()
	}
	close(start)
	wg.Wait()
	close(keys)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent root key initialization failed: %v", err)
	}
	winner := ""
	for key := range keys {
		if winner == "" {
			winner = key
		}
		if key != winner {
			t.Fatal("concurrent callers returned different root keys")
		}
	}
	if len(winner) != 3+rootKeyHexBytes*2 || !strings.HasPrefix(winner, "rk_") {
		t.Fatal("generated root key has invalid format")
	}

	data, err := os.ReadFile(cfg.RootKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != winner+"\n" {
		t.Fatal("root key file does not contain the returned key")
	}
	info, err := os.Stat(cfg.RootKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("root key mode = %04o, want 0600", got)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".root.key.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("root key initialization left temporary files: %v", temps)
	}
}

func TestReadOrCreateRootKeyRejectsMalformedOrPermissiveFiles(t *testing.T) {
	validKey := "rk_" + strings.Repeat("a", rootKeyHexBytes*2) + "\n"
	tests := []struct {
		name string
		data string
		mode os.FileMode
	}{
		{name: "empty", data: "", mode: 0600},
		{name: "wrong prefix", data: "xx_" + strings.Repeat("a", rootKeyHexBytes*2) + "\n", mode: 0600},
		{name: "short", data: "rk_abcd\n", mode: 0600},
		{name: "non hex", data: "rk_" + strings.Repeat("z", rootKeyHexBytes*2) + "\n", mode: 0600},
		{name: "extra data", data: validKey + "unexpected", mode: 0600},
		{name: "group readable", data: validKey, mode: 0640},
		{name: "world readable", data: validKey, mode: 0604},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "root.key")
			if err := os.WriteFile(path, []byte(test.data), test.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}

			st := New(config.Config{RootKeyPath: path})
			if _, err := st.ReadOrCreateRootKey(); err == nil {
				t.Fatal("expected invalid root key file to be rejected")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, []byte(test.data)) {
				t.Fatal("invalid root key file was replaced")
			}
		})
	}
}

func TestReadOrCreateRootKeyRejectsNonRegularFiles(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "root.key")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := New(config.Config{RootKeyPath: path}).ReadOrCreateRootKey(); err == nil {
			t.Fatal("expected root key directory to be rejected")
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatal("root key directory was replaced")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.key")
		path := filepath.Join(dir, "root.key")
		validKey := "rk_" + strings.Repeat("a", rootKeyHexBytes*2) + "\n"
		if err := os.WriteFile(target, []byte(validKey), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := New(config.Config{RootKeyPath: path}).ReadOrCreateRootKey(); err == nil {
			t.Fatal("expected root key symlink to be rejected")
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("root key symlink was replaced")
		}
	})
}

func TestRegisterRejectsTTLAboveMax(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.RegisterToken(key, "alice", "25h", time.Now())
	if err == nil {
		t.Fatal("expected ttl error")
	}
}

func TestHardRegisterTTLMax(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	if _, token, reservations, err := st.RegisterHardReservations(key, "alice", []int{0}, "24h", now); err != nil {
		t.Fatalf("24h reserved reservation should be accepted: %v", err)
	} else if token.Mode != model.TokenModeReserved || len(reservations) != 1 || reservations[0].GPU != 0 {
		t.Fatalf("unexpected reserved token/reservations: token=%+v reservations=%+v", token, reservations)
	}
	if _, _, _, err := st.RegisterHardReservations(key, "bob", []int{1}, "24h1s", now); err == nil {
		t.Fatal("expected reserved ttl above 24h to fail")
	}
}

func TestHardRegisterMultipleGPUs(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	_, token, reservations, err := st.RegisterHardReservations(key, "alice", []int{0, 1}, "1h", now)
	if err != nil {
		t.Fatal(err)
	}
	if token.Mode != model.TokenModeReserved || len(reservations) != 2 {
		t.Fatalf("unexpected reserved token/reservations: token=%+v reservations=%+v", token, reservations)
	}
	if reservations[0].GPU != 0 || reservations[1].GPU != 1 || reservations[0].TokenHash != token.Hash || reservations[1].TokenHash != token.Hash {
		t.Fatalf("unexpected reservations: %+v", reservations)
	}
}

func TestScheduledReservationRejectsOverlap(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	start := now.Add(2 * time.Hour)
	end := start.Add(2 * time.Hour)
	if _, _, reservations, err := st.RegisterScheduledReservations(key, "alice", "training", []int{0}, start, end, now); err != nil {
		t.Fatal(err)
	} else if reservations[0].StartsAt != start || reservations[0].Purpose != "training" {
		t.Fatalf("unexpected scheduled reservation: %+v", reservations[0])
	}
	if _, _, _, err := st.RegisterScheduledReservations(key, "bob", "", []int{0}, start.Add(time.Hour), end.Add(time.Hour), now); err == nil {
		t.Fatal("expected overlapping reservation to fail")
	}
	if _, _, _, err := st.RegisterScheduledReservations(key, "bob", "", []int{0}, end, end.Add(time.Hour), now); err != nil {
		t.Fatalf("adjacent reservation should succeed: %v", err)
	}
	if _, _, _, err := st.RegisterScheduledReservations(key, "past", "", []int{1}, now.Add(-2*time.Hour), now.Add(-time.Hour), now); err == nil {
		t.Fatal("fully expired reservation unexpectedly persisted")
	}
}

func TestScheduledReservationExternalSessionIDIsIdempotent(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	start := now.Add(time.Hour)
	end := start.Add(time.Hour)
	secret, token, reservations, err := st.RegisterScheduledReservationsWithSession(key, "alice", "training", "sess_web", []int{0, 1}, start, end, now)
	if err != nil {
		t.Fatal(err)
	}
	retrySecret, retryToken, retryReservations, err := st.RegisterScheduledReservationsWithSession(key, "alice", "training", "sess_web", []int{1, 0}, start, end, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retrySecret != secret || retryToken.ID != token.ID || len(retryReservations) != len(reservations) {
		t.Fatalf("retry created a different reservation: token=%+v reservations=%+v", retryToken, retryReservations)
	}
	if _, _, _, err := st.RegisterScheduledReservationsWithSession(key, "alice", "different", "sess_web", []int{0, 1}, start, end, now); err == nil {
		t.Fatal("external session id accepted a different reservation")
	}
}

func TestReservationActiveAtHonorsStartWindow(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	reservation := model.Reservation{
		ID:        "res_test",
		GPU:       0,
		CreatedAt: now,
		StartsAt:  now.Add(time.Hour),
		ExpiresAt: now.Add(2 * time.Hour),
		Active:    true,
	}
	if model.ReservationActiveAt(reservation, now.Add(30*time.Minute)) {
		t.Fatal("reservation should not be active before starts_at")
	}
	if !model.ReservationActiveAt(reservation, now.Add(90*time.Minute)) {
		t.Fatal("reservation should be active during window")
	}
	if model.ReservationActiveAt(reservation, now.Add(3*time.Hour)) {
		t.Fatal("reservation should not be active after expires_at")
	}
}

func TestSoftRegisterHasNoExpiry(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	secret, token, err := st.RegisterSoftToken(key, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if token.Mode != model.TokenModeClaimed || !token.ExpiresAt.IsZero() {
		t.Fatalf("unexpected claimed token: %+v", token)
	}
	if _, _, err := st.ValidateToken(secret, now.Add(365*24*time.Hour)); err != nil {
		t.Fatalf("claimed token should not expire: %v", err)
	}
}

func TestKeyStatusShowsStoredTokenKeys(t *testing.T) {
	st := testStore(t)
	rootKey, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	secret, token, err := st.RegisterSoftToken(rootKey, "alice", now)
	if err != nil {
		t.Fatal(err)
	}

	status, err := st.Status(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Tokens) != 1 || status.Tokens[0].Key != "" {
		t.Fatalf("status should not expose token key: %+v", status.Tokens)
	}

	keyStatus, err := st.KeyStatus(rootKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyStatus.Tokens) != 1 ||
		keyStatus.Tokens[0].ID != token.ID ||
		keyStatus.Tokens[0].Key != secret ||
		keyStatus.Tokens[0].KeyStatus != model.TokenKeyStatusStored {
		t.Fatalf("show-keys should expose stored token key: %+v", keyStatus.Tokens)
	}
}

func TestStatusLinksAuthorizationToToken(t *testing.T) {
	st := testStore(t)
	rootKey, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	_, token, err := st.RegisterSoftToken(rootKey, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddAuthorization(model.Authorization{
		ID:        NewAuthorizationID(),
		Mode:      model.ModeUser,
		TokenHash: token.Hash,
		TokenMode: token.Mode,
		Holder:    token.Name,
		Username:  "alice",
		CreatedAt: now,
		Active:    true,
	}); err != nil {
		t.Fatal(err)
	}

	status, err := st.Status(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Authorizations) != 1 || status.Authorizations[0].TokenID != token.ID {
		t.Fatalf("authorization token id = %+v, want %s", status.Authorizations, token.ID)
	}
}

func TestKeyStatusMarksLegacyTokenKeysUnavailable(t *testing.T) {
	st := testStore(t)
	rootKey, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	st.mu.Lock()
	st.state = model.State{Tokens: []model.Token{{
		ID:        "tok_legacy",
		Hash:      "legacy-hash",
		Name:      "alice",
		Mode:      model.TokenModeClaimed,
		CreatedAt: now,
	}}}
	st.loaded = true
	st.mu.Unlock()

	keyStatus, err := st.KeyStatus(rootKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyStatus.Tokens) != 1 ||
		keyStatus.Tokens[0].KeyStatus != model.TokenKeyStatusNotStored ||
		keyStatus.Tokens[0].Key != "" {
		t.Fatalf("legacy token should be marked not stored: %+v", keyStatus.Tokens)
	}
}

func TestKeyStatusDeletesExpiredTokenState(t *testing.T) {
	st := testStore(t)
	rootKey, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	_, token, reservations, err := st.RegisterHardReservations(rootKey, "expired", []int{0}, "1h", now)
	if err != nil {
		t.Fatal(err)
	}
	authorization := model.Authorization{
		ID:        NewAuthorizationID(),
		Mode:      model.ModeUser,
		TokenHash: token.Hash,
		TokenMode: token.Mode,
		Holder:    token.Name,
		UID:       1000,
		CreatedAt: now,
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	if err := st.AddAuthorization(authorization); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSoftClaim(model.SoftClaim{
		GPU:             0,
		TokenHash:       token.Hash,
		AuthorizationID: authorization.ID,
		Holder:          token.Name,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddLease(model.Lease{
		ID:        NewLeaseID(),
		GPU:       0,
		Mode:      model.ModeUser,
		TokenHash: token.Hash,
		Holder:    token.Name,
		UID:       1000,
		CreatedAt: now,
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddBypass(model.BypassRule{
		ID:        NewBypassID(),
		Type:      model.BypassPID,
		PID:       123,
		Reason:    "expired",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	later := now.Add(2 * time.Hour)
	keyStatus, err := st.KeyStatus(rootKey, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyStatus.Tokens) != 0 || len(keyStatus.Reservations) != 0 || len(keyStatus.Authorizations) != 0 || len(keyStatus.Bypasses) != 0 {
		t.Fatalf("expired state should be hidden from key status: %+v", keyStatus)
	}
	state, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tokens) == 0 || len(state.Reservations) == 0 || len(state.Authorizations) == 0 {
		t.Fatalf("expired enforcement evidence was removed by a status read: %+v", state)
	}
	if err := st.Prune(later); err != nil {
		t.Fatal(err)
	}
	state, err = st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tokens) != 0 ||
		len(state.Reservations) != 0 ||
		len(state.Authorizations) != 0 ||
		len(state.SoftClaims) != 0 ||
		len(state.Leases) != 0 ||
		len(state.Bypasses) != 0 {
		t.Fatalf("expired state should be deleted, reservation=%s state=%+v", reservations[0].ID, state)
	}
}

func TestRevokeTokenRevokesRelatedState(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	secret, token, reservations, err := st.RegisterHardReservations(key, "alice", []int{0}, "1h", now)
	if err != nil {
		t.Fatal(err)
	}
	reservation := reservations[0]
	authorization := model.Authorization{
		ID:        NewAuthorizationID(),
		Mode:      model.ModeUser,
		TokenHash: token.Hash,
		TokenMode: token.Mode,
		Holder:    token.Name,
		UID:       1000,
		CreatedAt: now,
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	if err := st.AddAuthorization(authorization); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSoftClaim(model.SoftClaim{GPU: 0, TokenHash: token.Hash, AuthorizationID: authorization.ID, Holder: token.Name}, now); err != nil {
		t.Fatal(err)
	}
	bypass := model.BypassRule{
		ID:        NewBypassID(),
		Type:      model.BypassPID,
		PID:       123,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	if err := st.AddBypass(bypass); err != nil {
		t.Fatal(err)
	}
	if err := st.Revoke(secret); err != nil {
		t.Fatal(err)
	}
	state, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tokens) != 1 || !state.Tokens[0].Revoked {
		t.Fatalf("revoked token evidence should be retained: %+v", state.Tokens)
	}
	if len(state.Reservations) != 1 || !state.Reservations[0].Revoked {
		t.Fatalf("revoked reservation evidence should be retained, including %s: %+v", reservation.ID, state.Reservations)
	}
	if len(state.Authorizations) != 1 || !state.Authorizations[0].Revoked {
		t.Fatalf("revoked authorization evidence should be retained: %+v", state.Authorizations)
	}
	if len(state.SoftClaims) != 1 {
		t.Fatalf("claim evidence should be retained until pruning: %+v", state.SoftClaims)
	}
	if err := st.Revoke(bypass.ID); err != nil {
		t.Fatal(err)
	}
	state, err = st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Bypasses) != 1 || !state.Bypasses[0].Revoked {
		t.Fatalf("revoked bypass evidence should be retained: %+v", state.Bypasses)
	}
	if err := st.Prune(now); err != nil {
		t.Fatal(err)
	}
	state, err = st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tokens) != 0 || len(state.Reservations) != 0 || len(state.Authorizations) != 0 || len(state.SoftClaims) != 0 || len(state.Bypasses) != 0 {
		t.Fatalf("revoked state should be removed after explicit pruning: %+v", state)
	}
}

func TestRevokeFutureReservationDeletesRelatedToken(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	start := now.Add(2 * time.Hour)
	_, _, reservations, err := st.RegisterScheduledReservations(key, "alice", "training", []int{0, 1}, start, start.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Revoke(reservations[0].ID); err != nil {
		t.Fatal(err)
	}
	status, err := st.KeyStatus(key, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Tokens) != 0 {
		t.Fatalf("revoke by future reservation id should delete related token: %+v", status.Tokens)
	}
	if len(status.Reservations) != 0 {
		t.Fatalf("revoke by future reservation id should delete related reservations: %+v", status.Reservations)
	}
}

func TestManagedKeyReservationsHaveIndependentGroupsAndRevokeKeepsKey(t *testing.T) {
	st := testStore(t)
	rootKey, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	secret := "gk_" + strings.Repeat("a", 48)
	snapshot := protocol.ManagedUserKeySnapshot{
		SnapshotID: "sha256:test",
		Keys:       []protocol.ManagedUserKey{{ID: "uk_alice", Owner: "alice", Version: 1, Hash: HashToken(secret)}},
	}
	if _, err := st.SyncManagedUserKeys(rootKey, snapshot, now); err != nil {
		t.Fatal(err)
	}
	_, firstGroup, first, err := st.RegisterManagedReservations(rootKey, "uk_alice", "first", "sess_first", []int{0}, now, now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	_, secondGroup, _, err := st.RegisterManagedReservations(rootKey, "uk_alice", "second", "sess_second", []int{1}, now, now.Add(time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if firstGroup == secondGroup {
		t.Fatal("reservations using one fixed key must have independent groups")
	}
	if err := st.Revoke(firstGroup); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ValidateToken(secret, now.Add(time.Minute)); err != nil {
		t.Fatalf("revoking a reservation must keep the fixed key valid: %v", err)
	}
	status, err := st.KeyStatus(rootKey, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Tokens) != 1 || status.Tokens[0].Revoked {
		t.Fatalf("fixed key was revoked: %+v", status.Tokens)
	}
	for _, reservation := range status.Reservations {
		if reservation.ID == first[0].ID {
			t.Fatal("revoked reservation remained visible")
		}
		if reservation.GroupID != secondGroup {
			t.Fatalf("unexpected surviving reservation: %+v", reservation)
		}
	}
}

func TestStatusHidesOrphanReservedTokenUntilPruned(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	_, token := newToken(model.TokenModeReserved, "orphan", now.Add(time.Hour), now)

	st.mu.Lock()
	st.state.Tokens = append(st.state.Tokens, token)
	st.loaded = true
	st.mu.Unlock()

	status, err := st.Status(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Tokens) != 0 {
		t.Fatalf("orphan reserved token should be hidden from status: %+v", status.Tokens)
	}
	keyStatus, err := st.KeyStatus(key, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyStatus.Tokens) != 0 {
		t.Fatalf("orphan reserved token should be hidden from key status: %+v", keyStatus.Tokens)
	}
	state, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tokens) != 1 {
		t.Fatalf("status reads should retain orphan enforcement evidence: %+v", state.Tokens)
	}
	if err := st.Prune(now); err != nil {
		t.Fatal(err)
	}
	state, err = st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tokens) != 0 {
		t.Fatalf("orphan reserved token should be removed by explicit pruning: %+v", state.Tokens)
	}
}

func TestStatusHidesRevokedLegacyState(t *testing.T) {
	st := testStore(t)
	key, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	tokenHash := "revoked-token-hash"
	authID := NewAuthorizationID()

	st.mu.Lock()
	st.state = model.State{
		Tokens: []model.Token{{
			ID:        "tok_revoked",
			Hash:      tokenHash,
			Name:      "alice",
			Mode:      model.TokenModeClaimed,
			CreatedAt: now,
			Revoked:   true,
		}},
		Reservations: []model.Reservation{{
			ID:        NewReservationID(),
			GPU:       0,
			TokenHash: tokenHash,
			Holder:    "alice",
			CreatedAt: now,
			ExpiresAt: now.Add(time.Hour),
			Active:    true,
			Revoked:   true,
		}},
		Authorizations: []model.Authorization{{
			ID:        authID,
			Mode:      model.ModeUser,
			TokenHash: tokenHash,
			TokenMode: model.TokenModeClaimed,
			Holder:    "alice",
			UID:       1000,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Hour),
			Active:    true,
			Revoked:   true,
		}},
		SoftClaims: []model.SoftClaim{{
			ID:              NewSoftClaimID(),
			GPU:             0,
			TokenHash:       tokenHash,
			AuthorizationID: authID,
			Holder:          "alice",
			CreatedAt:       now,
			UpdatedAt:       now,
		}},
		Bypasses: []model.BypassRule{{
			ID:        NewBypassID(),
			Type:      model.BypassPID,
			PID:       123,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Hour),
			Revoked:   true,
		}, {
			ID:        NewBypassID(),
			Type:      model.BypassPID,
			PID:       124,
			CreatedAt: now.Add(-2 * time.Hour),
			ExpiresAt: now.Add(-time.Hour),
		}},
	}
	st.loaded = true
	st.mu.Unlock()

	status, err := st.Status(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Tokens) != 0 || len(status.Reservations) != 0 || len(status.Authorizations) != 0 || len(status.SoftClaims) != 0 || len(status.Bypasses) != 0 {
		t.Fatalf("revoked legacy state should be hidden: %+v", status)
	}
	keyStatus, err := st.KeyStatus(key, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyStatus.Tokens) != 0 || len(keyStatus.Reservations) != 0 || len(keyStatus.Authorizations) != 0 || len(keyStatus.Bypasses) != 0 {
		t.Fatalf("revoked legacy state should be hidden from key status: %+v", keyStatus)
	}
}

func TestStatusJSONOmitsFalseRevokedFields(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	status := model.KeyStatus{
		Now: now,
		Tokens: []model.TokenView{{
			ID:        "tok_ok",
			Name:      "alice",
			Mode:      model.TokenModeClaimed,
			CreatedAt: now,
		}},
		Reservations: []model.ReservationView{{
			ID:        "res_ok",
			GPU:       0,
			Holder:    "alice",
			CreatedAt: now,
			ExpiresAt: now.Add(time.Hour),
			Active:    true,
		}},
		Authorizations: []model.AuthorizationView{{
			ID:        "auth_ok",
			Mode:      model.ModeUser,
			TokenMode: model.TokenModeClaimed,
			Holder:    "alice",
			CreatedAt: now,
			Active:    true,
		}},
		Bypasses: []model.BypassRule{{
			ID:        "bp_ok",
			Type:      model.BypassPID,
			PID:       123,
			Reason:    "test",
			CreatedAt: now,
			ExpiresAt: now.Add(time.Hour),
		}},
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "revoked") {
		t.Fatalf("status JSON should omit false revoked fields: %s", data)
	}
}

func TestSaveFailureRollsBackInMemoryState(t *testing.T) {
	st := testStore(t)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	authorization := model.Authorization{
		ID:        NewAuthorizationID(),
		Mode:      model.ModeUser,
		Holder:    "alice",
		Command:   []string{"python", "train.py"},
		CreatedAt: now,
		Active:    true,
	}
	if err := st.AddAuthorization(authorization); err != nil {
		t.Fatal(err)
	}
	before, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	st.mu.Lock()
	originalStatePath := st.cfg.StatePath
	blocker := filepath.Join(filepath.Dir(originalStatePath), "not-a-directory")
	st.mu.Unlock()
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	diskBefore, err := os.ReadFile(originalStatePath)
	if err != nil {
		t.Fatal(err)
	}

	st.mu.Lock()
	st.cfg.StatePath = filepath.Join(blocker, "state.json")
	st.mu.Unlock()
	update := authorization
	update.Holder = "changed"
	update.Command = []string{"changed"}
	if err := st.UpdateAuthorization(update); err == nil {
		t.Fatal("expected state persistence to fail")
	}
	after, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed save changed in-memory state: before=%+v after=%+v", before, after)
	}
	diskAfter, err := os.ReadFile(originalStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(diskAfter, diskBefore) {
		t.Fatal("failed save changed the last committed state file")
	}
}

func TestStatePersistenceUsesPrivateAtomicFile(t *testing.T) {
	st := testStore(t)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	rule := model.BypassRule{
		ID:        NewBypassID(),
		Type:      model.BypassPID,
		PID:       123,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	if err := st.AddBypass(rule); err != nil {
		t.Fatal(err)
	}

	st.mu.Lock()
	statePath := st.cfg.StatePath
	st.mu.Unlock()
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("state file mode = %04o, want 0600", got)
	}
	if err := os.Chmod(statePath, 0644); err != nil {
		t.Fatal(err)
	}
	rule.ID = NewBypassID()
	if err := st.AddBypass(rule); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("replacement state file mode = %04o, want 0600", got)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(statePath), ".state.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("state persistence left temporary files: %v", temps)
	}
}

func TestPostRenameSyncFailureKeepsMemoryAtDiskCommit(t *testing.T) {
	st := testStore(t)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	first := model.BypassRule{ID: "bp_first", Type: model.BypassPID, PID: 1, Reason: "test", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st.AddBypass(first); err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("injected directory sync failure")
	st.syncStateDir = func(string) error { return syncErr }
	second := model.BypassRule{ID: "bp_second", Type: model.BypassPID, PID: 2, Reason: "test", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st.AddBypass(second); !errors.Is(err, syncErr) {
		t.Fatalf("AddBypass error = %v, want injected sync error", err)
	}
	inMemory, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(inMemory.Bypasses) != 2 {
		t.Fatalf("post-rename in-memory state rolled back: %+v", inMemory.Bypasses)
	}

	reloaded := New(st.cfg)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	onDisk, err := reloaded.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inMemory.Bypasses, onDisk.Bypasses) {
		t.Fatalf("memory/disk diverged after rename: memory=%+v disk=%+v", inMemory.Bypasses, onDisk.Bypasses)
	}
}

func TestActivateAuthorizationCannotOverwriteRevocation(t *testing.T) {
	st := testStore(t)
	authorization := model.Authorization{ID: "auth_pending", Mode: model.ModeBare, Active: true, CreatedAt: time.Now()}
	if err := st.AddAuthorization(authorization); err != nil {
		t.Fatal(err)
	}
	if err := st.Revoke(authorization.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateAuthorization(authorization.ID, 123); err == nil {
		t.Fatal("revoked pending authorization was activated")
	}
	state, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Authorizations) != 1 || !state.Authorizations[0].Revoked || state.Authorizations[0].RootPID != 0 {
		t.Fatalf("activation resurrected stale authorization: %+v", state.Authorizations)
	}
}

func TestStateLoadRejectsInsecureOrSymlinkedFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, path string)
	}{
		{
			name: "group readable",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("{}\n"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				target := path + ".target"
				if err := os.WriteFile(target, []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			tc.prepare(t, st.cfg.StatePath)
			if err := st.Load(); err == nil {
				t.Fatal("Load unexpectedly accepted an insecure state file")
			}
		})
	}
}

func TestAuditLogUsesPrivateFilesAndRotates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private-audit")
	path := filepath.Join(dir, "audit.log")
	first := model.AuditEvent{
		Time:    time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
		Kind:    "test",
		Message: "first",
	}
	if err := appendAuditLog(path, first); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Fatalf("audit directory mode = %04o, want 0700", got)
	}
	logInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := logInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("audit log mode = %04o, want 0600", got)
	}

	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxAuditLogBytes); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Message = "second"
	if err := appendAuditLog(path, second); err != nil {
		t.Fatal(err)
	}

	backup := path + ".1"
	backupInfo, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if backupInfo.Size() != maxAuditLogBytes {
		t.Fatalf("audit backup size = %d, want %d", backupInfo.Size(), maxAuditLogBytes)
	}
	if got := backupInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("audit backup mode = %04o, want 0600", got)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted model.AuditEvent
	if err := json.Unmarshal(bytes.TrimSpace(active), &persisted); err != nil {
		t.Fatalf("active audit log is not one valid event: %v", err)
	}
	if persisted.Message != second.Message {
		t.Fatalf("active audit event message = %q, want %q", persisted.Message, second.Message)
	}
	logInfo, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if logInfo.Size() > maxAuditLogBytes {
		t.Fatalf("active audit log exceeds limit: %d", logInfo.Size())
	}
	if got := logInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("rotated audit log mode = %04o, want 0600", got)
	}

	if err := os.Truncate(path, maxAuditLogBytes); err != nil {
		t.Fatal(err)
	}
	third := first
	third.Message = "third"
	if err := appendAuditLog(path, third); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(path + ".*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 || backups[0] != backup {
		t.Fatalf("audit rotation backups = %v, want one .1 backup", backups)
	}
}

func TestAuditLogRejectsEventLargerThanRotationLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "audit.log")
	event := model.AuditEvent{Kind: "test", Message: strings.Repeat("x", maxAuditLogBytes)}
	if err := appendAuditLog(path, event); err == nil {
		t.Fatal("expected oversized audit event to be rejected")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized audit event created a log: %v", err)
	}
}

func TestActiveTokenLimitNormalizesHolderAndPreservesState(t *testing.T) {
	st := testStore(t)
	rootKey, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	tokens := make([]model.Token, 0, maxActiveTokensPerHolder+2)
	for i := 0; i < maxActiveTokensPerHolder-1; i++ {
		holders := []string{"Alice", " alice ", "ALICE"}
		tokens = append(tokens, model.Token{
			ID:        fmt.Sprintf("tok_active_%d", i),
			Hash:      fmt.Sprintf("hash_active_%d", i),
			Name:      holders[i%len(holders)],
			CreatedAt: now,
		})
	}
	tokens = append(tokens,
		model.Token{ID: "tok_revoked", Name: "alice", Revoked: true},
		model.Token{ID: "tok_expired", Name: "ALICE", ExpiresAt: now},
	)
	seedStoreState(st, model.State{Tokens: tokens})

	if _, _, err := st.RegisterToken(rootKey, " Alice ", "1h", now); err != nil {
		t.Fatalf("token at active limit boundary should be accepted: %v", err)
	}
	atLimit, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RegisterSoftToken(rootKey, "aLiCe", now); err == nil || !strings.Contains(err.Error(), "active token limit") {
		t.Fatalf("expected clear active token limit error, got %v", err)
	}
	after, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, atLimit) {
		t.Fatal("rejected token registration mutated state")
	}
}

func TestTotalTokenLimitRejectsScheduledRegistrationWithoutMutation(t *testing.T) {
	st := testStore(t)
	rootKey, err := st.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	tokens := make([]model.Token, maxTotalTokens)
	for i := range tokens {
		tokens[i] = model.Token{ID: fmt.Sprintf("tok_%d", i), Name: "retired", Revoked: true}
	}
	seedStoreState(st, model.State{Tokens: tokens})
	before, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = st.RegisterScheduledReservations(rootKey, "alice", "test", []int{0}, now, now.Add(time.Hour), now)
	if err == nil || !strings.Contains(err.Error(), "token limit exceeded") {
		t.Fatalf("expected clear total token limit error, got %v", err)
	}
	after, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("rejected scheduled registration mutated token or reservation state")
	}
}

func TestReservationLimitsAtBoundaryAndBatchAdmission(t *testing.T) {
	t.Run("direct add", func(t *testing.T) {
		st := testStore(t)
		reservations := make([]model.Reservation, maxTotalReservations-1)
		seedStoreState(st, model.State{Reservations: reservations})
		if err := st.AddReservation(model.Reservation{ID: "res_at_limit"}); err != nil {
			t.Fatalf("reservation at total limit boundary should be accepted: %v", err)
		}
		atLimit, err := st.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AddReservation(model.Reservation{ID: "res_over_limit"}); err == nil || !strings.Contains(err.Error(), "reservation limit exceeded") {
			t.Fatalf("expected clear reservation limit error, got %v", err)
		}
		after, err := st.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, atLimit) {
			t.Fatal("rejected reservation mutated state")
		}
	})

	t.Run("scheduled batch", func(t *testing.T) {
		st := testStore(t)
		rootKey, err := st.ReadOrCreateRootKey()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
		reservations := make([]model.Reservation, maxTotalReservations-1)
		seedStoreState(st, model.State{Reservations: reservations})
		before, err := st.Snapshot()
		if err != nil {
			t.Fatal(err)
		}

		_, _, _, err = st.RegisterScheduledReservations(rootKey, "alice", "test", []int{0, 1}, now, now.Add(time.Hour), now)
		if err == nil || !strings.Contains(err.Error(), "reservation limit exceeded") {
			t.Fatalf("expected clear reservation batch limit error, got %v", err)
		}
		after, err := st.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatal("rejected reservation batch mutated state")
		}
	})
}

func TestAuthorizationLimitsAccountForInactiveAndExpiredEntries(t *testing.T) {
	t.Run("active per token", func(t *testing.T) {
		st := testStore(t)
		now := time.Now().UTC()
		authorizations := make([]model.Authorization, 0, maxActiveAuthorizationsPerToken+3)
		for i := 0; i < maxActiveAuthorizationsPerToken-1; i++ {
			authorizations = append(authorizations, model.Authorization{
				ID:        fmt.Sprintf("auth_active_%d", i),
				TokenHash: "token-a",
				Active:    true,
				ExpiresAt: now.Add(time.Hour),
			})
		}
		authorizations = append(authorizations,
			model.Authorization{ID: "auth_inactive", TokenHash: "token-a", Active: false},
			model.Authorization{ID: "auth_revoked", TokenHash: "token-a", Active: true, Revoked: true},
			model.Authorization{ID: "auth_expired", TokenHash: "token-a", Active: true, ExpiresAt: now.Add(-time.Hour)},
		)
		seedStoreState(st, model.State{Authorizations: authorizations})

		candidate := model.Authorization{ID: "auth_boundary", TokenHash: "token-a", Active: true, ExpiresAt: now.Add(time.Hour)}
		if err := st.AddAuthorization(candidate); err != nil {
			t.Fatalf("authorization at active limit boundary should be accepted: %v", err)
		}
		atLimit, err := st.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		candidate.ID = "auth_over_limit"
		if err := st.AddAuthorization(candidate); err == nil || !strings.Contains(err.Error(), "active authorization limit") {
			t.Fatalf("expected clear active authorization limit error, got %v", err)
		}
		after, err := st.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, atLimit) {
			t.Fatal("rejected authorization mutated state")
		}

		expired := candidate
		expired.ID = "auth_new_expired"
		expired.ExpiresAt = now.Add(-time.Hour)
		if err := st.AddAuthorization(expired); err != nil {
			t.Fatalf("expired authorization should not consume active capacity: %v", err)
		}
	})

	t.Run("update activation", func(t *testing.T) {
		st := testStore(t)
		now := time.Now().UTC()
		authorizations := make([]model.Authorization, maxActiveAuthorizationsPerToken, maxActiveAuthorizationsPerToken+1)
		for i := range authorizations {
			authorizations[i] = model.Authorization{
				ID:        fmt.Sprintf("auth_active_%d", i),
				TokenHash: "token-a",
				Active:    true,
				ExpiresAt: now.Add(time.Hour),
			}
		}
		authorizations = append(authorizations, model.Authorization{ID: "auth_candidate", TokenHash: "token-a"})
		seedStoreState(st, model.State{Authorizations: authorizations})
		before, err := st.Snapshot()
		if err != nil {
			t.Fatal(err)
		}

		update := model.Authorization{ID: "auth_candidate", TokenHash: "token-a", Active: true, ExpiresAt: now.Add(time.Hour)}
		if err := st.UpdateAuthorization(update); err == nil || !strings.Contains(err.Error(), "active authorization limit") {
			t.Fatalf("expected activation limit error, got %v", err)
		}
		after, err := st.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatal("rejected authorization update mutated state")
		}
	})
}

func TestTotalAuthorizationLimitPreservesState(t *testing.T) {
	st := testStore(t)
	authorizations := make([]model.Authorization, maxTotalAuthorizations-1)
	seedStoreState(st, model.State{Authorizations: authorizations})
	if err := st.AddAuthorization(model.Authorization{ID: "auth_at_limit"}); err != nil {
		t.Fatalf("authorization at total limit boundary should be accepted: %v", err)
	}
	atLimit, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddAuthorization(model.Authorization{ID: "auth_over_limit"}); err == nil || !strings.Contains(err.Error(), "authorization limit exceeded") {
		t.Fatalf("expected clear total authorization limit error, got %v", err)
	}
	after, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, atLimit) {
		t.Fatal("rejected authorization mutated state")
	}
}

func TestPruneInactiveManagedScopesIsNarrow(t *testing.T) {
	st := testStore(t)
	seedStoreState(st, model.State{
		Authorizations: []model.Authorization{
			{ID: "inactive-managed", CgroupPath: "/managed/a"},
			{ID: "active-managed", CgroupPath: "/managed/b", Active: true},
			{ID: "inactive-scoped", Mode: model.ModeUser, UID: 1000},
		},
		Leases: []model.Lease{
			{ID: "inactive-managed-lease", CgroupPath: "/managed/l"},
			{ID: "inactive-scoped-lease", Mode: model.ModeUser, UID: 1000},
		},
	})
	if err := st.PruneInactiveManagedScopes(
		[]string{"inactive-managed", "active-managed", "inactive-scoped"},
		[]string{"inactive-managed-lease", "inactive-scoped-lease"},
	); err != nil {
		t.Fatal(err)
	}
	state, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Authorizations) != 2 || state.Authorizations[0].ID != "active-managed" || state.Authorizations[1].ID != "inactive-scoped" {
		t.Fatalf("authorizations after narrow prune = %+v", state.Authorizations)
	}
	if len(state.Leases) != 1 || state.Leases[0].ID != "inactive-scoped-lease" {
		t.Fatalf("leases after narrow prune = %+v", state.Leases)
	}
}

func TestEnforcementSnapshotsOmitHeavyDisplayState(t *testing.T) {
	st := testStore(t)
	seedStoreState(st, model.State{
		Tokens: []model.Token{
			{ID: "token_a", Hash: "hash_a", Secret: "secret-a"},
			{ID: "token_b", Hash: "hash_b", Secret: "secret-b"},
		},
		Reservations: []model.Reservation{{ID: "reservation_a", TokenHash: "hash_a"}},
		Authorizations: []model.Authorization{
			{ID: "auth_a", TokenHash: "hash_a", Command: []string{"large", "command"}},
			{ID: "auth_b", TokenHash: "hash_b", Command: []string{"other"}},
		},
		SoftClaims: []model.SoftClaim{{ID: "claim_a", TokenHash: "hash_a", AuthorizationID: "auth_a"}},
		Leases:     []model.Lease{{ID: "lease", Command: []string{"legacy"}}},
		Audit:      []model.AuditEvent{{Kind: "large-audit"}},
	})

	snapshot, err := st.EnforcementSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Tokens[0].Secret != "" || snapshot.Tokens[1].Secret != "" || snapshot.Authorizations[0].Command != nil || snapshot.Leases[0].Command != nil || snapshot.Audit != nil {
		t.Fatalf("enforcement snapshot retained heavy or secret state: %+v", snapshot)
	}
	scoped, err := st.EnforcementSnapshotForToken("hash_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Tokens) != 1 || scoped.Tokens[0].Hash != "hash_a" || scoped.Tokens[0].Secret != "" || len(scoped.Authorizations) != 1 || scoped.Authorizations[0].ID != "auth_a" || scoped.Authorizations[0].Command != nil || len(scoped.SoftClaims) != 1 || len(scoped.Leases) != 0 || scoped.Audit != nil {
		t.Fatalf("scoped enforcement snapshot = %+v", scoped)
	}
}

func TestAuthorizationViewBoundsCommand(t *testing.T) {
	command := make([]string, maxViewCommandArgs+1)
	for i := range command {
		command[i] = strings.Repeat("x", maxViewCommandBytes)
	}
	view := authorizationView(model.Authorization{Command: command}, "")
	bytes := 0
	for _, argument := range view.Command {
		bytes += len(argument)
	}
	if len(view.Command) > maxViewCommandArgs || bytes > maxViewCommandBytes {
		t.Fatalf("bounded command has %d args and %d bytes", len(view.Command), bytes)
	}
}

func TestPruneUnreferencedInvalidEntitlementsPreservesStaleScope(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	seedStoreState(st, model.State{
		Tokens: []model.Token{
			{ID: "token_free", Hash: "hash_free", ExpiresAt: now.Add(-time.Hour)},
			{ID: "token_needed", Hash: "hash_needed", ExpiresAt: now.Add(-time.Hour)},
			{ID: "token_valid", Hash: "hash_valid", ExpiresAt: now.Add(time.Hour)},
		},
		Reservations: []model.Reservation{
			{ID: "reservation_free", TokenHash: "hash_free", ExpiresAt: now.Add(-time.Hour), Active: true},
			{ID: "reservation_needed", TokenHash: "hash_needed", ExpiresAt: now.Add(-time.Hour), Active: true},
			{ID: "reservation_old", TokenHash: "hash_valid", ExpiresAt: now.Add(-time.Hour), Active: true},
		},
		Authorizations: []model.Authorization{
			{ID: "auth_needed", Mode: model.ModeUser, TokenHash: "hash_needed", Active: true},
			{ID: "auth_managed", Mode: model.ModeBare, TokenHash: "hash_free", CgroupPath: "/managed/auth"},
		},
	})
	if err := st.PruneUnreferencedInvalidEntitlements(now); err != nil {
		t.Fatal(err)
	}
	state, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tokens) != 2 || state.Tokens[0].Hash != "hash_needed" || state.Tokens[1].Hash != "hash_valid" {
		t.Fatalf("tokens after outage-safe prune = %+v", state.Tokens)
	}
	if len(state.Reservations) != 1 || state.Reservations[0].TokenHash != "hash_needed" {
		t.Fatalf("reservations after outage-safe prune = %+v", state.Reservations)
	}
}

func TestBypassLimitAtBoundaryPreservesState(t *testing.T) {
	st := testStore(t)
	bypasses := make([]model.BypassRule, maxTotalBypasses-1)
	seedStoreState(st, model.State{Bypasses: bypasses})
	if err := st.AddBypass(model.BypassRule{ID: "bp_at_limit"}); err != nil {
		t.Fatalf("bypass at total limit boundary should be accepted: %v", err)
	}
	atLimit, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddBypass(model.BypassRule{ID: "bp_over_limit"}); err == nil || !strings.Contains(err.Error(), "bypass limit exceeded") {
		t.Fatalf("expected clear bypass limit error, got %v", err)
	}
	after, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, atLimit) {
		t.Fatal("rejected bypass mutated state")
	}
}

func TestStateBoundsRejectUnboundedRowsAndValues(t *testing.T) {
	tests := []struct {
		name  string
		state model.State
	}{
		{name: "leases", state: model.State{Leases: make([]model.Lease, maxTotalLeases+1)}},
		{name: "soft claims", state: model.State{SoftClaims: make([]model.SoftClaim, maxTotalSoftClaims+1)}},
		{name: "oversized token name", state: model.State{Tokens: []model.Token{{Name: strings.Repeat("x", maxPersistedValueBytes+1)}}}},
		{name: "oversized command", state: model.State{Authorizations: []model.Authorization{{Command: []string{strings.Repeat("x", maxPersistedCommandBytes+1)}}}}},
		{name: "invalid gpu", state: model.State{Reservations: []model.Reservation{{GPU: maxGPUIndex + 1}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateStateBounds(test.state); err == nil {
				t.Fatal("validateStateBounds unexpectedly accepted unbounded state")
			}
		})
	}
}

func seedStoreState(st *Store, state model.State) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.state = cloneState(state)
	st.committed = cloneState(state)
	st.loaded = true
}
