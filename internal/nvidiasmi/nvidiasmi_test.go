package nvidiasmi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpuardian/internal/gpusmi"
)

func TestParseGPUIndexUUIDCSV(t *testing.T) {
	data := []byte("0, GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n1, GPU-11111111-2222-3333-4444-555555555555\n")
	got, err := ParseGPUIndexUUIDCSV(data)
	if err != nil {
		t.Fatalf("ParseGPUIndexUUIDCSV: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got["GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"] != 0 {
		t.Errorf("uuid 0 -> %d, want 0", got["GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"])
	}
	if got["GPU-11111111-2222-3333-4444-555555555555"] != 1 {
		t.Errorf("uuid 1 -> %d, want 1", got["GPU-11111111-2222-3333-4444-555555555555"])
	}
}

func TestParseGPUIndexUUIDCSV_SkipsBadRows(t *testing.T) {
	data := []byte("not-a-number, GPU-x\n0, N/A\n0, \n1, GPU-good\n")
	got, err := ParseGPUIndexUUIDCSV(data)
	if err != nil {
		t.Fatalf("ParseGPUIndexUUIDCSV: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1 (only GPU-good)", len(got))
	}
	if got["GPU-good"] != 1 {
		t.Errorf("GPU-good -> %d, want 1", got["GPU-good"])
	}
}

func TestParseComputeAppsCSV(t *testing.T) {
	uuidToIndex := map[string]int{
		"GPU-aaa": 0,
		"GPU-bbb": 1,
	}
	data := []byte("GPU-aaa, 1234, 512\nGPU-bbb, 5678, 1024\n")
	got, err := ParseComputeAppsCSV(data, uuidToIndex)
	if err != nil {
		t.Fatalf("ParseComputeAppsCSV: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d processes, want 2", len(got))
	}
	if got[0].GPU != 0 || got[0].PID != 1234 || got[0].MemBytes != 512*1024*1024 {
		t.Errorf("process 0: %+v", got[0])
	}
	if got[0].MemBytesUnknown {
		t.Errorf("process 0 should not be MemBytesUnknown")
	}
	if got[1].GPU != 1 || got[1].PID != 5678 || got[1].MemBytes != 1024*1024*1024 {
		t.Errorf("process 1: %+v", got[1])
	}
}

func TestParseComputeAppsCSV_UnknownUUIDSkipped(t *testing.T) {
	uuidToIndex := map[string]int{"GPU-aaa": 0}
	data := []byte("GPU-unknown, 1234, 512\nGPU-aaa, 5678, 256\n")
	got, err := ParseComputeAppsCSV(data, uuidToIndex)
	if err != nil {
		t.Fatalf("ParseComputeAppsCSV: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d processes, want 1 (unknown uuid skipped)", len(got))
	}
	if got[0].PID != 5678 {
		t.Errorf("expected pid 5678, got %d", got[0].PID)
	}
}

func TestParseComputeAppsCSV_NAMemoryIsUnknown(t *testing.T) {
	uuidToIndex := map[string]int{"GPU-aaa": 0}
	data := []byte("GPU-aaa, 1234, N/A\n")
	got, err := ParseComputeAppsCSV(data, uuidToIndex)
	if err != nil {
		t.Fatalf("ParseComputeAppsCSV: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d processes, want 1", len(got))
	}
	if !got[0].MemBytesUnknown {
		t.Errorf("expected MemBytesUnknown=true for N/A memory")
	}
	if got[0].MemBytes != 0 {
		t.Errorf("expected MemBytes=0 for N/A memory, got %d", got[0].MemBytes)
	}
}

func TestParseComputeAppsCSV_BadPID(t *testing.T) {
	uuidToIndex := map[string]int{"GPU-aaa": 0}
	data := []byte("GPU-aaa, notapid, 512\nGPU-aaa, -5, 512\nGPU-aaa, 0, 512\nGPU-aaa, 999, 512\n")
	got, err := ParseComputeAppsCSV(data, uuidToIndex)
	if err != nil {
		t.Fatalf("ParseComputeAppsCSV: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d processes, want 1 (only pid 999 valid)", len(got))
	}
	if got[0].PID != 999 {
		t.Errorf("expected pid 999, got %d", got[0].PID)
	}
}

func TestParseMetricCSV(t *testing.T) {
	data := []byte("0, 81920, 4096, 37\n1, 81920, 0, 0\n")
	got, err := ParseMetricCSV(data)
	if err != nil {
		t.Fatalf("ParseMetricCSV: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d metrics, want 2", len(got))
	}
	if got[0].GPU != 0 {
		t.Errorf("metric 0 GPU: %d", got[0].GPU)
	}
	if got[0].MemoryTotalBytes == nil || *got[0].MemoryTotalBytes != 81920*1024*1024 {
		t.Errorf("metric 0 total: %v", got[0].MemoryTotalBytes)
	}
	if got[0].MemoryUsedBytes == nil || *got[0].MemoryUsedBytes != 4096*1024*1024 {
		t.Errorf("metric 0 used: %v", got[0].MemoryUsedBytes)
	}
	if got[0].UtilizationPct == nil || *got[0].UtilizationPct != 37 {
		t.Errorf("metric 0 util: %v", got[0].UtilizationPct)
	}
}

func TestParseMetricCSV_NAFieldsNil(t *testing.T) {
	data := []byte("0, N/A, N/A, N/A\n")
	got, err := ParseMetricCSV(data)
	if err != nil {
		t.Fatalf("ParseMetricCSV: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d metrics, want 1", len(got))
	}
	if got[0].MemoryTotalBytes != nil || got[0].MemoryUsedBytes != nil || got[0].UtilizationPct != nil {
		t.Errorf("expected all nil for N/A, got %+v", got[0])
	}
}

func TestParseMetricCSV_UtilizationClamped(t *testing.T) {
	data := []byte("0, 81920, 4096, 150\n")
	got, err := ParseMetricCSV(data)
	if err != nil {
		t.Fatalf("ParseMetricCSV: %v", err)
	}
	if got[0].UtilizationPct == nil || *got[0].UtilizationPct != 100 {
		t.Errorf("expected util clamped to 100, got %v", got[0].UtilizationPct)
	}
}

func TestParseDeviceCSV(t *testing.T) {
	data := []byte("0, NVIDIA H100 80GB HBM3, GPU-aaa\n1, NVIDIA A100-SXM4-40GB, GPU-bbb\n")
	got, err := ParseDeviceCSV(data)
	if err != nil {
		t.Fatalf("ParseDeviceCSV: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d devices, want 2", len(got))
	}
	if got[0].Vendor != "nvidia" {
		t.Errorf("device 0 vendor: %q, want nvidia", got[0].Vendor)
	}
	if got[0].Model != "NVIDIA H100 80GB HBM3" {
		t.Errorf("device 0 model: %q", got[0].Model)
	}
	if got[0].UUID != "GPU-aaa" {
		t.Errorf("device 0 uuid: %q", got[0].UUID)
	}
}

func TestParseDeviceCSV_NAFieldsEmpty(t *testing.T) {
	data := []byte("0, N/A, N/A\n")
	got, err := ParseDeviceCSV(data)
	if err != nil {
		t.Fatalf("ParseDeviceCSV: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d devices, want 1", len(got))
	}
	if got[0].Model != "" || got[0].UUID != "" {
		t.Errorf("expected empty model/uuid for N/A, got %+v", got[0])
	}
	if got[0].Vendor != "nvidia" {
		t.Errorf("vendor should still be nvidia, got %q", got[0].Vendor)
	}
}

func TestParseCSVRows_Empty(t *testing.T) {
	rows, err := parseCSVRows([]byte(""), 10)
	if err != nil {
		t.Fatalf("parseCSVRows empty: %v", err)
	}
	if rows != nil {
		t.Errorf("expected nil for empty input, got %v", rows)
	}
}

func TestParseCSVRows_Oversized(t *testing.T) {
	big := strings.Repeat("0, GPU-x\n", maxProcessRows+1)
	_, err := parseCSVRows([]byte(big), maxProcessRows)
	if err == nil {
		t.Fatal("expected error for oversized output, got nil")
	}
}

func TestCLIProviderProcesses(t *testing.T) {
	gpuOut := []byte("0, GPU-aaa\n1, GPU-bbb\n")
	appsOut := []byte("GPU-aaa, 1234, 512\nGPU-bbb, 5678, 1024\n")
	calls := 0
	provider := CLIProvider{
		Command: "nvidia-smi",
		runCommand: func(ctx context.Context, command string, args ...string) ([]byte, error) {
			calls++
			switch calls {
			case 1:
				return gpuOut, nil
			case 2:
				return appsOut, nil
			default:
				return nil, errors.New("unexpected call")
			}
		},
	}
	got, err := provider.Processes(context.Background())
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d processes, want 2", len(got))
	}
	if got[0].GPU != 0 || got[0].PID != 1234 {
		t.Errorf("process 0: %+v", got[0])
	}
	if got[1].GPU != 1 || got[1].PID != 5678 {
		t.Errorf("process 1: %+v", got[1])
	}
}

