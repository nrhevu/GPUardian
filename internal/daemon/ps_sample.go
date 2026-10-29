package daemon

import (
	"context"
	"time"

	"gpuardian/internal/model"
	"gpuardian/internal/proc"
)

type fixedProcessProvider []model.GPUProcess

func (p fixedProcessProvider) Processes(context.Context) ([]model.GPUProcess, error) {
	return p, nil
}

// psWithProcesses runs the normal PS logic against an existing GPU process
// sample without mutating the live server or invoking the SMI provider again.
// procReader is the /proc memo bound to that sample (nil falls back to
// s.Proc).
func (s *Server) psWithProcesses(ctx context.Context, now time.Time, processes []model.GPUProcess, procReader proc.Reader) ([]model.PSRow, error) {
	sampled := &Server{
		Cfg:             s.Cfg,
		Store:           s.Store,
		GPU:             fixedProcessProvider(processes),
		Proc:            s.Proc,
		Runtime:         s.Runtime,
		Killer:          s.Killer,
		Interval:        s.Interval,
		bootID:          s.bootID,
		processReadAt:   time.Now(),
		processReadRows: append([]model.GPUProcess(nil), processes...),
	}
	if memo, ok := procReader.(*proc.Memo); ok && memo != nil {
		sampled.processReadProc = memo
	}
	return sampled.ps(ctx, now, "", true)
}
