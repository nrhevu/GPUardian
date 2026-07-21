package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFSReaderInfoUsesExeAndStartTime(t *testing.T) {
	const pid = 123
	root := t.TempDir()
	base := filepath.Join(root, "123")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cmdline"), []byte("/spoofed/argv0\x00--flag\x00"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup"), []byte("0::/gpuardian/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Name:\ttest\nUid:\t%d\t%d\t%d\t%d\n", os.Getuid(), os.Getuid(), os.Getuid(), os.Getuid())
	if err := os.WriteFile(filepath.Join(base, "status"), []byte(status), 0644); err != nil {
		t.Fatal(err)
	}
	stat := "123 (worker (gpu) task) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242 20\n"
	if err := os.WriteFile(filepath.Join(base, "stat"), []byte(stat), 0644); err != nil {
		t.Fatal(err)
	}
	const executable = "/opt/gpuardian/actual worker"
	if err := os.Symlink(executable, filepath.Join(base, "exe")); err != nil {
		t.Fatal(err)
	}

	reader := NewFSReader(root)
	info, err := reader.Info(pid)
	if err != nil {
		t.Fatal(err)
	}
	if info.CommandPath != executable {
		t.Fatalf("command path = %q, want proc exe %q", info.CommandPath, executable)
	}
	if len(info.Cmdline) == 0 || info.Cmdline[0] != "/spoofed/argv0" {
		t.Fatalf("cmdline = %q, want spoofed argv retained only as display data", info.Cmdline)
	}
	if info.StartTime != 424242 {
		t.Fatalf("start time = %d, want 424242", info.StartTime)
	}

	if err := os.Remove(filepath.Join(base, "exe")); err != nil {
		t.Fatal(err)
	}
	info, err = reader.Info(pid)
	if err != nil {
		t.Fatal(err)
	}
	if info.CommandPath != "" {
		t.Fatalf("command path without proc exe = %q, want empty rather than argv[0]", info.CommandPath)
	}
}

func TestParseStartTimeRejectsMalformedStat(t *testing.T) {
	tests := []string{
		"123 worker S 1 2 3",
		"123 (worker) S 1 2 3",
		"123 (worker) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 invalid",
	}
	for _, stat := range tests {
		if _, err := parseStartTime(stat); err == nil {
			t.Fatalf("parseStartTime(%q) unexpectedly succeeded", stat)
		}
	}
}

func TestExtractContainerIDRequiresRuntimeOwnedCgroupShape(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, cgroup := range []string{
		"0::/docker/" + id,
		"0::/system.slice/docker-" + id + ".scope",
		"0::/kubepods.slice/kubepods-burstable.slice/pod.scope/cri-containerd-" + id + ".scope",
		"11:memory:/kubepods/burstable/pod/" + id,
	} {
		if got := ExtractContainerID(cgroup); got != id {
			t.Fatalf("ExtractContainerID(%q) = %q, want %q", cgroup, got, id)
		}
	}
	for _, cgroup := range []string{
		"0::/user.slice/attacker/" + id,
		"0::/user.slice/docker-" + id + ".scope",
		"0::/arbitrary-" + id,
	} {
		if got := ExtractContainerID(cgroup); got != "" {
			t.Fatalf("ExtractContainerID(%q) = %q, want rejection", cgroup, got)
		}
	}
	spoofed := strings.Repeat("b", 64)
	nested := "0::/kubepods.slice/pod.scope/cri-containerd-" + id + ".scope/" + spoofed
	if got := ExtractContainerID(nested); got != id {
		t.Fatalf("nested delegated cgroup selected %q, want canonical ancestor %q", got, id)
	}
	cgroupFSNested := "0::/kubepods/burstable/pod-workload/" + id + "/docker-" + spoofed + ".scope"
	if got := ExtractContainerID(cgroupFSNested); got != id {
		t.Fatalf("cgroupfs delegated scope selected %q, want immediate pod child %q", got, id)
	}
}

func TestReadCmdlineRejectsOversizedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmdline")
	if err := os.WriteFile(path, make([]byte, maxCmdlineBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCmdline(path); err == nil {
		t.Fatal("oversized cmdline unexpectedly accepted")
	}
}
