package amdsmi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"gpuardian/internal/model"
)

const processQueryWorkers = 4

// AMD SMI 26.2.2 can repeat one GPU's process memory counters across
// every GPU in a bulk query. Separate CLI invocations preserve attribution.
// All queries share the caller's deadline; a partial sample is never returned
// as success because the daemon uses it to finalize jobs and enforce access.
func (p CLIProvider) processesPerGPU(ctx context.Context, command string) ([]model.GPUProcess, error) {
	out, err := p.output(ctx, command, "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("list GPUs: %w", err)
	}
	gpus, err := parseGPUIndexes(out)
	if err != nil {
		return nil, fmt.Errorf("list GPUs: %w", err)
	}
	type result struct {
		rows []model.GPUProcess
		err  error
	}
	results := make([]result, len(gpus))
	work := make(chan int, len(gpus))
	for i := range gpus {
		work <- i
	}
	close(work)
	var wg sync.WaitGroup
	for range min(processQueryWorkers, len(gpus)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if err := ctx.Err(); err != nil {
					results[i].err = err
					continue
				}
				results[i].rows, results[i].err = p.processesForGPU(ctx, command, gpus[i])
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []model.GPUProcess
	for i, result := range results {
		if result.err != nil {
			return nil, fmt.Errorf("GPU %d processes: %w", gpus[i], result.err)
		}
		if len(rows)+len(result.rows) > maxProcessRows {
			return nil, fmt.Errorf("process response exceeds %d rows", maxProcessRows)
		}
		rows = append(rows, result.rows...)
	}
	return rows, nil
}

func (p CLIProvider) processesForGPU(ctx context.Context, command string, gpu int) ([]model.GPUProcess, error) {
	out, err := p.output(ctx, command, "process", "--gpu", strconv.Itoa(gpu), "--json")
	if err != nil {
		return nil, err
	}
	ids, err := parseGPUIndexes(out)
	if err != nil {
		return nil, err
	}
	if len(ids) != 1 || ids[0] != gpu {
		return nil, fmt.Errorf("expected only GPU %d, got %v", gpu, ids)
	}
	return ParseProcessJSON(out)
}

func parseGPUIndexes(data []byte) ([]int, error) {
	data = trimToJSONArray(data)
	if err := validateSMIJSONComplexity(data); err != nil {
		return nil, err
	}
	var entries []struct {
		GPU any `json:"gpu"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	if len(entries) > maxGPUIndex+1 {
		return nil, fmt.Errorf("GPU list exceeds %d entries", maxGPUIndex+1)
	}
	if entries == nil {
		return nil, fmt.Errorf("expected GPU array")
	}
	seen := make(map[int]struct{}, len(entries))
	ids := make([]int, 0, len(entries))
	for _, entry := range entries {
		gpu, err := gpuNumber(entry.GPU)
		if err != nil {
			return nil, err
		}
		if err := addGPU(seen, gpu, "GPU list"); err != nil {
			return nil, err
		}
		ids = append(ids, gpu)
	}
	sort.Ints(ids)
	return ids, nil
}
