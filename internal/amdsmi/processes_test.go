package amdsmi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestProcessesKeepMemoryOnItsGPU(t *testing.T) {
	p := CLIProvider{Command: "/opt/rocm/bin/amd-smi", runCommand: func(_ context.Context, command string, args ...string) ([]byte, error) {
		if command != "/opt/rocm/bin/amd-smi" {
			return nil, fmt.Errorf("unexpected command %s", command)
		}
		switch strings.Join(args, " ") {
		case "list --json":
			return []byte(`[{"gpu":7},{"gpu":1},{"gpu":0}]`), nil
		case "process --json":
			// Reproduces the misleading bulk result seen on Alpha.
			return []byte(`[{"gpu":0,"process_list":[{"process_info":{"pid":2828560,"mem_usage":{"value":0}}}]},{"gpu":1,"process_list":[{"process_info":{"pid":2828560,"mem_usage":{"value":0}}}]}]`), nil
		case "process --gpu 0 --json":
			return []byte(`[{"gpu":0,"process_list":[{"process_info":{"pid":2828560,"mem_usage":{"value":0}}}]}]`), nil
		case "process --gpu 1 --json":
			return []byte(`[{"gpu":1,"process_list":[{"process_info":{"pid":2828560,"mem_usage":{"value":270846402560}}}]}]`), nil
		case "process --gpu 7 --json":
			return []byte(`[{"gpu":7,"process_list":[{"process_info":"No running processes detected"}]}]`), nil
		default:
			return nil, fmt.Errorf("unexpected args %v", args)
		}
	}}
	rows, err := p.Processes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].GPU != 0 || rows[0].MemBytes != 0 || rows[1].GPU != 1 || rows[1].PID != 2828560 || rows[1].MemBytes != 270846402560 {
		t.Fatalf("lost or misattributed live GPU memory: %+v", rows)
	}
}

func TestProcessesRejectPartialSamples(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		err  error
	}{
		{"command failure", "", errors.New("SMI failed")},
		{"invalid JSON", "broken", nil},
		{"missing GPU", "[]", nil},
		{"wrong GPU even without processes", `[{"gpu":0,"process_list":[]}]`, nil},
		{"multiple GPUs", `[{"gpu":0},{"gpu":1}]`, nil},
		{"invalid process list", `[{"gpu":1,"process_list":false}]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := CLIProvider{runCommand: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[0] == "list" {
					return []byte(`[{"gpu":0},{"gpu":1}]`), nil
				}
				if args[2] == "0" {
					return []byte(`[{"gpu":0,"process_list":[{"process_info":{"pid":42,"mem_usage":{"value":100}}}]}]`), nil
				}
				return []byte(tc.data), tc.err
			}}
			rows, err := p.Processes(context.Background())
			if err == nil || rows != nil {
				t.Fatalf("partial sample accepted: rows=%+v err=%v", rows, err)
			}
		})
	}
}

func TestProcessesShareDeadline(t *testing.T) {
	p := CLIProvider{Timeout: 20 * time.Millisecond, runCommand: func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "list" {
			return []byte(`[{"gpu":0},{"gpu":1},{"gpu":2},{"gpu":3},{"gpu":4}]`), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	rows, err := p.Processes(context.Background())
	if rows != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}

func TestProcessGPUListValidation(t *testing.T) {
	for _, input := range []string{`null`, `{}`, `[{}]`, `[{"gpu":-1}]`, `[{"gpu":1.5}]`, `[{"gpu":1024}]`, `[{"gpu":0},{"gpu":0}]`} {
		t.Run(input, func(t *testing.T) {
			if ids, err := parseGPUIndexes([]byte(input)); err == nil {
				t.Fatalf("accepted %s as %v", input, ids)
			}
		})
	}
}
