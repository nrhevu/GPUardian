package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"gpuardian/internal/model"
)

type countingReader struct {
	infos       map[int]model.ProcInfo
	infoCalls   map[int]int
	existsCalls map[int]int
}

func (r *countingReader) Info(pid int) (model.ProcInfo, error) {
	r.infoCalls[pid]++
	info, ok := r.infos[pid]
	if !ok {
		return model.ProcInfo{}, fmt.Errorf("read process start time: %w", &fs.PathError{Op: "open", Path: "stat", Err: syscall.ENOENT})
	}
	return info, nil
}

func (r *countingReader) Exists(pid int) bool {
	r.existsCalls[pid]++
	_, ok := r.infos[pid]
	return ok
}

func TestMemoReadsEachPIDOnce(t *testing.T) {
	reader := &countingReader{
		infos:       map[int]model.ProcInfo{10: {PID: 10, StartTime: 1, Cmdline: []string{"python"}}},
		infoCalls:   map[int]int{},
		existsCalls: map[int]int{},
	}
	memo := NewMemo(reader)
	for i := 0; i < 3; i++ {
		info, err := memo.Info(10)
		if err != nil || info.StartTime != 1 {
			t.Fatalf("Info = %+v, %v", info, err)
		}
		info.Cmdline[0] = "mutated"
		if _, err := memo.Info(11); !IsNotExist(err) {
			t.Fatalf("missing pid err = %v", err)
		}
		if !memo.Exists(10) || memo.Exists(11) {
			t.Fatal("unexpected existence")
		}
	}
	if reader.infoCalls[10] != 1 || reader.infoCalls[11] != 1 {
		t.Fatalf("info calls = %v, want one per pid", reader.infoCalls)
	}
	if reader.existsCalls[10] != 0 || reader.existsCalls[11] != 1 {
		t.Fatalf("exists calls = %v, want none for a pid with cached info", reader.existsCalls)
	}
	if info, _ := memo.Info(10); info.Cmdline[0] != "python" {
		t.Fatalf("cached info was mutated through a returned copy: %+v", info)
	}
	if memo.Uncached() != Reader(reader) {
		t.Fatal("Uncached does not expose the underlying reader")
	}
}

func TestIsNotExistUnwrapsReaderErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "wrapped ENOENT", err: fmt.Errorf("read process start time: %w", &fs.PathError{Op: "open", Err: syscall.ENOENT}), want: true},
		{name: "os.ErrNotExist", err: os.ErrNotExist, want: true},
		{name: "exiting process", err: fmt.Errorf("read: %w", syscall.ESRCH), want: true},
		{name: "permission", err: fmt.Errorf("read: %w", syscall.EACCES), want: false},
		{name: "other", err: errors.New("identity changed"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNotExist(tc.err); got != tc.want {
				t.Fatalf("IsNotExist(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
