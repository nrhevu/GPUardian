package amdsmi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuardian/internal/gpusmi"
)

var (
	_ gpusmi.FreshProvider      = CLIProvider{}
	_ gpusmi.GPUProcessProvider = CLIProvider{}
)

// recordingSMI fakes amd-smi/rocm-smi and counts each distinct invocation.
type recordingSMI struct {
	mu      sync.Mutex
	calls   map[string]int
	respond func(call string) ([]byte, error)
}

func (r *recordingSMI) run(_ context.Context, command string, args ...string) ([]byte, error) {
	call := command + " " + strings.Join(args, " ")
	r.mu.Lock()
	if r.calls == nil {
		r.calls = make(map[string]int)
	}
	r.calls[call]++
	r.mu.Unlock()
	return r.respond(call)
}

func (r *recordingSMI) count(call string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[call]
}

func cachedProvider(smi *recordingSMI, now *time.Time) CLIProvider {
	cache := newCLICache()
	cache.now = func() time.Time { return *now }
	return CLIProvider{Command: "amd-smi", runCommand: smi.run, cache: cache}
}

func perGPUResponse(call string) ([]byte, error) {
	for _, gpu := range []string{"0", "1"} {
		if call == "amd-smi process --gpu "+gpu+" --json" {
			return []byte(fmt.Sprintf(`[{"gpu":%s,"process_list":[{"process_info":{"pid":4%s,"mem_usage":{"value":1}}}]}]`, gpu, gpu)), nil
		}
	}
	return nil, fmt.Errorf("unexpected call %q", call)
}

func TestProcessesReuseCachedGPUList(t *testing.T) {
	now := time.Unix(1000, 0)
	smi := &recordingSMI{respond: func(call string) ([]byte, error) {
		if call == "amd-smi list --json" {
			return []byte(`[{"gpu":0},{"gpu":1}]`), nil
		}
		return perGPUResponse(call)
	}}
	provider := cachedProvider(smi, &now)
	for i := 0; i < 3; i++ {
		rows, err := provider.Processes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %+v", rows)
		}
	}
	if got := smi.count("amd-smi list --json"); got != 1 {
		t.Fatalf("list calls = %d, want 1 cached inventory", got)
	}
	if got := smi.count("amd-smi process --gpu 1 --json"); got != 3 {
		t.Fatalf("per-GPU calls = %d, want one per sample", got)
	}
	now = now.Add(gpuListTTL)
	if _, err := provider.Processes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := smi.count("amd-smi list --json"); got != 2 {
		t.Fatalf("list calls after TTL = %d, want 2", got)
	}
}

