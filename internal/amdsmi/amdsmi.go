package amdsmi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"gpuardian/internal/gpusmi"
	"gpuardian/internal/model"
)

// CLIProvider shells out to amd-smi (with a rocm-smi fallback for metrics) and
// satisfies gpusmi.Provider, gpusmi.MetricsProvider, and gpusmi.DeviceProvider
// via Go's structural typing. The vendor-neutral interfaces live in
// internal/gpusmi; this package owns only the AMD-specific parsing.
type CLIProvider struct {
	Command    string
	Timeout    time.Duration
	runCommand func(context.Context, string, ...string) ([]byte, error)
}

const maxSMIOutputBytes = 16 << 20
const maxGPUIndex = 1023
const maxProcessRows = 32768
const maxMetricEntryBytes = 256 << 10
const maxProcessEntryBytes = 256 << 10
const maxSMIJSONDepth = 64
const maxSMIJSONTokens = 1 << 20
const maxSMIJSONContainers = maxProcessRows*4 + maxGPUIndex + 16

type boundedOutput struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.Buffer.Write(data)
	}
	if original > remaining {
		b.exceeded = true
	}
	return original, nil
}

func NewCLIProvider() CLIProvider {
	return CLIProvider{Command: "amd-smi", Timeout: 5 * time.Second}
}

func (p CLIProvider) Processes(ctx context.Context) ([]model.GPUProcess, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := p.Command
	if command == "" {
		command = "amd-smi"
	}
	out, err := p.output(ctx, command, "process", "--json")
	if err != nil {
		return nil, err
	}
	return ParseProcessJSON(out)
}

func (p CLIProvider) Metrics(ctx context.Context) ([]model.GPUMetric, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := p.Command
	if command == "" {
		command = "amd-smi"
	}
	metricOut, metricErr := p.output(ctx, command, "metric", "--mem-usage", "--usage", "--json")
	var metrics []model.GPUMetric
	var parseErr error
	if metricErr == nil {
		metrics, parseErr = ParseMetricJSON(metricOut)
	}

	var staticErr error
	if metricsNeedTotal(metrics) {
		var staticOut []byte
		staticOut, staticErr = p.output(ctx, command, "static", "--vram", "--json")
		if staticErr == nil {
			staticMetrics, err := ParseStaticJSON(staticOut)
			if err == nil {
				metrics = mergeGPUMetrics(metrics, staticMetrics)
				parseErr = nil
			} else if parseErr == nil {
				parseErr = err
			}
		}
	}

	var rocmErr error
	tryRocm := command == "amd-smi" || strings.HasSuffix(command, "/amd-smi")
	if tryRocm && !metricsComplete(metrics) {
		var rocmOut []byte
		rocmOut, rocmErr = p.output(ctx, "rocm-smi", "--showmeminfo", "vram", "--showuse", "--json")
		if rocmErr == nil {
			rocmMetrics, err := ParseRocmSMIJSON(rocmOut)
			if err == nil {
				metrics = mergeGPUMetrics(metrics, rocmMetrics)
				parseErr = nil
			} else if parseErr == nil {
				parseErr = err
			}
		}
	}
	if len(metrics) > 0 {
		return metrics, nil
	}
	if metricErr != nil {
		return nil, metricErr
	}
	if staticErr != nil {
		return nil, staticErr
	}
	if tryRocm && rocmErr != nil {
		return nil, rocmErr
	}
	return nil, parseErr
}

func (p CLIProvider) output(ctx context.Context, command string, args ...string) ([]byte, error) {
	if p.runCommand != nil {
		return p.runCommand(ctx, command, args...)
	}
	output := boundedOutput{limit: maxSMIOutputBytes}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if output.exceeded {
		return nil, fmt.Errorf("%s output exceeds %d bytes", command, maxSMIOutputBytes)
	}
	return output.Bytes(), nil
}

// Devices reports static device identity (vendor/model/UUID) keyed by GPU
// index. It is best-effort: any failure returns an empty map so the snapshot
// simply omits the vendor/model/UUID fields rather than failing the whole
// snapshot. The amd-smi static payload is a gpu_data array whose entries may
// carry product_name/marketing_name/gpu_uuid fields alongside the gpu index.
func (p CLIProvider) Devices(ctx context.Context) (map[int]gpusmi.DeviceInfo, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := p.Command
	if command == "" {
		command = "amd-smi"
	}
	out, err := p.output(ctx, command, "static", "--json")
	if err != nil {
		return nil, err
	}
	return ParseDeviceJSON(out)
}

