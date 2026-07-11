package amdsmi

import "testing"

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
