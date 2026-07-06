package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"rocguardd/internal/config"
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
