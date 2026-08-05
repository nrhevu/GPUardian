package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryPersists0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	registry := NewRegistry(path, false)
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

func TestRegistryCanLoadLegacyRemoteHTTPRecordWithoutContactingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	legacy := []byte(`[{"id":"srv_legacy","name":"legacy","endpoint":"http://node.example:8192","root_key":"rk_test"}]` + "\n")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	reloaded := NewRegistry(path, false)
	records, err := reloaded.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "legacy" {
		t.Fatalf("legacy records = %+v", records)
	}
	if _, err := joinURL(records[0].Endpoint, "/healthz", false); err == nil {
		t.Fatal("legacy remote HTTP endpoint unexpectedly passed the outbound request boundary")
	}
	if err := reloaded.Delete(records[0].ID); err != nil {
		t.Fatalf("delete legacy record: %v", err)
	}
}

func TestRegistryRejectsNewPlaintextEndpoint(t *testing.T) {
	registry := NewRegistry(filepath.Join(t.TempDir(), "servers.json"), false)
	if _, err := registry.Upsert(ServerRecord{Name: "insecure", Endpoint: "http://127.0.0.1:8192", RootKey: "rk_test"}); err == nil {
		t.Fatal("new plaintext endpoint unexpectedly persisted")
	}
}

func TestRegistryAllowsNewPlaintextEndpointWithExplicitOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	registry := NewRegistry(path, true)
	record, err := registry.Upsert(ServerRecord{Name: "insecure", Endpoint: "http://127.0.0.1:8192", RootKey: "rk_test"})
	if err != nil {
		t.Fatal(err)
	}
	if record.Endpoint != "http://127.0.0.1:8192" {
		t.Fatalf("endpoint = %q", record.Endpoint)
	}
	reloaded := NewRegistry(path, true)
	records, err := reloaded.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Endpoint != record.Endpoint {
		t.Fatalf("persisted records = %+v", records)
	}
}

func TestRegistryRenamePersistsWithoutChangingConnectionDetails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	registry := NewRegistry(path, false)
	record, err := registry.Upsert(ServerRecord{Name: "old", Endpoint: "https://node-a:8192", RootKey: "rk_test"})
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := registry.Rename(record.ID, " new name ")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "new name" || renamed.Endpoint != record.Endpoint || renamed.RootKey != record.RootKey {
		t.Fatalf("renamed record = %+v", renamed)
	}
	reloaded, err := NewRegistry(path, false).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 1 || reloaded[0].Name != "new name" {
		t.Fatalf("reloaded records = %+v", reloaded)
	}
}
