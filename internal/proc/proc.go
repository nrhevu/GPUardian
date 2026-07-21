package proc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuardian/internal/model"
)

const maxCmdlineBytes = 64 << 10

type usernameCacheEntry struct {
	name      string
	err       error
	expiresAt time.Time
}

var usernameCache = struct {
	sync.Mutex
	entries map[int]usernameCacheEntry
}{entries: make(map[int]usernameCacheEntry)}

type Reader interface {
	Info(pid int) (model.ProcInfo, error)
	Exists(pid int) bool
}

type FSReader struct {
	Root string
}

func NewFSReader(root string) FSReader {
	if root == "" {
		root = "/proc"
	}
	return FSReader{Root: root}
}

func (r FSReader) Exists(pid int) bool {
	_, err := os.Stat(filepath.Join(r.Root, strconv.Itoa(pid)))
	return err == nil
}

func (r FSReader) Info(pid int) (model.ProcInfo, error) {
	base := filepath.Join(r.Root, strconv.Itoa(pid))
	startTime, err := readStartTime(filepath.Join(base, "stat"))
	if err != nil {
		return model.ProcInfo{}, err
	}
	cmdline, _ := readCmdline(filepath.Join(base, "cmdline"))
	commandPath, _ := os.Readlink(filepath.Join(base, "exe"))
	cgroupBytes, _ := os.ReadFile(filepath.Join(base, "cgroup"))
	statusBytes, _ := os.ReadFile(filepath.Join(base, "status"))
	confirmedStartTime, err := readStartTime(filepath.Join(base, "stat"))
	if err != nil {
		return model.ProcInfo{}, err
	}
	if confirmedStartTime != startTime {
		return model.ProcInfo{}, fmt.Errorf("process %d identity changed while reading proc metadata", pid)
	}
	stderrPath := filepath.Join(base, "fd", "2")
	uid := parseUID(string(statusBytes))
	info := model.ProcInfo{
		PID:         pid,
		StartTime:   startTime,
		UID:         uid,
		Cmdline:     cmdline,
		CommandPath: commandPath,
		Cgroup:      strings.TrimSpace(string(cgroupBytes)),
		StderrPath:  stderrPath,
	}
	info.ContainerID = ExtractContainerID(info.Cgroup)
	return info, nil
}

func readStartTime(path string) (uint64, error) {
	statBytes, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read process start time: %w", err)
	}
	startTime, err := parseStartTime(string(statBytes))
	if err != nil {
		return 0, fmt.Errorf("parse process start time: %w", err)
	}
	return startTime, nil
}

func parseStartTime(stat string) (uint64, error) {
	open := strings.IndexByte(stat, '(')
	close := strings.LastIndexByte(stat, ')')
	if open < 0 || close <= open {
		return 0, errors.New("malformed stat command")
	}
	fields := strings.Fields(stat[close+1:])
	const startTimeIndex = 19 // field 22, relative to field 3 after the command
	if len(fields) <= startTimeIndex {
		return 0, errors.New("stat is missing start time")
	}
	startTime, err := strconv.ParseUint(fields[startTimeIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid start time %q: %w", fields[startTimeIndex], err)
	}
	return startTime, nil
}

func LookupUsername(uid int) (string, error) {
	if uid < 0 {
		return "", errors.New("invalid uid")
	}
	now := time.Now()
	usernameCache.Lock()
	if entry, ok := usernameCache.entries[uid]; ok && now.Before(entry.expiresAt) {
		usernameCache.Unlock()
		return entry.name, entry.err
	}
	usernameCache.Unlock()
	u, err := user.LookupId(strconv.Itoa(uid))
	name := ""
	if err == nil {
		name = u.Username
	} else {
		var unknown user.UnknownUserIdError
		if errors.As(err, &unknown) {
			err = nil
		}
	}
	ttl := 5 * time.Minute
	if err != nil {
		ttl = 5 * time.Second
	}
	usernameCache.Lock()
	if len(usernameCache.entries) >= 4096 {
		for key, entry := range usernameCache.entries {
			if !now.Before(entry.expiresAt) {
				delete(usernameCache.entries, key)
			}
		}
		for len(usernameCache.entries) >= 4096 {
			for key := range usernameCache.entries {
				delete(usernameCache.entries, key)
				break
			}
		}
	}
	usernameCache.entries[uid] = usernameCacheEntry{name: name, err: err, expiresAt: now.Add(ttl)}
	usernameCache.Unlock()
	return name, err
}

func readCmdline(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCmdlineBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCmdlineBytes {
		return nil, fmt.Errorf("cmdline exceeds %d bytes", maxCmdlineBytes)
	}
	if len(data) == 0 {
		return nil, nil
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	var out []string
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out, nil
}

func parseUID(status string) int {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return -1
		}
		uid, err := strconv.Atoi(fields[1])
		if err != nil {
			return -1
		}
		return uid
	}
	return -1
}

var (
	dockerCgroupPattern = regexp.MustCompile(`(?i)^/(?:docker/([0-9a-f]{64})(?:/.*)?|system\.slice/docker-([0-9a-f]{64})\.scope(?:/.*)?)$`)
	// A Kubernetes runtime owns the cgroup immediately beneath the pod cgroup.
	// Descendants are delegated to the workload and cannot be trusted as
	// container identity, even if their names resemble runtime scopes.
	k8sPodChildPattern = regexp.MustCompile(`(?i)/(?:[^/]*-)?pod[^/]*/(?:(?:docker-|cri-containerd-|crio-)?([0-9a-f]{64})(?:\.scope)?)(?:/|$)`)
)

func ExtractContainerID(cgroup string) string {
	for _, line := range strings.Split(cgroup, "\n") {
		_, path, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		_, path, ok = strings.Cut(path, ":")
		if !ok {
			continue
		}
		patterns := []*regexp.Regexp{dockerCgroupPattern}
		if strings.HasPrefix(strings.TrimSpace(path), "/kubepods") {
			patterns = append(patterns, k8sPodChildPattern)
		}
		for _, pattern := range patterns {
			match := pattern.FindStringSubmatch(strings.TrimSpace(path))
			if len(match) < 2 {
				continue
			}
			for _, candidate := range match[1:] {
				if candidate != "" {
					return strings.ToLower(candidate)
				}
			}
		}
	}
	return ""
}
