package nvidiasmi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuardian/internal/gpusmi"
)

const (
	gpuMapCall  = "--query-gpu=index,uuid --format=csv,noheader,nounits"
	computeCall = "--query-compute-apps=gpu_uuid,pid,used_memory --format=csv,noheader,nounits"
)

type fakeNvidiaSMI struct {
	mu    sync.Mutex
	calls map[string]int
	gpus  string
	apps  string
}

func (f *fakeNvidiaSMI) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[call]++
	switch call {
	case gpuMapCall:
		return []byte(f.gpus), nil
	case computeCall:
		return []byte(f.apps), nil
	}
	return nil, fmt.Errorf("unexpected call %q", call)
}

func (f *fakeNvidiaSMI) set(gpus, apps string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gpus, f.apps = gpus, apps
}

func (f *fakeNvidiaSMI) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[call]
}

func TestCLIProviderCachesGPUIndexMap(t *testing.T) {
	now := time.Unix(1000, 0)
	smi := &fakeNvidiaSMI{gpus: "0, GPU-aaa\n1, GPU-bbb\n", apps: "GPU-bbb, 42, 512\n"}
	provider := CLIProvider{Command: "nvidia-smi", runCommand: smi.run, cache: &cliCache{now: func() time.Time { return now }}}

	steps := []struct {
		name     string
		advance  time.Duration
		gpus     string
		apps     string
		wantMaps int
		wantGPU  int
		wantRows int
	}{
		{name: "first sample queries the map", wantMaps: 1, wantGPU: 1, wantRows: 1},
		{name: "known UUID reuses the map", wantMaps: 1, wantGPU: 1, wantRows: 1},
		{name: "unseen UUID refreshes the map", gpus: "0, GPU-aaa\n1, GPU-bbb\n2, GPU-ccc\n", apps: "GPU-ccc, 43, 512\n", wantMaps: 2, wantGPU: 2, wantRows: 1},
		{name: "unresolvable UUID refreshes once", apps: "GPU-zzz, 44, 512\n", wantMaps: 3, wantRows: 0},
		{name: "same unresolvable UUID reuses the map", apps: "GPU-zzz, 44, 512\n", wantMaps: 3, wantRows: 0},
		{name: "TTL expiry refreshes the map", advance: gpuMapTTL, apps: "GPU-aaa, 45, 512\n", wantMaps: 4, wantGPU: 0, wantRows: 1},
	}
	for _, step := range steps {
		now = now.Add(step.advance)
		if step.gpus != "" || step.apps != "" {
			gpus := step.gpus
			if gpus == "" {
				gpus = smi.gpus
			}
			smi.set(gpus, step.apps)
		}
		rows, err := provider.Processes(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if len(rows) != step.wantRows || (step.wantRows > 0 && rows[0].GPU != step.wantGPU) {
			t.Fatalf("%s: rows = %+v", step.name, rows)
		}
		if got := smi.count(gpuMapCall); got != step.wantMaps {
			t.Fatalf("%s: map queries = %d, want %d", step.name, got, step.wantMaps)
		}
	}
}

func TestCLIProviderProcessesForGPUsFilters(t *testing.T) {
	smi := &fakeNvidiaSMI{gpus: "0, GPU-aaa\n1, GPU-bbb\n", apps: "GPU-aaa, 41, 512\nGPU-bbb, 42, 512\n"}
	provider := NewCLIProvider()
	provider.runCommand = smi.run
	rows, err := provider.ProcessesForGPUs(context.Background(), []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GPU != 1 || rows[0].PID != 42 {
		t.Fatalf("rows = %+v", rows)
	}
}

var (
	_ gpusmi.FreshProvider      = CLIProvider{}
	_ gpusmi.GPUProcessProvider = CLIProvider{}
)
