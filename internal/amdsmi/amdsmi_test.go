package amdsmi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gpuardian/internal/model"
)

func TestParseProcessJSON(t *testing.T) {
	data := []byte(`[
		{"gpu":0,"process_list":[{"process_info":{"name":"python","pid":123,"mem_usage":{"value":42,"unit":"B"}}}]},
		{"gpu":1,"process_list":[{"process_info":{"name":"N/A","pid":"bad","mem_usage":{"value":0,"unit":"B"}}}]}
	]`)
	processes, err := ParseProcessJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 1 {
		t.Fatalf("got %d processes, want 1", len(processes))
	}
	got := processes[0]
	if got.GPU != 0 || got.PID != 123 || got.Name != "python" || got.MemBytes != 42 {
		t.Fatalf("unexpected process: %+v", got)
	}
}

func TestParseProcessJSONPreservesUnknownMemory(t *testing.T) {
	data := []byte(`[{"gpu":0,"process_list":[{"process_info":{"name":"python","pid":123,"mem_usage":{"value":"N/A","unit":"B"}}}]}]`)
	processes, err := ParseProcessJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 1 || !processes[0].MemBytesUnknown {
		t.Fatalf("unknown memory telemetry was not preserved: %+v", processes)
	}
}

func TestParseMetricJSON(t *testing.T) {
	data := []byte(`{
		"gpu_data": [
			{
				"gpu": 0,
				"mem_usage": {
					"used_vram": {"value": 2048, "unit": "MB"},
					"total_vram": {"value": 65536, "unit": "MB"}
				},
				"usage": {
					"gfx_activity": {"value": 37, "unit": "%"}
				}
			}
		]
	}`)
	metrics, err := ParseMetricJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 {
		t.Fatalf("got %d metrics, want 1", len(metrics))
	}
	got := metrics[0]
	if got.GPU != 0 {
		t.Fatalf("got gpu %d, want 0", got.GPU)
	}
	if got.MemoryUsedBytes == nil || *got.MemoryUsedBytes != 2048*1024*1024 {
		t.Fatalf("unexpected used memory: %v", got.MemoryUsedBytes)
	}
	if got.MemoryTotalBytes == nil || *got.MemoryTotalBytes != 65536*1024*1024 {
		t.Fatalf("unexpected total memory: %v", got.MemoryTotalBytes)
	}
	if got.UtilizationPct == nil || *got.UtilizationPct != 37 {
		t.Fatalf("unexpected utilization: %v", got.UtilizationPct)
	}
}

func TestParseStaticJSON(t *testing.T) {
	data := []byte(`{
		"gpu_data": [
			{
				"gpu": 0,
				"vram": {
					"size": {"value": 192, "unit": "GB"}
				}
			}
		]
	}`)
	metrics, err := ParseStaticJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 {
		t.Fatalf("got %d metrics, want 1", len(metrics))
	}
	got := metrics[0]
	if got.GPU != 0 {
		t.Fatalf("got gpu %d, want 0", got.GPU)
	}
	if got.MemoryTotalBytes == nil || *got.MemoryTotalBytes != 192*1024*1024*1024 {
		t.Fatalf("unexpected total memory: %v", got.MemoryTotalBytes)
	}
}

// TestParseDeviceJSONModelFromNestedFields covers the amd-smi static payload
// shape seen on MI350X hardware: the marketing name lives under
// asic.market_name (with vbios.name as a secondary), not as a top-level
// product_name/marketing_name field. Both nested paths must be recognized;
// "N/A" values (e.g. board.model_number) must be skipped.
func TestParseDeviceJSONModelFromNestedFields(t *testing.T) {
	data := []byte(`{
		"gpu_data": [
			{
				"gpu": 0,
				"asic": {"market_name": "AMD Instinct MI350X VF"},
				"vbios": {"name": "AMD MI350X", "part_number": "113-M350-01-1K1-010C"},
				"board": {"model_number": "N/A"}
			},
			{
				"gpu": 1,
				"asic": {"market_name": "N/A"},
				"vbios": {"name": "AMD MI350X"}
			}
		]
	}`)
	devices, err := ParseDeviceJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(devices))
	}
	if d := devices[0]; d.Model != "AMD Instinct MI350X VF" {
		t.Fatalf("gpu 0 model = %q, want %q", d.Model, "AMD Instinct MI350X VF")
	}
	if d := devices[1]; d.Model != "AMD MI350X" {
		t.Fatalf("gpu 1 model = %q, want %q (vbios.name fallback)", d.Model, "AMD MI350X")
	}
}

