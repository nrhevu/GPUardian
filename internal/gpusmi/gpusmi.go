// Package gpusmi defines the vendor-neutral GPU provider interfaces used by
// the daemon to sample running processes, telemetry metrics, and static
// device identity from the host's GPU management tooling.
//
// The concrete implementations live in vendor-specific packages
// (internal/amdsmi for amd-smi/rocm-smi, internal/nvidiasmi for nvidia-smi).
// Both satisfy these interfaces via Go's structural typing; the daemon wires
// one of them in based on the GPUARDIAN_GPU_VENDOR config (or auto-detection).
package gpusmi

import (
	"context"
	"sync"
	"time"

	"gpuardian/internal/model"
)

// Provider reports the set of host processes currently holding GPU memory.
// This is the primary input to the monitor-and-kill enforcement loop: the
// daemon samples Processes every enforcement tick and feeds the result to
// the authorizer, which kills any process not covered by an active
// reservation/lease/authorization on that GPU index.
type Provider interface {
	Processes(ctx context.Context) ([]model.GPUProcess, error)
}

// MetricsProvider reports per-GPU memory and utilization telemetry. It is
// optional: a Provider that does not also implement MetricsProvider simply
// yields nil metrics (the snapshot will omit memory/utilization fields).
type MetricsProvider interface {
	Metrics(ctx context.Context) ([]model.GPUMetric, error)
}

// DeviceInfo carries static identity for a single GPU. All fields are
// best-effort; the daemon populates the matching fields on model.GPUSnapshot
// only when the provider returns them.
type DeviceInfo struct {
	Vendor string
	Model  string
	UUID   string
}

// DeviceProvider reports static device identity keyed by GPU index. It is
// optional: a Provider that does not also implement DeviceProvider leaves the
// snapshot's vendor/model/UUID fields empty.
type DeviceProvider interface {
	Devices(ctx context.Context) (map[int]DeviceInfo, error)
}

// GPUProcessProvider is optionally implemented by providers that can sample a
// subset of GPUs without a full inventory query. The daemon uses it to
// re-verify GPU membership of the processes it is about to signal. Calls are
// never coalesced with other samples, so the result is always taken after the
// caller's request.
type GPUProcessProvider interface {
	ProcessesForGPUs(ctx context.Context, gpus []int) ([]model.GPUProcess, error)
}

// FreshProvider is optionally implemented by providers that coalesce
// concurrent Processes calls. FreshProcesses only shares an in-flight sample
// that started at or after notBefore, so a caller that must observe every
// process started before its request (for example the reservation busy check)
// never receives an older sample.
type FreshProvider interface {
	FreshProcesses(ctx context.Context, notBefore time.Time) ([]model.GPUProcess, error)
}

// ProcessFlight coalesces concurrent process samples so callers that arrive
// while a sample is in flight (the monitor, register, snapshot readers) share
// one SMI exec instead of each spawning their own.
//
// The shared sample runs detached from any single caller's cancellation and is
// bounded only by the timeout the sample function applies itself; a caller
// whose context ends simply stops waiting. Each caller receives its own copy of
// the rows. The zero value is ready to use.
type ProcessFlight struct {
	mu   sync.Mutex
	call *processCall
}

type processCall struct {
	started time.Time
	done    chan struct{}
	rows    []model.GPUProcess
	err     error
}

// Do returns the result of an in-flight sample that started at or after
// notBefore, or starts a new sample. A zero notBefore joins any in-flight
// sample.
func (f *ProcessFlight) Do(ctx context.Context, notBefore time.Time, sample func(context.Context) ([]model.GPUProcess, error)) ([]model.GPUProcess, error) {
	for {
		f.mu.Lock()
		call := f.call
		if call != nil && call.started.Before(notBefore) {
			// The in-flight sample predates the caller's request. Wait for it
			// and then start (or join) a fresh one rather than running a second
			// concurrent exec.
			f.mu.Unlock()
			select {
			case <-call.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if call == nil {
			call = &processCall{started: time.Now(), done: make(chan struct{})}
			f.call = call
			sampleCtx := context.WithoutCancel(ctx)
			go func() {
				rows, err := sample(sampleCtx)
				f.mu.Lock()
				call.rows, call.err = rows, err
				if f.call == call {
					f.call = nil
				}
				f.mu.Unlock()
				close(call.done)
			}()
		}
		f.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil || call.rows == nil {
				return nil, call.err
			}
			return append([]model.GPUProcess(nil), call.rows...), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
