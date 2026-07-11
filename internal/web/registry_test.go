package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryPersists0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	registry := NewRegistry(path)
	record, err := registry.Upsert(ServerRecord{
		Name:     "node-a",
		Endpoint: "https://node-a:8443",
		RootKey:  "rk_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID == "" {
		t.Fatal("expected generated server id")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("registry permissions = %o, want 0600", got)
	}
	public, err := registry.PublicList()
	if err != nil {
		t.Fatal(err)
	}
	if len(public) != 1 || public[0].ID != record.ID {
		t.Fatalf("unexpected public records: %+v", public)
	}
}
