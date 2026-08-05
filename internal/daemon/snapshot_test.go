package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"gpuardian/internal/model"
	"gpuardian/internal/telemetry"
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
	server.GPU = provider
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

func TestTelemetrySamplesGPUWithoutReservation(t *testing.T) {
	server := testServer(t)
	usedBytes := uint64(1024)
	utilization := 25.0
	server.GPU = &countingSnapshotAMD{
		metrics: []model.GPUMetric{{
			GPU:              3,
			UtilizationPct:   &utilization,
			MemoryUsedBytes:  &usedBytes,
			MemoryTotalBytes: &usedBytes,
		}},
	}
	dir := t.TempDir()
	box, err := telemetry.Open(filepath.Join(dir, "node.id"), filepath.Join(dir, "outbox"), "boot-test")
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	server.Telemetry = box

	start := time.Now().UTC().Add(-5 * time.Second)
	server.sampleTelemetryMetrics(context.Background(), start, start.Add(5*time.Second))

	page, err := box.Page("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Type != telemetry.EventGPUSample {
		t.Fatalf("events = %+v, want one GPU sample", page.Events)
	}
	var sample telemetry.GPUSample
	if err := json.Unmarshal(page.Events[0].Payload, &sample); err != nil {
		t.Fatal(err)
	}
	if len(sample.GPUs) != 1 || sample.GPUs[0].GPU != 3 || sample.GPUs[0].GroupID != "" ||
		sample.GPUs[0].UtilizationPct == nil || *sample.GPUs[0].UtilizationPct != utilization {
		t.Fatalf("unreserved GPU sample = %+v", sample.GPUs)
	}
}

func TestTelemetrySamplesClaimedRunGroup(t *testing.T) {
	server := testServer(t)
	utilization := 42.0
	server.GPU = &countingSnapshotAMD{
		metrics: []model.GPUMetric{{GPU: 3, UtilizationPct: &utilization}},
	}
	server.observedJobs = map[string]*observedTelemetryJob{
		"claimed-job": {
			event: telemetry.JobEvent{
				ExecutionID: "job-claimed",
				TokenMode:   model.TokenModeClaimed,
				GPUs:        []int{3},
			},
		},
	}
	dir := t.TempDir()
	box, err := telemetry.Open(filepath.Join(dir, "node.id"), filepath.Join(dir, "outbox"), "boot-test")
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	server.Telemetry = box

	start := time.Now().UTC().Add(-5 * time.Second)
	server.sampleTelemetryMetrics(context.Background(), start, start.Add(5*time.Second))

	page, err := box.Page("", 10)
	if err != nil {
		t.Fatal(err)
	}
	var sample telemetry.GPUSample
	if len(page.Events) != 1 || json.Unmarshal(page.Events[0].Payload, &sample) != nil {
		t.Fatalf("events = %+v, want one decodable GPU sample", page.Events)
	}
	if len(sample.GPUs) != 1 || sample.GPUs[0].GroupID != "claimed:job-claimed" ||
		len(sample.GPUs[0].GroupIDs) != 1 || sample.GPUs[0].GroupIDs[0] != "claimed:job-claimed" {
		t.Fatalf("claimed GPU sample = %+v", sample.GPUs)
	}
}
