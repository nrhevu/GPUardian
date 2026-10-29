package web

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNodeLayoutStorePersistsOrderAndGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-layout.json")
	store := NewNodeLayoutStore(path)
	initial, err := store.Get([]string{"srv_a", "srv_b", "srv_c"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(initial.Root, []string{"srv_a", "srv_b", "srv_c"}) {
		t.Fatalf("initial root = %v", initial.Root)
	}

	want := NodeLayout{
		Root: []string{"grp_train", "srv_c"},
		Groups: []NodeGroupRecord{{
			ID:      "grp_train",
			Name:    "Training",
			NodeIDs: []string{"srv_b", "srv_a"},
		}},
	}
	stored, err := store.Update(want, []string{"srv_a", "srv_b", "srv_c"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored layout = %+v, want %+v", stored, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("layout permissions = %o, want 600", got)
	}

	reloaded, err := NewNodeLayoutStore(path).Get([]string{"srv_a", "srv_b", "srv_c"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded, want) {
		t.Fatalf("reloaded layout = %+v, want %+v", reloaded, want)
	}
}

func TestNodeLayoutStorePrunesDeletedNodesAndAppendsNewNodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-layout.json")
	store := NewNodeLayoutStore(path)
	_, err := store.Update(NodeLayout{
		Root: []string{"grp_train", "srv_b"},
		Groups: []NodeGroupRecord{{
			ID:      "grp_train",
			Name:    "Training",
			NodeIDs: []string{"srv_a"},
		}},
	}, []string{"srv_a", "srv_b"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get([]string{"srv_b", "srv_c"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Root, []string{"grp_train", "srv_b", "srv_c"}) {
		t.Fatalf("root = %v", got.Root)
	}
	if len(got.Groups) != 1 || len(got.Groups[0].NodeIDs) != 0 {
		t.Fatalf("groups = %+v", got.Groups)
	}
}

func TestNodeLayoutStoreRejectsUnknownAndDuplicateNodes(t *testing.T) {
	store := NewNodeLayoutStore(filepath.Join(t.TempDir(), "node-layout.json"))
	if _, err := store.Update(NodeLayout{Root: []string{"srv_missing"}}, []string{"srv_a"}); err == nil {
		t.Fatal("expected unknown node error")
	}
	if _, err := store.Update(NodeLayout{
		Root:   []string{"grp_train", "srv_a"},
		Groups: []NodeGroupRecord{{ID: "grp_train", Name: "Training", NodeIDs: []string{"srv_a"}}},
	}, []string{"srv_a"}); err == nil {
		t.Fatal("expected duplicate node error")
	}
}

func TestNodeLayoutStoreGetDoesNotRewriteUnchangedLayout(t *testing.T) {
	tests := []struct {
		name    string
		layout  NodeLayout
		servers []string
	}{
		{name: "empty group", servers: []string{"srv_a"}, layout: NodeLayout{
			Root:   []string{"grp_empty", "srv_a"},
			Groups: []NodeGroupRecord{{ID: "grp_empty", Name: "Empty", NodeIDs: []string{}}},
		}},
		{name: "no nodes", servers: []string{}, layout: NodeLayout{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node-layout.json")
			store := NewNodeLayoutStore(path)
			if _, err := store.Update(tt.layout, tt.servers); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := store.Get(tt.servers); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("Get rewrote an unchanged layout: stat err = %v", err)
			}
		})
	}
}
