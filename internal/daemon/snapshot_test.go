package daemon

import (
	"context"
	"testing"
	"time"

	"gpuardian/internal/model"
)

type countingSnapshotAMD struct {
	processCalls int
	metricCalls  int
	processes    []model.GPUProcess
	metrics      []model.GPUMetric
}

func (p *countingSnapshotAMD) Processes(context.Context) ([]model.GPUProcess, error) {
	p.processCalls++
	return p.processes, nil
}

func (p *countingSnapshotAMD) Metrics(context.Context) ([]model.GPUMetric, error) {
	p.metricCalls++
	return p.metrics, nil
}

func TestSnapshotSamplesProcessesOnce(t *testing.T) {
	server := testServer(t)
	usedBytes := uint64(2)
	provider := &countingSnapshotAMD{
		processes: []model.GPUProcess{{GPU: 3, PID: 42, Name: "python", MemBytes: 1}},
		metrics:   []model.GPUMetric{{GPU: 3, MemoryUsedBytes: &usedBytes}},
	}
	server.AMD = provider
	server.Proc = daemonFakeProc{infos: map[int]model.ProcInfo{42: {PID: 42, Cmdline: []string{"python"}}}}

	snapshot, err := server.Snapshot(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Snapshot(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if provider.processCalls != 1 {
		t.Fatalf("Processes calls = %d, want 1 cached sample", provider.processCalls)
	}
	if provider.metricCalls != 1 {
		t.Fatalf("Metrics calls = %d, want 1 cached sample", provider.metricCalls)
	}
	for _, gpu := range snapshot.GPUs {
		if gpu.ID == 3 && len(gpu.Processes) == 1 && gpu.Processes[0].PID == 42 {
			return
		}
	}
	t.Fatalf("snapshot does not contain sampled process: %+v", snapshot.GPUs)
}
