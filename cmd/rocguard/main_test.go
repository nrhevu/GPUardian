package main

import (
	"bytes"
	"strings"
	"testing"

	"rocguardd/internal/config"
	"rocguardd/internal/model"
)

func TestWritePSRowsFormatsTable(t *testing.T) {
	var buf bytes.Buffer
	err := writePSRows(&buf, []model.PSRow{{
		ID:      "res_test",
		GPU:     3,
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