// ParseDeviceJSON extracts per-GPU identity from an amd-smi static --json
// payload. Unknown or missing fields are left empty; the returned map only
// contains entries whose GPU index parsed cleanly.
func ParseDeviceJSON(data []byte) (map[int]gpusmi.DeviceInfo, error) {
	data = trimToJSON(data)
	if err := validateSMIJSONComplexity(data); err != nil {
		return nil, err
	}
	entriesJSON, err := metricEntriesJSON(data)
	if err != nil {
		return nil, err
	}
	if _, err := scanJSONArray(entriesJSON, maxGPUIndex+1, maxMetricEntryBytes, "static device response", nil); err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(entriesJSON, &entries); err != nil {
		return nil, err
	}
	out := make(map[int]gpusmi.DeviceInfo, len(entries))
	seen := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		gpu, err := gpuNumber(entry["gpu"])
		if err != nil {
			continue
		}
		if err := addGPU(seen, gpu, "static device response"); err != nil {
			return nil, err
		}
		info := gpusmi.DeviceInfo{Vendor: "amd"}
		if name, ok := stringField(firstValue(entry, "product_name", "marketing_name", "name")); ok {
			info.Model = name
		}
		if uuid, ok := stringField(firstValue(entry, "gpu_uuid", "uuid", "gpu_id")); ok {
			info.UUID = uuid
		}
		out[gpu] = info
	}
	return out, nil
}

func stringField(value any) (string, bool) {
	s, ok := value.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return "", false
	}
	return s, true
}

