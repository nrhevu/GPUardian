package daemon

import (
	"context"
	"os"
	"sort"
	"time"

	"gpuardian/internal/gpusmi"
	"gpuardian/internal/model"
)

func (s *Server) Snapshot(ctx context.Context, now time.Time) (model.NodeSnapshot, error) {
	status, err := s.Store.Status(now)
	if err != nil {
		return model.NodeSnapshot{}, err
	}
	processes, _ := s.processesForRead(ctx)
	rows, _ := s.psWithProcesses(ctx, now, processes)
	metricsByGPU := map[int]model.GPUMetric{}
	if metrics, err := s.metricsForRead(ctx); err == nil {
		for _, metric := range metrics {
			idsMetric := metric
			metricsByGPU[idsMetric.GPU] = idsMetric
		}
	}
	devicesByGPU, _ := s.devicesForRead(ctx)
	hostname, _ := os.Hostname()

	ids := map[int]bool{}
	for gpu := 0; gpu < s.Cfg.GPUCount; gpu++ {
		ids[gpu] = true
	}
	processesByGPU := map[int][]model.GPUProcess{}
	for _, process := range processes {
		ids[process.GPU] = true
		processesByGPU[process.GPU] = append(processesByGPU[process.GPU], process)
	}
	for gpu := range metricsByGPU {
		ids[gpu] = true
	}
	activeReservationByGPU := map[int]model.ReservationView{}
	for _, reservation := range status.Reservations {
		ids[reservation.GPU] = true
		if reservationViewActiveAt(reservation, now) {
			activeReservationByGPU[reservation.GPU] = reservation
		}
	}
	claimByGPU := map[int]model.SoftClaimView{}
	for _, claim := range status.SoftClaims {
		ids[claim.GPU] = true
		claimByGPU[claim.GPU] = claim
	}
	for _, lease := range status.Leases {
		ids[lease.GPU] = true
	}

	gpuIDs := make([]int, 0, len(ids))
	for gpu := range ids {
		gpuIDs = append(gpuIDs, gpu)
	}
	sort.Ints(gpuIDs)

	gpus := make([]model.GPUSnapshot, 0, len(gpuIDs))
	for _, gpu := range gpuIDs {
		item := model.GPUSnapshot{
			ID:               gpu,
			State:            "available",
			MemoryUsedBytes:  metricsByGPU[gpu].MemoryUsedBytes,
			MemoryTotalBytes: metricsByGPU[gpu].MemoryTotalBytes,
			UtilizationPct:   metricsByGPU[gpu].UtilizationPct,
			Processes:        processesByGPU[gpu],
		}
		if info, ok := devicesByGPU[gpu]; ok {
			item.Vendor = info.Vendor
			item.Model = info.Model
			item.UUID = info.UUID
		}
		if reservation, ok := activeReservationByGPU[gpu]; ok {
			item.State = "reserved"
			copy := reservation
			item.Reservation = &copy
		} else if claim, ok := claimByGPU[gpu]; ok {
			item.State = "claimed"
			copy := claim
			item.Claim = &copy
		}
		gpus = append(gpus, item)
	}

	return model.NodeSnapshot{
		Now:            status.Now,
		Hostname:       hostname,
		GPUs:           gpus,
		Tokens:         status.Tokens,
		Reservations:   status.Reservations,
		Authorizations: status.Authorizations,
		SoftClaims:     status.SoftClaims,
		Leases:         status.Leases,
		Bypasses:       status.Bypasses,
		PS:             rows,
	}, nil
}

func (s *Server) metricsForRead(ctx context.Context) ([]model.GPUMetric, error) {
	provider, ok := s.GPU.(gpusmi.MetricsProvider)
	if !ok {
		return nil, nil
	}
	s.metricsReadMu.Lock()
	defer s.metricsReadMu.Unlock()
	if !s.metricsReadAt.IsZero() {
		return append([]model.GPUMetric(nil), s.metricsReadRows...), s.metricsReadErr
	}
	rows, err := provider.Metrics(ctx)
	s.metricsReadAt = time.Now()
	s.metricsReadRows = append(s.metricsReadRows[:0], rows...)
	s.metricsReadErr = err
	return append([]model.GPUMetric(nil), rows...), err
}

// devicesForRead returns the cached static device identity for this enforcement
// pass, or samples it from the GPU provider's DeviceProvider implementation
// (if any). A provider that does not implement DeviceProvider yields an empty
// map, so the snapshot simply omits vendor/model/UUID.
func (s *Server) devicesForRead(ctx context.Context) (map[int]gpusmi.DeviceInfo, error) {
	provider, ok := s.GPU.(gpusmi.DeviceProvider)
	if !ok {
		return nil, nil
	}
	s.devicesReadMu.Lock()
	defer s.devicesReadMu.Unlock()
	if !s.devicesReadAt.IsZero() {
		return s.devicesReadRows, s.devicesReadErr
	}
	rows, err := provider.Devices(ctx)
	s.devicesReadAt = time.Now()
	s.devicesReadRows = rows
	s.devicesReadErr = err
	return rows, err
}

func reservationViewActiveAt(reservation model.ReservationView, now time.Time) bool {
	startsAt := reservation.StartsAt
	if startsAt.IsZero() {
		startsAt = reservation.CreatedAt
	}
	return reservation.Active && !reservation.Revoked && !now.Before(startsAt) && now.Before(reservation.ExpiresAt)
}
