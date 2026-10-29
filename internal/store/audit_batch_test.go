package store

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"gpuardian/internal/model"
)

func TestAppendAuditsWritesOnlyTheAuditLog(t *testing.T) {
	st := testStore(t)
	if err := st.AddBypass(model.BypassRule{ID: "bypass_1", Type: model.BypassCommand, Command: "/bin/true", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(st.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	events := []model.AuditEvent{{Kind: "kill_term", Message: "first", PID: 10}, {Kind: "kill", Message: "second", PID: 10}}
	if err := st.AppendAudits(events); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(st.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("auditing rewrote state.json")
	}
	log, err := os.ReadFile(st.cfg.AuditLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"first"`) || !strings.Contains(lines[1], `"second"`) {
		t.Fatalf("audit log = %q", log)
	}
}

func TestLoadAcceptsLegacyAuditArrayAndDropsIt(t *testing.T) {
	st := testStore(t)
	legacy := model.State{
		Bypasses: []model.BypassRule{{ID: "bypass_1", Type: model.BypassCommand, Command: "/bin/true", ExpiresAt: time.Now().Add(time.Hour)}},
		Audit:    []model.AuditEvent{{Kind: "kill", Message: "recorded by an older daemon"}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.cfg.StatePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.Load(); err != nil {
		t.Fatalf("legacy state with an audit array was rejected: %v", err)
	}
	state, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Bypasses) != 1 || len(state.Audit) != 0 {
		t.Fatalf("loaded state = %+v", state)
	}
	if err := st.AddBypass(model.BypassRule{ID: "bypass_2", Type: model.BypassCommand, Command: "/bin/false", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(st.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "recorded by an older daemon") {
		t.Fatal("legacy audit copy survived the next save")
	}
}

func TestReleaseScopesWritesStateOnce(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	seedStoreState(st, model.State{
		Reservations: []model.Reservation{
			{ID: "res_a", GPU: 0, TokenHash: "hash", ExpiresAt: now.Add(time.Hour), Active: true},
			{ID: "res_b", GPU: 1, TokenHash: "hash", ExpiresAt: now.Add(time.Hour), Active: true},
		},
		Authorizations: []model.Authorization{
			{ID: "auth_a", Mode: model.ModeUser, TokenHash: "hash", Active: true},
			{ID: "auth_b", Mode: model.ModeUser, TokenHash: "hash", Active: true},
		},
		Leases: []model.Lease{{ID: "lease_a", GPU: 2, ExpiresAt: now.Add(time.Hour), Active: true}},
	})
	writes := 0
	st.syncStateDir = func(dir string) error {
		writes++
		return syncDir(dir)
	}
	tests := []struct {
		name           string
		reservations   []string
		authorizations []string
		leases         []string
		wantWrites     int
	}{
		{name: "nothing to release", wantWrites: 0},
		{name: "unknown ids", reservations: []string{"res_missing"}, wantWrites: 0},
		{name: "rows of every kind", reservations: []string{"res_a"}, authorizations: []string{"auth_a", "auth_b"}, leases: []string{"lease_a"}, wantWrites: 1},
		{name: "already inactive", authorizations: []string{"auth_a"}, wantWrites: 1},
	}
	for _, tc := range tests {
		if err := st.ReleaseScopes(tc.reservations, tc.authorizations, tc.leases); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if writes != tc.wantWrites {
			t.Fatalf("%s: state writes = %d, want %d", tc.name, writes, tc.wantWrites)
		}
	}
	reloaded := New(st.cfg)
	state, err := reloaded.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	active := map[string]bool{}
	for _, reservation := range state.Reservations {
		active[reservation.ID] = reservation.Active
	}
	for _, authorization := range state.Authorizations {
		active[authorization.ID] = authorization.Active
	}
	for _, lease := range state.Leases {
		active[lease.ID] = lease.Active
	}
	want := map[string]bool{"res_a": false, "res_b": true, "auth_a": false, "auth_b": false, "lease_a": false}
	for id, wantActive := range want {
		if active[id] != wantActive {
			t.Fatalf("persisted %s active = %v, want %v (state %+v)", id, active[id], wantActive, state)
		}
	}
}