func ParseProcessJSON(data []byte) ([]model.GPUProcess, error) {
	data = trimToJSONArray(data)
	if err := validateSMIJSONComplexity(data); err != nil {
		return nil, err
	}
	if err := validateProcessCollections(data); err != nil {
		return nil, err
	}
	var raw []struct {
		GPU         any `json:"gpu"`
		ProcessList []struct {
			ProcessInfo struct {
				Name     string `json:"name"`
				PID      any    `json:"pid"`
				MemUsage struct {
					Value any    `json:"value"`
					Unit  string `json:"unit"`
				} `json:"mem_usage"`
			} `json:"process_info"`
		} `json:"process_list"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if len(raw) > maxGPUIndex+1 {
		return nil, fmt.Errorf("process response contains too many GPUs")
	}
	var processes []model.GPUProcess
	seen := make(map[int]struct{}, len(raw))
	for _, gpuEntry := range raw {
		gpu, err := gpuNumber(gpuEntry.GPU)
		if err != nil {
			return nil, fmt.Errorf("parse gpu id: %w", err)
		}
		if err := addGPU(seen, gpu, "process response"); err != nil {
			return nil, err
		}
		for _, process := range gpuEntry.ProcessList {
			if len(processes) >= maxProcessRows {
				return nil, fmt.Errorf("process response exceeds %d rows", maxProcessRows)
			}
			pid, err := number(process.ProcessInfo.PID)
			if err != nil || pid <= 0 {
				continue
			}
			mem, memErr := number(process.ProcessInfo.MemUsage.Value)
			processes = append(processes, model.GPUProcess{
				GPU:             gpu,
				PID:             pid,
				Name:            process.ProcessInfo.Name,
				MemBytes:        uint64(max(0, mem)),
				MemBytesUnknown: memErr != nil || mem < 0,
			})
		}
	}
	return processes, nil
}

func ParseMetricJSON(data []byte) ([]model.GPUMetric, error) {
	data = trimToJSON(data)
	if err := validateSMIJSONComplexity(data); err != nil {
		return nil, err
	}
	entriesJSON, err := metricEntriesJSON(data)
	if err != nil {
		return nil, err
	}
	if _, err := scanJSONArray(entriesJSON, maxGPUIndex+1, maxMetricEntryBytes, "metric response", nil); err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(entriesJSON, &entries); err != nil {
		return nil, err
	}

	metrics := make([]model.GPUMetric, 0, len(entries))
	seen := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		gpu, err := gpuNumber(entry["gpu"])
		if err != nil {
			return nil, fmt.Errorf("parse gpu id: %w", err)
		}
		if err := addGPU(seen, gpu, "metric response"); err != nil {
			return nil, err
		}
		metric := model.GPUMetric{GPU: gpu}
		if memUsage, ok := object(entry["mem_usage"]); ok {
			metric.MemoryUsedBytes = bytesValue(firstValue(memUsage, "used_vram", "vram_used", "used"))
			metric.MemoryTotalBytes = bytesValue(firstValue(memUsage, "total_vram", "vram_total", "total"))
		}
		if usage, ok := object(entry["usage"]); ok {
			metric.UtilizationPct = percentValue(firstValue(usage, "gfx_activity", "average_gfx_activity", "gfx_busy_inst"))
		}
		metrics = append(metrics, metric)
	}
	return metrics, nil
}

func ParseStaticJSON(data []byte) ([]model.GPUMetric, error) {
	data = trimToJSON(data)
	if err := validateSMIJSONComplexity(data); err != nil {
		return nil, err
	}
	entriesJSON, err := metricEntriesJSON(data)
	if err != nil {
		return nil, err
	}
	if _, err := scanJSONArray(entriesJSON, maxGPUIndex+1, maxMetricEntryBytes, "static response", nil); err != nil {
		return nil, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(entriesJSON, &entries); err != nil {
		return nil, err
	}

	metrics := make([]model.GPUMetric, 0, len(entries))
	seen := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		gpu, err := gpuNumber(entry["gpu"])
		if err != nil {
			return nil, fmt.Errorf("parse gpu id: %w", err)
		}
		if err := addGPU(seen, gpu, "static response"); err != nil {
			return nil, err
		}
		metric := model.GPUMetric{GPU: gpu}
		if vram, ok := object(firstValue(entry, "vram", "vram_info")); ok {
			metric.MemoryTotalBytes = bytesValue(firstValue(vram, "size", "vram_size", "total_vram"))
		}
		metrics = append(metrics, metric)
	}
	return metrics, nil
}

func ParseRocmSMIJSON(data []byte) ([]model.GPUMetric, error) {
	data = trimToJSON(data)
	if err := validateSMIJSONComplexity(data); err != nil {
		return nil, err
	}
	if err := scanJSONObject(data, maxGPUIndex+1, maxMetricEntryBytes, "rocm-smi response"); err != nil {
		return nil, err
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	metrics := make([]model.GPUMetric, 0, len(raw))
	seen := make(map[int]struct{}, len(raw))
	for card, values := range raw {
		gpu, err := parseRocmCardID(card, values)
		if err != nil {
			return nil, err
		}
		if err := addGPU(seen, gpu, "rocm-smi response"); err != nil {
			return nil, err
		}
		metrics = append(metrics, model.GPUMetric{
			GPU: gpu,
			MemoryUsedBytes: bytesValueWithDefaultUnit(firstValue(
				values,
				"VRAM Total Used Memory (B)",
				"VRAM Used Memory (B)",
				"GPU Memory Used (B)",
			), "B"),
			MemoryTotalBytes: bytesValueWithDefaultUnit(firstValue(
				values,
				"VRAM Total Memory (B)",
				"VRAM Memory Total (B)",
				"GPU Memory Total (B)",
			), "B"),
			UtilizationPct: percentValue(firstValue(
				values,
				"GPU use (%)",
				"GPU Use (%)",
				"GPU use",
				"GPU Utilization (%)",
			)),
		})
	}
	return metrics, nil
}

func validateProcessCollections(data []byte) error {
	totalProcesses := 0
	_, err := scanJSONArray(data, maxGPUIndex+1, maxSMIOutputBytes, "process response", func(entry json.RawMessage) error {
		var envelope struct {
			ProcessList json.RawMessage `json:"process_list"`
		}
		if err := json.Unmarshal(entry, &envelope); err != nil {
			return err
		}
		list := bytes.TrimSpace(envelope.ProcessList)
		if len(list) == 0 || bytes.Equal(list, []byte("null")) {
			return nil
		}
		remaining := maxProcessRows - totalProcesses
		count, err := scanJSONArray(list, remaining, maxProcessEntryBytes, "process list", nil)
		if err != nil {
			return err
		}
		totalProcesses += count
		return nil
	})
	return err
}

func validateSMIJSONComplexity(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	tokens := 0
	containers := 0
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if depth != 0 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
		tokens++
		if tokens > maxSMIJSONTokens {
			return fmt.Errorf("SMI JSON exceeds %d tokens", maxSMIJSONTokens)
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch delimiter {
		case '{', '[':
			depth++
			containers++
			if depth > maxSMIJSONDepth {
				return fmt.Errorf("SMI JSON exceeds nesting depth %d", maxSMIJSONDepth)
			}
			if containers > maxSMIJSONContainers {
				return fmt.Errorf("SMI JSON exceeds %d containers", maxSMIJSONContainers)
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return fmt.Errorf("SMI JSON has an unexpected closing delimiter")
			}
		}
	}
}

func metricEntriesJSON(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	if trimmed[0] == '[' {
		return trimmed, nil
	}
	var envelope struct {
		GPUData json.RawMessage `json:"gpu_data"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, err
	}
	entries := bytes.TrimSpace(envelope.GPUData)
	if len(entries) == 0 {
		return nil, fmt.Errorf("metric response is missing gpu_data")
	}
	if bytes.Equal(entries, []byte("null")) {
		return []byte("[]"), nil
	}
	return entries, nil
}

