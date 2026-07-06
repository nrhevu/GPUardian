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
