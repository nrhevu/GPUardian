package proc

import (
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"rocguardd/internal/model"
)

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
	if _, err := os.Stat(base); err != nil {
		return model.ProcInfo{}, err
	}
	cmdline, _ := readCmdline(filepath.Join(base, "cmdline"))
	cgroupBytes, _ := os.ReadFile(filepath.Join(base, "cgroup"))
	statusBytes, _ := os.ReadFile(filepath.Join(base, "status"))
	stderrPath := filepath.Join(base, "fd", "2")
	uid := parseUID(string(statusBytes))
	info := model.ProcInfo{
		PID:         pid,
		UID:         uid,
		Username:    lookupUsername(uid),
		Cmdline:     cmdline,
		CommandPath: first(cmdline),
		Cgroup:      strings.TrimSpace(string(cgroupBytes)),
		StderrPath:  stderrPath,
	}
	info.ContainerID = ExtractContainerID(info.Cgroup)
	return info, nil
}

func lookupUsername(uid int) string {
	if uid < 0 {
		return ""
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return ""
	}
	return u.Username
}

func readCmdline(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

var containerIDPattern = regexp.MustCompile(`(?i)([0-9a-f]{64})`)

func ExtractContainerID(cgroup string) string {
	match := containerIDPattern.FindStringSubmatch(cgroup)
	if len(match) < 2 {
		return ""
	}
	return strings.ToLower(match[1])
}

func WriteMessageToStderr(info model.ProcInfo, message string) error {
	if info.StderrPath == "" {
		info.StderrPath = filepath.Join("/proc", strconv.Itoa(info.PID), "fd", "2")
	}
	file, err := os.OpenFile(info.StderrPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(message)
	return err
}
