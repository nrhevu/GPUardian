package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"rocguardd/internal/config"
	"rocguardd/internal/model"
)

func TestWritePSRowsFormatsTable(t *testing.T) {
	var buf bytes.Buffer
	err := writePSRows(&buf, []model.PSRow{{
		ID:      "res_test",
		GPU:     "3",
		User:    "alice",
		Command: "reserved until 2026-07-02T01:00:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"id", "gpu", "user", "command", "res_test", "reserved until"} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted ps output missing %q: %q", want, out)
		}
	}
}

func TestRunCommandRejectsLeadingFlag(t *testing.T) {
	err := runCommand(config.Config{}, []string{"-x", "--", "echo", "ok"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rocguard run -- <command>") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseGPUList(t *testing.T) {
	gpus, err := parseGPUList("0, 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 || gpus[0] != 0 || gpus[1] != 1 {
		t.Fatalf("unexpected gpus: %+v", gpus)
	}
	if _, err := parseGPUList("0,0"); err == nil {
		t.Fatal("expected duplicate gpu error")
	}
}

func TestUsageTextShowsOnlyCurrentCommands(t *testing.T) {
	out := usageText()
	for _, want := range []string{
		"rocguard help",
		"rocguard register (--reserved | --claimed)",
		"KEY=... rocguard run -- <command>",
		"KEY=... rocguard allow docker --container <name-or-id>",
		"ROOT_KEY=... rocguard show-keys",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage text missing %q: %q", want, out)
		}
	}
	for _, old := range []string{
		"show-" + "root-key",
		"--" + "hard",
		"--" + "soft",
		"--" + "gpu",
		"rocguard " + "docker allow",
		"rocguard " + "k8s allow",
	} {
		if strings.Contains(out, old) {
			t.Fatalf("usage text still contains old command %q: %q", old, out)
		}
	}
}

func TestFilterStatusHidesRevokedAndExpiredRows(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	expiredAt := now.Add(-time.Hour)
	status := model.Status{
		Now: now,
		Tokens: []model.TokenView{
			{ID: "tok_revoked", Revoked: true},
			{ID: "tok_expired", ExpiresAt: &expiredAt},
			{ID: "tok_ok"},
		},
		Reservations: []model.ReservationView{
			{ID: "res_revoked", Active: true, Revoked: true, ExpiresAt: expiresAt},
			{ID: "res_expired", Active: true, ExpiresAt: expiredAt},
			{ID: "res_ok", Active: true, ExpiresAt: expiresAt},
		},
		Authorizations: []model.AuthorizationView{
			{ID: "auth_revoked", Active: true, Revoked: true},
			{ID: "auth_ok", Active: true},
		},
		SoftClaims: []model.SoftClaimView{
			{ID: "claim_revoked", AuthorizationID: "auth_revoked"},
			{ID: "claim_ok", AuthorizationID: "auth_ok"},
		},
		Bypasses: []model.BypassRule{
			{ID: "bp_revoked", ExpiresAt: expiresAt, Revoked: true},
			{ID: "bp_expired", ExpiresAt: expiredAt},
			{ID: "bp_ok", ExpiresAt: expiresAt},
		},
	}

	filterStatus(&status)

	if len(status.Tokens) != 1 || status.Tokens[0].ID != "tok_ok" {
		t.Fatalf("unexpected tokens: %+v", status.Tokens)
	}
	if len(status.Reservations) != 1 || status.Reservations[0].ID != "res_ok" {
		t.Fatalf("unexpected reservations: %+v", status.Reservations)
	}
	if len(status.Authorizations) != 1 || status.Authorizations[0].ID != "auth_ok" {
		t.Fatalf("unexpected authorizations: %+v", status.Authorizations)
	}
	if len(status.SoftClaims) != 1 || status.SoftClaims[0].ID != "claim_ok" {
		t.Fatalf("unexpected soft claims: %+v", status.SoftClaims)
	}
	if len(status.Bypasses) != 1 || status.Bypasses[0].ID != "bp_ok" {
		t.Fatalf("unexpected bypasses: %+v", status.Bypasses)
	}
}
