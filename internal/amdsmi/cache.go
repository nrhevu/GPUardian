package amdsmi

import (
	"sync"
	"time"

	"gpuardian/internal/gpusmi"
	"gpuardian/internal/model"
)

const (
	// gpuListTTL bounds how long the amd-smi list inventory is reused before
	// it is queried again even without any sign of change. Any per-GPU query
	// error, an unexpected GPU in a response, or a metric sample naming an
	// unknown GPU invalidates it immediately.
	gpuListTTL = 2 * time.Minute
	// metricSourceTTL bounds how long the node is remembered as needing
	// rocm-smi instead of amd-smi metric before amd-smi is probed again.
	metricSourceTTL = 10 * time.Minute
)

// cliCache holds state shared by every copy of a CLIProvider built with
// NewCLIProvider. All methods are safe on a nil receiver, which disables
// caching and sample coalescing.
type cliCache struct {
	flight gpusmi.ProcessFlight
	now    func() time.Time

	mu            sync.Mutex
	gpus          []int
	gpusAt        time.Time
	static        map[int]model.GPUMetric
	rocmOnlyUntil time.Time
}

func newCLICache() *cliCache {
	return &cliCache{now: time.Now}
}

func (c *cliCache) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *cliCache) cachedGPUs() ([]int, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gpus == nil || c.nowTime().Sub(c.gpusAt) >= gpuListTTL {
		return nil, false
	}
	return append([]int(nil), c.gpus...), true
}

func (c *cliCache) storeGPUs(gpus []int) {
	if c == nil || len(gpus) == 0 {
		// An empty inventory (driver not loaded yet) is never reused.
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gpus = append([]int(nil), gpus...)
	c.gpusAt = c.nowTime()
}

func (c *cliCache) invalidateGPUs() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gpus = nil
}

// noteMetricGPUs drops the cached inventory when a metric sample reports a GPU
// that the inventory does not contain, so a newly visible GPU is enforced on
// the next process sample rather than after gpuListTTL.
func (c *cliCache) noteMetricGPUs(metrics []model.GPUMetric) {
	if c == nil || len(metrics) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gpus == nil {
		return
	}
	known := make(map[int]struct{}, len(c.gpus))
	for _, gpu := range c.gpus {
		known[gpu] = struct{}{}
	}
	for _, metric := range metrics {
		if _, ok := known[metric.GPU]; !ok {
			c.gpus = nil
			return
		}
	}
}

// staticTotals returns the cached static VRAM totals when they cover every GPU
// in metrics. VRAM size never changes for a device, so the static query only
// runs again when a GPU appears that the cache has not seen.
func (c *cliCache) staticTotals(metrics []model.GPUMetric) ([]model.GPUMetric, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.static) == 0 {
		return nil, false
	}
	for _, metric := range metrics {
		if _, ok := c.static[metric.GPU]; !ok {
			return nil, false
		}
	}
	out := make([]model.GPUMetric, 0, len(c.static))
	for _, metric := range c.static {
		out = append(out, cloneMetric(metric))
	}
	return out, true
}

func (c *cliCache) storeStatic(metrics []model.GPUMetric) {
	if c == nil {
		return
	}
	static := make(map[int]model.GPUMetric, len(metrics))
	for _, metric := range metrics {
		if metric.MemoryTotalBytes != nil {
			static[metric.GPU] = model.GPUMetric{GPU: metric.GPU, MemoryTotalBytes: cloneMetric(metric).MemoryTotalBytes}
		}
	}
	if len(static) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.static = static
}

func (c *cliCache) rocmOnly() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nowTime().Before(c.rocmOnlyUntil)
}

func (c *cliCache) setRocmOnly(enabled bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if enabled {
		c.rocmOnlyUntil = c.nowTime().Add(metricSourceTTL)
	} else {
		c.rocmOnlyUntil = time.Time{}
	}
}

func cloneMetric(metric model.GPUMetric) model.GPUMetric {
	out := model.GPUMetric{GPU: metric.GPU}
	if metric.MemoryUsedBytes != nil {
		value := *metric.MemoryUsedBytes
		out.MemoryUsedBytes = &value
	}
	if metric.MemoryTotalBytes != nil {
		value := *metric.MemoryTotalBytes
		out.MemoryTotalBytes = &value
	}
	if metric.UtilizationPct != nil {
		value := *metric.UtilizationPct
		out.UtilizationPct = &value
	}
	return out
}