func TestParseRocmSMIJSON(t *testing.T) {
	data := []byte(`{
		"card0": {
			"GPU use (%)": "95.0",
			"VRAM Total Memory (B)": "308902100992",
			"VRAM Total Used Memory (B)": "179314884608"
		}
	}`)
	metrics, err := ParseRocmSMIJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 {
		t.Fatalf("got %d metrics, want 1", len(metrics))
	}
	got := metrics[0]
	if got.GPU != 0 {
		t.Fatalf("got gpu %d, want 0", got.GPU)
	}
	if got.MemoryUsedBytes == nil || *got.MemoryUsedBytes != 179314884608 {
		t.Fatalf("unexpected used memory: %v", got.MemoryUsedBytes)
	}
	if got.MemoryTotalBytes == nil || *got.MemoryTotalBytes != 308902100992 {
		t.Fatalf("unexpected total memory: %v", got.MemoryTotalBytes)
	}
	if got.UtilizationPct == nil || *got.UtilizationPct != 95 {
		t.Fatalf("unexpected utilization: %v", got.UtilizationPct)
	}
}

func TestMetricsUsesOnlyPrimaryCommandWhenComplete(t *testing.T) {
	var calls []string
	provider := CLIProvider{
		Command: "amd-smi",
		runCommand: func(_ context.Context, command string, args ...string) ([]byte, error) {
			calls = append(calls, command+" "+strings.Join(args, " "))
			if command != "amd-smi" || len(args) == 0 || args[0] != "metric" {
				t.Fatalf("unexpected fallback command: %s %v", command, args)
			}
			return []byte(`{"gpu_data":[{"gpu":0,"mem_usage":{"used_vram":{"value":2,"unit":"MB"},"total_vram":{"value":64,"unit":"MB"}},"usage":{"gfx_activity":{"value":20,"unit":"%"}}}]}`), nil
		},
	}

	metrics, err := provider.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || !metricsComplete(metrics) {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], " metric ") {
		t.Fatalf("commands = %v, want metric only", calls)
	}
}