func TestProcessesRefreshCachedGPUListOnFailure(t *testing.T) {
	tests := []struct {
		name string
		// fail answers the first per-GPU query for GPU 1 after the inventory
		// was cached.
		fail func() ([]byte, error)
	}{
		{name: "query error", fail: func() ([]byte, error) { return nil, errors.New("GPU 1 vanished") }},
		{name: "unexpected GPU", fail: func() ([]byte, error) { return []byte(`[{"gpu":2,"process_list":[]}]`), nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			var mu sync.Mutex
			failNext := false
			smi := &recordingSMI{}
			smi.respond = func(call string) ([]byte, error) {
				if call == "amd-smi list --json" {
					return []byte(`[{"gpu":0},{"gpu":1}]`), nil
				}
				mu.Lock()
				fail := failNext && call == "amd-smi process --gpu 1 --json"
				if fail {
					failNext = false
				}
				mu.Unlock()
				if fail {
					return tc.fail()
				}
				return perGPUResponse(call)
			}
			provider := cachedProvider(smi, &now)
			if _, err := provider.Processes(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			failNext = true
			mu.Unlock()
			rows, err := provider.Processes(context.Background())
			if err != nil {
				t.Fatalf("stale cached inventory was not retried against a fresh list: %v", err)
			}
			if len(rows) != 2 {
				t.Fatalf("rows = %+v", rows)
			}
			if got := smi.count("amd-smi list --json"); got != 2 {
				t.Fatalf("list calls = %d, want refresh after failure", got)
			}
		})
	}
}

func TestProcessesForGPUsQueriesOnlyRequestedGPUs(t *testing.T) {
	now := time.Unix(1000, 0)
	smi := &recordingSMI{respond: perGPUResponse}
	provider := cachedProvider(smi, &now)
	rows, err := provider.ProcessesForGPUs(context.Background(), []int{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GPU != 1 || rows[0].PID != 41 {
		t.Fatalf("rows = %+v", rows)
	}
	if smi.count("amd-smi list --json") != 0 || smi.count("amd-smi process --gpu 0 --json") != 0 || smi.count("amd-smi process --gpu 1 --json") != 1 {
		t.Fatalf("calls = %v, want one query for GPU 1 only", smi.calls)
	}
	if _, err := provider.ProcessesForGPUs(context.Background(), []int{maxGPUIndex + 1}); err == nil {
		t.Fatal("out-of-range GPU accepted")
	}
}

func TestProcessesCoalesceConcurrentSamples(t *testing.T) {
	now := time.Unix(1000, 0)
	release := make(chan struct{})
	smi := &recordingSMI{respond: func(call string) ([]byte, error) {
		if call == "amd-smi list --json" {
			<-release
			return []byte(`[{"gpu":0}]`), nil
		}
		return perGPUResponse(call)
	}}
	provider := cachedProvider(smi, &now)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := provider.Processes(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	for smi.count("amd-smi list --json") == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := smi.count("amd-smi process --gpu 0 --json"); got != 1 {
		t.Fatalf("per-GPU calls = %d, want one coalesced sample", got)
	}
}

func TestMetricsCachesStaticTotalsAndMetricSource(t *testing.T) {
	const (
		metricCall = "amd-smi metric --mem-usage --usage --json"
		staticCall = "amd-smi static --vram --json"
		rocmCall   = "rocm-smi --showmeminfo vram --showuse --json"
	)
	tests := []struct {
		name string
		// samples is the number of Metrics calls.
		samples int
		respond func(call string, sample int) ([]byte, error)
		want    map[string]int
	}{
		{
			name:    "static totals are queried once",
			samples: 3,
			respond: func(call string, _ int) ([]byte, error) {
				switch call {
				case metricCall:
					return []byte(`{"gpu_data":[{"gpu":0,"mem_usage":{"used_vram":{"value":2,"unit":"MB"}},"usage":{"gfx_activity":{"value":20,"unit":"%"}}}]}`), nil
				case staticCall:
					return []byte(`{"gpu_data":[{"gpu":0,"vram":{"size":{"value":64,"unit":"MB"}}}]}`), nil
				}
				return nil, fmt.Errorf("unexpected call %q", call)
			},
			want: map[string]int{metricCall: 3, staticCall: 1, rocmCall: 0},
		},
		{
			name:    "rocm-smi used directly once amd-smi metric is known to fail",
			samples: 3,
			respond: func(call string, _ int) ([]byte, error) {
				switch call {
				case metricCall:
					return nil, errors.New("metric unsupported")
				case staticCall:
					return []byte(`{"gpu_data":[{"gpu":0,"vram":{"size":{"value":64,"unit":"MB"}}}]}`), nil
				case rocmCall:
					return []byte(`{"card0":{"GPU use (%)":"20","VRAM Total Memory (B)":"67108864","VRAM Total Used Memory (B)":"2097152"}}`), nil
				}
				return nil, fmt.Errorf("unexpected call %q", call)
			},
			want: map[string]int{metricCall: 1, staticCall: 1, rocmCall: 3},
		},
		{
			name:    "rocm-smi failure re-probes amd-smi metric",
			samples: 3,
			respond: func(call string, sample int) ([]byte, error) {
				switch call {
				case metricCall:
					return nil, errors.New("metric unsupported")
				case staticCall:
					return []byte(`{"gpu_data":[{"gpu":0,"vram":{"size":{"value":64,"unit":"MB"}}}]}`), nil
				case rocmCall:
					if sample == 1 {
						return nil, errors.New("rocm-smi failed")
					}
					return []byte(`{"card0":{"GPU use (%)":"20","VRAM Total Memory (B)":"67108864","VRAM Total Used Memory (B)":"2097152"}}`), nil
				}
				return nil, fmt.Errorf("unexpected call %q", call)
			},
			want: map[string]int{metricCall: 2, staticCall: 1, rocmCall: 3},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			sample := 0
			smi := &recordingSMI{}
			smi.respond = func(call string) ([]byte, error) { return tc.respond(call, sample) }
			provider := cachedProvider(smi, &now)
			for ; sample < tc.samples; sample++ {
				metrics, err := provider.Metrics(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(metrics) != 1 || metrics[0].MemoryTotalBytes == nil {
					t.Fatalf("sample %d metrics = %+v", sample, metrics)
				}
			}
			for call, want := range tc.want {
				if got := smi.count(call); got != want {
					t.Fatalf("%q calls = %d, want %d (all calls %v)", call, got, want, smi.calls)
				}
			}
		})
	}
}

func TestMetricsWithUnknownGPUInvalidatesInventory(t *testing.T) {
	now := time.Unix(1000, 0)
	smi := &recordingSMI{respond: func(call string) ([]byte, error) {
		switch call {
		case "amd-smi list --json":
			return []byte(`[{"gpu":0}]`), nil
		case "amd-smi metric --mem-usage --usage --json":
			return []byte(`{"gpu_data":[{"gpu":0,"mem_usage":{"used_vram":{"value":2,"unit":"MB"},"total_vram":{"value":64,"unit":"MB"}},"usage":{"gfx_activity":{"value":20,"unit":"%"}}},{"gpu":1,"mem_usage":{"used_vram":{"value":2,"unit":"MB"},"total_vram":{"value":64,"unit":"MB"}},"usage":{"gfx_activity":{"value":20,"unit":"%"}}}]}`), nil
		}
		return perGPUResponse(call)
	}}
	provider := cachedProvider(smi, &now)
	if _, err := provider.Processes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Metrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Processes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := smi.count("amd-smi list --json"); got != 2 {
		t.Fatalf("list calls = %d, want refresh after metrics reported GPU 1", got)
	}
}