func TestCLIProviderProcesses_GPUCallFails(t *testing.T) {
	provider := CLIProvider{
		Command: "nvidia-smi",
		runCommand: func(ctx context.Context, command string, args ...string) ([]byte, error) {
			return nil, errors.New("nvidia-smi not found")
		},
	}
	if _, err := provider.Processes(context.Background()); err == nil {
		t.Fatal("expected error when nvidia-smi fails, got nil")
	}
}

func TestCLIProviderMetrics(t *testing.T) {
	metricOut := []byte("0, 81920, 4096, 37\n1, 81920, 8192, 99\n")
	provider := CLIProvider{
		Command: "nvidia-smi",
		runCommand: func(ctx context.Context, command string, args ...string) ([]byte, error) {
			return metricOut, nil
		},
	}
	got, err := provider.Metrics(context.Background())
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d metrics, want 2", len(got))
	}
	if got[0].UtilizationPct == nil || *got[0].UtilizationPct != 37 {
		t.Errorf("metric 0 util: %v", got[0].UtilizationPct)
	}
}

func TestCLIProviderDevices(t *testing.T) {
	deviceOut := []byte("0, NVIDIA H100 80GB HBM3, GPU-aaa\n")
	provider := CLIProvider{
		Command: "nvidia-smi",
		runCommand: func(ctx context.Context, command string, args ...string) ([]byte, error) {
			return deviceOut, nil
		},
	}
	got, err := provider.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d devices, want 1", len(got))
	}
	info := got[0]
	if info.Vendor != "nvidia" || info.Model != "NVIDIA H100 80GB HBM3" || info.UUID != "GPU-aaa" {
		t.Errorf("device 0: %+v", info)
	}
}

// Verify CLIProvider satisfies the gpusmi interfaces at compile time.
var (
	_ gpusmi.Provider        = CLIProvider{}
	_ gpusmi.MetricsProvider = CLIProvider{}
	_ gpusmi.DeviceProvider  = CLIProvider{}
)
