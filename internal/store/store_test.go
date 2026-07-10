package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rocguardd/internal/config"
	"rocguardd/internal/model"
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
	if _, _, err := st.ValidateToken(secret, now.Add(30*time.Minute)); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("got %v, want ErrTokenNotFound", err)
	}
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
	if _, token, reservations, err := st.RegisterHardReservations(key, "alice", []int{0}, "8h", now); err != nil {
		t.Fatalf("8h reserved reservation should be accepted: %v", err)
	} else if token.Mode != model.TokenModeReserved || len(reservations) != 1 || reservations[0].GPU != 0 {
		t.Fatalf("unexpected reserved token/reservations: token=%+v reservations=%+v", token, reservations)
	}
	if _, _, _, err := st.RegisterHardReservations(key, "bob", []int{1}, "8h1s", now); err == nil {
		t.Fatal("expected reserved ttl above 8h to fail")
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
	if len(state.Tokens) != 0 {
		t.Fatalf("token should be deleted: %+v", state.Tokens)
	}
	if len(state.Reservations) != 0 {
		t.Fatalf("reservation should be deleted, including %s: %+v", reservation.ID, state.Reservations)
	}
	if len(state.Authorizations) != 0 {
		t.Fatalf("authorization should be deleted: %+v", state.Authorizations)
	}
	if len(state.SoftClaims) != 0 {
		t.Fatalf("claims should be removed: %+v", state.SoftClaims)
	}
	if err := st.Revoke(bypass.ID); err != nil {
		t.Fatal(err)
	}
	state, err = st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Bypasses) != 0 {
		t.Fatalf("bypass should be deleted: %+v", state.Bypasses)
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
