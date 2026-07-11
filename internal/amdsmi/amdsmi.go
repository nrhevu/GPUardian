package amdsmi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"rocguard/internal/model"
)

type Provider interface {
	Processes(ctx context.Context) ([]model.GPUProcess, error)
}

type MetricsProvider interface {
	Metrics(ctx context.Context) ([]model.GPUMetric, error)
}

type CLIProvider struct {
	Command string
	Timeout time.Duration
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
	out, err := exec.CommandContext(ctx, command, "process", "--json").Output()
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
	metricOut, metricErr := exec.CommandContext(ctx, command, "metric", "--mem-usage", "--usage", "--json").Output()
	staticOut, staticErr := exec.CommandContext(ctx, command, "static", "--vram", "--json").Output()
	var rocmOut []byte
	var rocmErr error
	tryRocm := command == "amd-smi" || strings.HasSuffix(command, "/amd-smi")
	if tryRocm {
		rocmOut, rocmErr = exec.CommandContext(ctx, "rocm-smi", "--showmeminfo", "vram", "--showuse", "--json").Output()
	}

	var metrics []model.GPUMetric
	var parseErr error
	if metricErr == nil {
		metrics, parseErr = ParseMetricJSON(metricOut)
	}
	if staticErr == nil {
		staticMetrics, err := ParseStaticJSON(staticOut)
		if err == nil {
			metrics = mergeGPUMetrics(metrics, staticMetrics)
			parseErr = nil
		} else if parseErr == nil {
			parseErr = err
		}
	}
	if tryRocm && rocmErr == nil {
		rocmMetrics, err := ParseRocmSMIJSON(rocmOut)
		if err == nil {
			metrics = mergeGPUMetrics(metrics, rocmMetrics)
			parseErr = nil
		} else if parseErr == nil {
			parseErr = err
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

func ParseProcessJSON(data []byte) ([]model.GPUProcess, error) {
	data = trimToJSONArray(data)
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
	var processes []model.GPUProcess
	for _, gpuEntry := range raw {
		gpu, err := number(gpuEntry.GPU)
		if err != nil {
			return nil, fmt.Errorf("parse gpu id: %w", err)
		}
		for _, process := range gpuEntry.ProcessList {
			pid, err := number(process.ProcessInfo.PID)
			if err != nil || pid <= 0 {
				continue
			}
			mem, _ := number(process.ProcessInfo.MemUsage.Value)
			processes = append(processes, model.GPUProcess{
				GPU:      gpu,
				PID:      pid,
				Name:     process.ProcessInfo.Name,
				MemBytes: uint64(max(0, mem)),
			})
		}
	}
	return processes, nil
}

func ParseMetricJSON(data []byte) ([]model.GPUMetric, error) {
	data = trimToJSON(data)
	var raw struct {
		GPUData []map[string]any `json:"gpu_data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	entries := raw.GPUData
	if len(entries) == 0 {
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, err
		}
	}

	metrics := make([]model.GPUMetric, 0, len(entries))
	for _, entry := range entries {
		gpu, err := number(entry["gpu"])
		if err != nil {
			return nil, fmt.Errorf("parse gpu id: %w", err)
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
	var raw struct {
		GPUData []map[string]any `json:"gpu_data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	entries := raw.GPUData
	if len(entries) == 0 {
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, err
		}
	}

	metrics := make([]model.GPUMetric, 0, len(entries))
	for _, entry := range entries {
		gpu, err := number(entry["gpu"])
		if err != nil {
			return nil, fmt.Errorf("parse gpu id: %w", err)
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
	var raw map[string]map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	metrics := make([]model.GPUMetric, 0, len(raw))
	for card, values := range raw {
		gpu, err := parseRocmCardID(card, values)
		if err != nil {
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

func parseRocmCardID(card string, values map[string]any) (int, error) {
	if gpu, err := number(firstValue(values, "GPU", "gpu", "GPU ID")); err == nil {
		return gpu, nil
	}
	idText := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(card)), "card")
	gpu, err := strconv.Atoi(idText)
	if err != nil {
		return 0, fmt.Errorf("parse rocm-smi card id %q: %w", card, err)
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
		return v, true
	case string:
		v = strings.TrimSpace(v)
		if v == "" || v == "N/A" {
			return 0, false
		}
		v = strings.TrimSpace(strings.TrimSuffix(v, "%"))
		parsed, err := strconv.ParseFloat(v, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
