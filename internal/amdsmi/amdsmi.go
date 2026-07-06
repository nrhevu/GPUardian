package amdsmi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"rocguardd/internal/model"
)

type Provider interface {
	Processes(ctx context.Context) ([]model.GPUProcess, error)
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
