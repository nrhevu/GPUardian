// Package nvidiasmi shells out to nvidia-smi to report the set of host
// processes holding GPU memory, per-GPU telemetry, and static device
// identity. It mirrors the structure of internal/amdsmi and satisfies the
// vendor-neutral interfaces in internal/gpusmi (Provider, MetricsProvider,
// DeviceProvider) via Go's structural typing.
//
// nvidia-smi must be installed on the host (it ships with the NVIDIA driver).
// The daemon never opens /dev/nvidia* directly; container device passthrough
// is the caller's responsibility, exactly as the AMD path leaves
// /dev/kfd and /dev/dri passthrough to the container runtime.
package nvidiasmi

import (
	"bytes"
	"context"
	"encoding/csv"
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

const (
	maxSMIOutputBytes = 16 << 20
	maxGPUIndex       = 1023
	maxProcessRows    = 32768
)

// CLIProvider shells out to nvidia-smi. The zero value is not usable; use
// NewCLIProvider. runCommand is injectable for tests (nil falls back to
// exec.CommandContext).
type CLIProvider struct {
	Command    string
	Timeout    time.Duration
	runCommand func(context.Context, string, ...string) ([]byte, error)
}

func NewCLIProvider() CLIProvider {
	return CLIProvider{Command: "nvidia-smi", Timeout: 5 * time.Second}
}

// Processes reports the set of host processes currently holding GPU memory.
// It runs two nvidia-smi calls: first a gpu index<->uuid map, then the
// compute-app list joined on gpu_uuid. nvidia-smi reports used_memory in MiB;
// the returned MemBytes is converted to bytes. A process whose used_memory is
// unavailable is reported with MemBytesUnknown=true.
func (p CLIProvider) Processes(ctx context.Context) ([]model.GPUProcess, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := p.Command
	if command == "" {
		command = "nvidia-smi"
	}

	gpuOut, err := p.output(ctx, command, "--query-gpu=index,uuid", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	uuidToIndex, err := ParseGPUIndexUUIDCSV(gpuOut)
	if err != nil {
		return nil, err
	}
	if len(uuidToIndex) == 0 {
		return nil, nil
	}

	appsOut, err := p.output(ctx, command, "--query-compute-apps=gpu_uuid,pid,used_memory", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return ParseComputeAppsCSV(appsOut, uuidToIndex)
}

// Metrics reports per-GPU memory and utilization. nvidia-smi reports memory
// in MiB and utilization as a percentage; both are converted to the
// model.GPUMetric byte/percent conventions. Missing or N/A fields are left
// nil so the snapshot omits them.
func (p CLIProvider) Metrics(ctx context.Context) ([]model.GPUMetric, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := p.Command
	if command == "" {
		command = "nvidia-smi"
	}
	out, err := p.output(ctx, command,
		"--query-gpu=index,memory.total,memory.used,utilization.gpu",
		"--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return ParseMetricCSV(out)
}

// Devices reports static device identity (vendor/model/UUID) keyed by GPU
// index. It is best-effort: any failure returns an empty map so the snapshot
// simply omits the vendor/model/UUID fields rather than failing.
func (p CLIProvider) Devices(ctx context.Context) (map[int]gpusmi.DeviceInfo, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := p.Command
	if command == "" {
		command = "nvidia-smi"
	}
	out, err := p.output(ctx, command, "--query-gpu=index,name,uuid", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return ParseDeviceCSV(out)
}

func (p CLIProvider) output(ctx context.Context, command string, args ...string) ([]byte, error) {
	if p.runCommand != nil {
		return p.runCommand(ctx, command, args...)
	}
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > maxSMIOutputBytes {
		return nil, fmt.Errorf("%s output exceeds %d bytes", command, maxSMIOutputBytes)
	}
	return out, nil
}

// ParseGPUIndexUUIDCSV parses `nvidia-smi --query-gpu=index,uuid
// --format=csv,noheader,nounits` output into a uuid -> GPU index map.
// Rows with an unparseable index or empty/NA uuid are skipped.
func ParseGPUIndexUUIDCSV(data []byte) (map[string]int, error) {
	rows, err := parseCSVRows(data, maxGPUIndex+1)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		gpu, err := parseGPUIndex(row[0])
		if err != nil {
			continue
		}
		uuid := strings.TrimSpace(row[1])
		if uuid == "" || uuid == "N/A" {
			continue
		}
		out[uuid] = gpu
	}
	return out, nil
}

// ParseComputeAppsCSV parses `nvidia-smi --query-compute-apps=gpu_uuid,pid,used_memory
// --format=csv,noheader,nounits` output into GPUProcess rows, joining
// gpu_uuid to a GPU index via uuidToIndex. used_memory is MiB -> bytes.
func ParseComputeAppsCSV(data []byte, uuidToIndex map[string]int) ([]model.GPUProcess, error) {
	rows, err := parseCSVRows(data, maxProcessRows)
	if err != nil {
		return nil, err
	}
	var processes []model.GPUProcess
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		if len(processes) >= maxProcessRows {
			return nil, fmt.Errorf("compute-apps response exceeds %d rows", maxProcessRows)
		}
		uuid := strings.TrimSpace(row[0])
		gpu, ok := uuidToIndex[uuid]
		if !ok {
			// Process on a GPU whose index we could not resolve; skip rather
			// than guess — enforcement keys on the integer index.
			continue
		}
		pid, err := parsePID(row[1])
		if err != nil || pid <= 0 {
			continue
		}
		var mem uint64
		memUnknown := true
		if len(row) >= 3 {
			memStr := strings.TrimSpace(row[2])
			if memStr != "" && memStr != "N/A" {
				if mib, perr := strconv.ParseFloat(memStr, 64); perr == nil &&
					!math.IsNaN(mib) && !math.IsInf(mib, 0) && mib >= 0 {
					mem = uint64(mib * 1024 * 1024)
					memUnknown = false
				}
			}
		}
		processes = append(processes, model.GPUProcess{
			GPU:             gpu,
			PID:             pid,
			MemBytes:        mem,
			MemBytesUnknown: memUnknown,
		})
	}
	return processes, nil
}

// ParseMetricCSV parses `nvidia-smi --query-gpu=index,memory.total,memory.used,utilization.gpu
// --format=csv,noheader,nounits` output into GPUMetric rows. Memory is MiB ->
// bytes; utilization is a percentage. N/A fields are left nil.
func ParseMetricCSV(data []byte) ([]model.GPUMetric, error) {
	rows, err := parseCSVRows(data, maxGPUIndex+1)
	if err != nil {
		return nil, err
	}
	metrics := make([]model.GPUMetric, 0, len(rows))
	seen := make(map[int]struct{}, len(rows))
	for _, row := range rows {
		if len(row) < 1 {
			continue
		}
		gpu, err := parseGPUIndex(row[0])
		if err != nil {
			continue
		}
		if err := addGPU(seen, gpu, "metric response"); err != nil {
			return nil, err
		}
		metric := model.GPUMetric{GPU: gpu}
		if len(row) >= 2 {
			metric.MemoryTotalBytes = parseMibBytes(row[1])
		}
		if len(row) >= 3 {
			metric.MemoryUsedBytes = parseMibBytes(row[2])
		}
		if len(row) >= 4 {
			metric.UtilizationPct = parsePercent(row[3])
		}
		metrics = append(metrics, metric)
	}
	return metrics, nil
}

// ParseDeviceCSV parses `nvidia-smi --query-gpu=index,name,uuid
// --format=csv,noheader,nounits` output into a GPU index -> DeviceInfo map.
func ParseDeviceCSV(data []byte) (map[int]gpusmi.DeviceInfo, error) {
	rows, err := parseCSVRows(data, maxGPUIndex+1)
	if err != nil {
		return nil, err
	}
	out := make(map[int]gpusmi.DeviceInfo, len(rows))
	seen := make(map[int]struct{}, len(rows))
	for _, row := range rows {
		if len(row) < 1 {
			continue
		}
		gpu, err := parseGPUIndex(row[0])
		if err != nil {
			continue
		}
		if err := addGPU(seen, gpu, "device response"); err != nil {
			return nil, err
		}
		info := gpusmi.DeviceInfo{Vendor: "nvidia"}
		if len(row) >= 2 {
			if name := strings.TrimSpace(row[1]); name != "" && name != "N/A" {
				info.Model = name
			}
		}
		if len(row) >= 3 {
			if uuid := strings.TrimSpace(row[2]); uuid != "" && uuid != "N/A" {
				info.UUID = uuid
			}
		}
		out[gpu] = info
	}
	return out, nil
}

// parseCSVRows reads nvidia-smi CSV output (one row per line, comma-separated,
// no header) and returns the trimmed fields. It bounds the row count to
// protect against runaway output. Quoted fields containing commas are
// handled by encoding/csv; nvidia-smi rarely emits them but the parser is
// strict enough to reject malformed input rather than silently truncate.
func parseCSVRows(data []byte, maxRows int) ([][]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if len(data) > maxSMIOutputBytes {
		return nil, fmt.Errorf("nvidia-smi output exceeds %d bytes", maxSMIOutputBytes)
	}
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1 // nvidia-smi rows vary in column count across queries.
	rows := make([][]string, 0, 16)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse nvidia-smi csv: %w", err)
		}
		if len(rows) >= maxRows {
			return nil, fmt.Errorf("nvidia-smi response exceeds %d rows", maxRows)
		}
		trimmed := make([]string, len(row))
		for i, field := range row {
			trimmed[i] = strings.TrimSpace(field)
		}
		rows = append(rows, trimmed)
	}
	return rows, nil
}

func parseGPUIndex(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "N/A" {
		return 0, fmt.Errorf("empty gpu index")
	}
	gpu, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse gpu index %q: %w", value, err)
	}
	if gpu < 0 || gpu > maxGPUIndex {
		return 0, fmt.Errorf("gpu index %d is outside 0..%d", gpu, maxGPUIndex)
	}
	return gpu, nil
}

func parsePID(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "N/A" {
		return 0, fmt.Errorf("empty pid")
	}
	pid, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse pid %q: %w", value, err)
	}
	return pid, nil
}

func parseMibBytes(value string) *uint64 {
	value = strings.TrimSpace(value)
	if value == "" || value == "N/A" {
		return nil
	}
	mib, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(mib) || math.IsInf(mib, 0) || mib < 0 {
		return nil
	}
	bytes := uint64(mib * 1024 * 1024)
	return &bytes
}

func parsePercent(value string) *float64 {
	value = strings.TrimSpace(value)
	if value == "" || value == "N/A" {
		return nil
	}
	pct, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) {
		return nil
	}
	pct = math.Max(0, math.Min(100, pct))
	return &pct
}

func addGPU(seen map[int]struct{}, gpu int, kind string) error {
	if _, duplicate := seen[gpu]; duplicate {
		return fmt.Errorf("%s contains duplicate gpu %d", kind, gpu)
	}
	seen[gpu] = struct{}{}
	return nil
}
