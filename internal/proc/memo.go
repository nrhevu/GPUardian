package proc

import (
	"errors"
	"io/fs"
	"sync"
	"syscall"

	"gpuardian/internal/model"
)

// maxMemoEntries bounds a Memo; later lookups pass through uncached.
const maxMemoEntries = 65536

// IsNotExist reports whether err from Reader.Info means the process is gone.
// Unlike os.IsNotExist it unwraps wrapped errors, and it also accepts ESRCH,
// which /proc reads can return for a process that is exiting.
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// Memo caches Reader results for the lifetime of one GPU process sample, so an
// enforcement pass (stale-scope eviction, enforcement, escalation) and the
// read-only views of the same sample read each PID's /proc metadata at most
// once. It must never back a check that needs a fresh read, such as the
// identity revalidation immediately before a signal; killers keep their own
// uncached Reader. Memo is safe for concurrent use.
type Memo struct {
	reader Reader

	mu     sync.Mutex
	infos  map[int]memoInfo
	exists map[int]bool
}

type memoInfo struct {
	info model.ProcInfo
	err  error
}

func NewMemo(reader Reader) *Memo {
	return &Memo{reader: reader, infos: make(map[int]memoInfo), exists: make(map[int]bool)}
}

// Uncached returns the underlying Reader for callers that need a fresh read.
func (m *Memo) Uncached() Reader {
	return m.reader
}

func (m *Memo) Info(pid int) (model.ProcInfo, error) {
	m.mu.Lock()
	cached, ok := m.infos[pid]
	m.mu.Unlock()
	if ok {
		return cloneProcInfo(cached.info), cached.err
	}
	info, err := m.reader.Info(pid)
	m.mu.Lock()
	if len(m.infos) < maxMemoEntries {
		m.infos[pid] = memoInfo{info: cloneProcInfo(info), err: err}
	}
	m.mu.Unlock()
	return info, err
}

func (m *Memo) Exists(pid int) bool {
	m.mu.Lock()
	if cached, ok := m.infos[pid]; ok && cached.err == nil {
		m.mu.Unlock()
		return true
	}
	exists, ok := m.exists[pid]
	m.mu.Unlock()
	if ok {
		return exists
	}
	exists = m.reader.Exists(pid)
	m.mu.Lock()
	if len(m.exists) < maxMemoEntries {
		m.exists[pid] = exists
	}
	m.mu.Unlock()
	return exists
}

func cloneProcInfo(info model.ProcInfo) model.ProcInfo {
	info.Cmdline = append([]string(nil), info.Cmdline...)
	return info
}