func TestMetricsUsesStaticOnlyWhenTotalIsMissing(t *testing.T) {
	var calls []string
	provider := CLIProvider{
		Command: "amd-smi",
		runCommand: func(_ context.Context, command string, args ...string) ([]byte, error) {
			calls = append(calls, command+" "+strings.Join(args, " "))
			switch {
			case command == "amd-smi" && len(args) > 0 && args[0] == "metric":
				return []byte(`{"gpu_data":[{"gpu":0,"mem_usage":{"used_vram":{"value":2,"unit":"MB"}},"usage":{"gfx_activity":{"value":20,"unit":"%"}}}]}`), nil
			case command == "amd-smi" && len(args) > 0 && args[0] == "static":
				return []byte(`{"gpu_data":[{"gpu":0,"vram":{"size":{"value":64,"unit":"MB"}}}]}`), nil
			default:
				t.Fatalf("unexpected fallback command: %s %v", command, args)
				return nil, nil
			}
		},
	}

	metrics, err := provider.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || !metricsComplete(metrics) {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
	if len(calls) != 2 || !strings.Contains(calls[0], " metric ") || !strings.Contains(calls[1], " static ") {
		t.Fatalf("commands = %v, want metric then static", calls)
	}
}

func TestMetricsSkipsStaticWhenOnlyRocmCanFillMissingField(t *testing.T) {
	var calls []string
	provider := CLIProvider{
		Command: "amd-smi",
		runCommand: func(_ context.Context, command string, args ...string) ([]byte, error) {
			calls = append(calls, command+" "+strings.Join(args, " "))
			switch {
			case command == "amd-smi" && len(args) > 0 && args[0] == "metric":
				return []byte(`{"gpu_data":[{"gpu":0,"mem_usage":{"used_vram":{"value":2,"unit":"MB"},"total_vram":{"value":64,"unit":"MB"}}}]}`), nil
			case command == "rocm-smi":
				return []byte(`{"card0":{"GPU use (%)":"20","VRAM Total Memory (B)":"67108864","VRAM Total Used Memory (B)":"2097152"}}`), nil
			default:
				t.Fatalf("unexpected fallback command: %s %v", command, args)
				return nil, nil
			}
		},
	}

	metrics, err := provider.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || !metricsComplete(metrics) {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
	if len(calls) != 2 || !strings.Contains(calls[0], " metric ") || !strings.HasPrefix(calls[1], "rocm-smi ") {
		t.Fatalf("commands = %v, want metric then rocm-smi", calls)
	}
}

func TestMetricsFallsBackWhenPrimaryCommandFails(t *testing.T) {
	var calls []string
	provider := CLIProvider{
		Command: "amd-smi",
		runCommand: func(_ context.Context, command string, args ...string) ([]byte, error) {
			calls = append(calls, command+" "+strings.Join(args, " "))
			switch {
			case command == "amd-smi" && len(args) > 0 && args[0] == "metric":
				return nil, errors.New("metric command is unavailable")
			case command == "amd-smi" && len(args) > 0 && args[0] == "static":
				return []byte(`{"gpu_data":[{"gpu":0,"vram":{"size":{"value":64,"unit":"MB"}}}]}`), nil
			case command == "rocm-smi":
				return []byte(`{"card0":{"GPU use (%)":"20","VRAM Total Memory (B)":"67108864","VRAM Total Used Memory (B)":"2097152"}}`), nil
			default:
				t.Fatalf("unexpected fallback command: %s %v", command, args)
				return nil, nil
			}
		},
	}

	metrics, err := provider.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || !metricsComplete(metrics) {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
	if len(calls) != 3 || !strings.Contains(calls[0], " metric ") || !strings.Contains(calls[1], " static ") || !strings.HasPrefix(calls[2], "rocm-smi ") {
		t.Fatalf("commands = %v, want metric, static, then rocm-smi", calls)
	}
}

func TestNumericParsingRejectsInvalidAndUnboundedValues(t *testing.T) {
	for _, value := range []any{-1.0, 1024.0, 1.5, "not-a-number"} {
		if _, err := gpuNumber(value); err == nil {
			t.Fatalf("gpuNumber(%v) unexpectedly succeeded", value)
		}
	}
	if value := bytesValue("NaN"); value != nil {
		t.Fatalf("NaN byte value = %v, want nil", *value)
	}
}

func TestMetricParsersRejectOversizedAndDuplicateGPUs(t *testing.T) {
	tooMany := []byte(`[` + strings.Repeat(`{"gpu":0},`, maxGPUIndex+1) + `{"gpu":0}]`)
	for name, parser := range map[string]func([]byte) ([]model.GPUMetric, error){
		"metric": ParseMetricJSON,
		"static": ParseStaticJSON,
	} {
		t.Run(name+" oversized", func(t *testing.T) {
			if _, err := parser(tooMany); err == nil || !strings.Contains(err.Error(), "exceeds 1024 entries") {
				t.Fatalf("oversized parse error = %v", err)
			}
		})
		t.Run(name+" duplicate", func(t *testing.T) {
			if _, err := parser([]byte(`[{"gpu":0},{"gpu":0}]`)); err == nil || !strings.Contains(err.Error(), "duplicate gpu 0") {
				t.Fatalf("duplicate parse error = %v", err)
			}
		})
	}

	var oversizedRocm strings.Builder
	oversizedRocm.WriteByte('{')
	for gpu := 0; gpu <= maxGPUIndex+1; gpu++ {
		if gpu > 0 {
			oversizedRocm.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&oversizedRocm, `"card%d":{"GPU":%d}`, gpu, gpu)
	}
	oversizedRocm.WriteByte('}')
	if _, err := ParseRocmSMIJSON([]byte(oversizedRocm.String())); err == nil || !strings.Contains(err.Error(), "exceeds 1024 entries") {
		t.Fatalf("oversized rocm-smi parse error = %v", err)
	}
	if _, err := ParseRocmSMIJSON([]byte(`{"card0":{"GPU":0},"card1":{"GPU":0}}`)); err == nil || !strings.Contains(err.Error(), "duplicate gpu 0") {
		t.Fatalf("duplicate rocm-smi parse error = %v", err)
	}
}

func TestProcessParserPrevalidatesCollectionBounds(t *testing.T) {
	tooManyGPUs := []byte(`[` + strings.Repeat(`{"gpu":0},`, maxGPUIndex+1) + `{"gpu":0}]`)
	if _, err := ParseProcessJSON(tooManyGPUs); err == nil || !strings.Contains(err.Error(), "exceeds 1024 entries") {
		t.Fatalf("oversized GPU parse error = %v", err)
	}

	tooManyProcesses := []byte(`[{"gpu":0,"process_list":[` + strings.Repeat(`{"process_info":{}},`, maxProcessRows) + `{"process_info":{}}]}]`)
	if _, err := ParseProcessJSON(tooManyProcesses); err == nil || !strings.Contains(err.Error(), "exceeds 32768 entries") {
		t.Fatalf("oversized process parse error = %v", err)
	}
	if _, err := ParseProcessJSON([]byte(`[{"gpu":0},{"gpu":0}]`)); err == nil || !strings.Contains(err.Error(), "duplicate gpu 0") {
		t.Fatalf("duplicate process GPU parse error = %v", err)
	}
}

func TestMetricParserRejectsExcessiveJSONDepth(t *testing.T) {
	data := []byte(strings.Repeat("[", maxSMIJSONDepth+1) + strings.Repeat("]", maxSMIJSONDepth+1))
	if _, err := ParseMetricJSON(data); err == nil || !strings.Contains(err.Error(), "nesting depth 64") {
		t.Fatalf("deep JSON parse error = %v", err)
	}
}

func TestBoundedOutputCapsMemory(t *testing.T) {
	output := boundedOutput{limit: 4}
	if n, err := output.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := output.String(); got != "abcd" || !output.exceeded {
		t.Fatalf("bounded output = %q exceeded=%v", got, output.exceeded)
	}
}