func scanJSONArray(data []byte, maximum, maxEntryBytes int, kind string, visit func(json.RawMessage) error) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if token != json.Delim('[') {
		return 0, fmt.Errorf("%s must be a JSON array", kind)
	}
	count := 0
	for decoder.More() {
		count++
		if count > maximum {
			return 0, fmt.Errorf("%s exceeds %d entries", kind, maximum)
		}
		var entry json.RawMessage
		if err := decoder.Decode(&entry); err != nil {
			return 0, err
		}
		if len(entry) > maxEntryBytes {
			return 0, fmt.Errorf("%s entry exceeds %d bytes", kind, maxEntryBytes)
		}
		if visit != nil {
			if err := visit(entry); err != nil {
				return 0, err
			}
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%s has an invalid closing delimiter", kind)
	}
	if err := ensureJSONEOF(decoder, kind); err != nil {
		return 0, err
	}
	return count, nil
}

func scanJSONObject(data []byte, maximum, maxEntryBytes int, kind string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("%s must be a JSON object", kind)
	}
	seenFields := make(map[string]struct{})
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return fmt.Errorf("%s contains a non-string field", kind)
		}
		if _, duplicate := seenFields[field]; duplicate {
			return fmt.Errorf("%s contains duplicate field %q", kind, field)
		}
		seenFields[field] = struct{}{}
		if len(seenFields) > maximum {
			return fmt.Errorf("%s exceeds %d entries", kind, maximum)
		}
		var entry json.RawMessage
		if err := decoder.Decode(&entry); err != nil {
			return err
		}
		if len(entry) > maxEntryBytes {
			return fmt.Errorf("%s entry exceeds %d bytes", kind, maxEntryBytes)
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		if err != nil {
			return err
		}
		return fmt.Errorf("%s has an invalid closing delimiter", kind)
	}
	return ensureJSONEOF(decoder, kind)
}

func ensureJSONEOF(decoder *json.Decoder, kind string) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%s must contain one JSON value", kind)
}

func addGPU(seen map[int]struct{}, gpu int, kind string) error {
	if _, duplicate := seen[gpu]; duplicate {
		return fmt.Errorf("%s contains duplicate gpu %d", kind, gpu)
	}
	seen[gpu] = struct{}{}
	return nil
}

func mergeGPUMetrics(primary, secondary []model.GPUMetric) []model.GPUMetric {
	byGPU := make(map[int]model.GPUMetric, len(primary)+len(secondary))
	for _, metric := range primary {
		byGPU[metric.GPU] = metric
	}
	for _, metric := range secondary {
		existing := byGPU[metric.GPU]
		if existing.MemoryUsedBytes == nil {
			existing.MemoryUsedBytes = metric.MemoryUsedBytes
		}
		if existing.MemoryTotalBytes == nil {
			existing.MemoryTotalBytes = metric.MemoryTotalBytes
		}
		if existing.UtilizationPct == nil {
			existing.UtilizationPct = metric.UtilizationPct
		}
		existing.GPU = metric.GPU
		byGPU[metric.GPU] = existing
	}
	out := make([]model.GPUMetric, 0, len(byGPU))
	for _, metric := range byGPU {
		out = append(out, metric)
	}
	return out
}

func metricsNeedTotal(metrics []model.GPUMetric) bool {
	if len(metrics) == 0 {
		return true
	}
	for _, metric := range metrics {
		if metric.MemoryTotalBytes == nil {
			return true
		}
	}
	return false
}

