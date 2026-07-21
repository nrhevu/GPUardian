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
