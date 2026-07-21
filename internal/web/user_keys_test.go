package web

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFixedUserKeyBackfillRevealReopenAndRegenerate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	store := NewUserStore(path)
	if _, err := store.Create("alice", "correct horse battery staple", RoleUser); err != nil {
		t.Fatal(err)
	}
	master := bytes.Repeat([]byte{0x41}, 32)
	if err := store.InitializeFixedKeys(master); err != nil {
		t.Fatal(err)
	}
	first, err := store.RevealFixedKey("alice")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.Version != 1 || len(first.Secret) != 3+fixedKeySecretBytes*2 || first.Secret[:3] != "gk_" {
		t.Fatalf("unexpected fixed key: %+v", first)
	}

	reopened := NewUserStore(path)
	if err := reopened.InitializeFixedKeys(master); err != nil {
		t.Fatal(err)
	}
	revealed, err := reopened.RevealFixedKey("alice")
	if err != nil {
		t.Fatal(err)
	}
	if revealed.Secret != first.Secret || revealed.ID != first.ID {
		t.Fatal("fixed key changed after reopen")
	}
	second, err := reopened.RegenerateFixedKey("alice")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.Secret == first.Secret || second.Version != 2 {
		t.Fatalf("regenerate did not replace the credential: first=%+v second=%+v", first, second)
	}

	wrongMaster := bytes.Repeat([]byte{0x42}, 32)
	if err := NewUserStore(path).InitializeFixedKeys(wrongMaster); err == nil {
		t.Fatal("wrong master key should fail startup validation")
	}
}

func TestUserCreatedAfterFixedKeyInitializationGetsOneKey(t *testing.T) {
	store := NewUserStore(filepath.Join(t.TempDir(), "users.json"))
	if err := store.InitializeFixedKeys(bytes.Repeat([]byte{0x55}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("bob", "correct horse battery staple", RoleUser); err != nil {
		t.Fatal(err)
	}
	keys, err := store.FixedKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Owner != "bob" || keys[0].Version != 1 {
		t.Fatalf("unexpected keys: %+v", keys)
	}
}

func TestMissingMasterDoesNotReplaceKeyForExistingEncryptedUsers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	masterPath := filepath.Join(dir, "user-key.key")
	store := NewUserStore(path)
	if _, err := store.Create("alice", "correct horse battery staple", RoleUser); err != nil {
		t.Fatal(err)
	}
	master, err := loadOrCreateSessionKey(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeFixedKeys(master); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(masterPath); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateUserKeyMaster(masterPath, NewUserStore(path)); err == nil {
		t.Fatal("missing master for encrypted users should fail")
	}
	if _, err := os.Stat(masterPath); !os.IsNotExist(err) {
		t.Fatalf("missing master was silently replaced: %v", err)
	}
}