func metricsComplete(metrics []model.GPUMetric) bool {
	if len(metrics) == 0 {
		return false
	}
	for _, metric := range metrics {
		if metric.MemoryUsedBytes == nil || metric.MemoryTotalBytes == nil || metric.UtilizationPct == nil {
			return false
		}
	}
	return true
}

func parseRocmCardID(card string, values map[string]any) (int, error) {
	if gpu, err := gpuNumber(firstValue(values, "GPU", "gpu", "GPU ID")); err == nil {
		return gpu, nil
	}
	idText := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(card)), "card")
	gpu, err := strconv.Atoi(idText)
	if err != nil {
		return 0, fmt.Errorf("parse rocm-smi card id %q: %w", card, err)
	}
	if gpu < 0 || gpu > maxGPUIndex {
		return 0, fmt.Errorf("parse rocm-smi card id %q: index outside 0..%d", card, maxGPUIndex)
	}
	return gpu, nil
}

func trimToJSON(data []byte) []byte {
	objectStart := bytes.IndexByte(data, '{')
	arrayStart := bytes.IndexByte(data, '[')
	start := objectStart
	if start < 0 || (arrayStart >= 0 && arrayStart < start) {
		start = arrayStart
	}
	objectEnd := bytes.LastIndexByte(data, '}')
	arrayEnd := bytes.LastIndexByte(data, ']')
	end := max(objectEnd, arrayEnd)
	if start >= 0 && end >= start {
		return data[start : end+1]
	}
	return data
}

func trimToJSONArray(data []byte) []byte {
	start := bytes.IndexByte(data, '[')
	end := bytes.LastIndexByte(data, ']')
	if start >= 0 && end >= start {
		return data[start : end+1]
	}
	return data
}

func number(value any) (int, error) {
	switch v := value.(type) {
	case float64:
		integerLimit := math.Ldexp(1, strconv.IntSize-1)
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v >= integerLimit || v < -integerLimit {
			return 0, fmt.Errorf("invalid integer %v", v)
		}
		return int(v), nil
	case string:
		if v == "" || v == "N/A" {
			return 0, fmt.Errorf("not a number: %q", v)
		}
		return strconv.Atoi(v)
	default:
		return 0, fmt.Errorf("unsupported number type %T", value)
	}
}

func gpuNumber(value any) (int, error) {
	gpu, err := number(value)
	if err != nil {
		return 0, err
	}
	if gpu < 0 || gpu > maxGPUIndex {
		return 0, fmt.Errorf("gpu index %d is outside 0..%d", gpu, maxGPUIndex)
	}
	return gpu, nil
}

func object(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

func firstValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func bytesValue(value any) *uint64 {
	return bytesValueWithDefaultUnit(value, "MB")
}

func bytesValueWithDefaultUnit(value any, defaultUnit string) *uint64 {
	number, unit, ok := valueWithUnit(value, defaultUnit)
	if !ok {
		return nil
	}
	switch strings.ToUpper(unit) {
	case "", "MB", "MIB":
		number *= 1024 * 1024
	case "GB", "GIB":
		number *= 1024 * 1024 * 1024
	case "KB", "KIB":
		number *= 1024
	case "B":
	default:
		return nil
	}
	if number < 0 {
		number = 0
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number > float64(^uint64(0)) {
		return nil
	}
	result := uint64(number)
	return &result
}

func percentValue(value any) *float64 {
	number, _, ok := valueWithUnit(value, "%")
	if !ok {
		return nil
	}
	number = max(0, min(100, number))
	return &number
}

func valueWithUnit(value any, defaultUnit string) (float64, string, bool) {
	if nested, ok := object(value); ok {
		number, ok := floatNumber(nested["value"])
		if !ok {
			return 0, "", false
		}
		unit, _ := nested["unit"].(string)
		if strings.TrimSpace(unit) == "" {
			unit = defaultUnit
		}
		return number, unit, true
	}
	number, ok := floatNumber(value)
	return number, defaultUnit, ok
}

func floatNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	case string:
		v = strings.TrimSpace(v)
		if v == "" || v == "N/A" {
			return 0, false
		}
		v = strings.TrimSpace(strings.TrimSuffix(v, "%"))
		parsed, err := strconv.ParseFloat(v, 64)
		return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	default:
		return 0, false
	}
}
