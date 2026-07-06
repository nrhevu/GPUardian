package model

import "time"

const (
	ModeBare   = "bare"
	ModeDocker = "docker"
	ModeK8s    = "k8s"

	BypassPID     = "pid"
	BypassCommand = "command"
)

type GPUProcess struct {
	GPU      int    `json:"gpu"`
	PID      int    `json:"pid"`
	Name     string `json:"name,omitempty"`
	MemBytes uint64 `json:"mem_bytes,omitempty"`
}

type ProcInfo struct {
	PID         int
	UID         int
	Cmdline     []string
	CommandPath string
	Cgroup      string
	ContainerID string
	StderrPath  string
}

type Token struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}

type Lease struct {
	ID          string    `json:"id"`
	GPU         int       `json:"gpu"`
	Mode        string    `json:"mode"`
	TokenHash   string    `json:"token_hash"`
	Holder      string    `json:"holder"`
	UID         int       `json:"uid"`
	GID         int       `json:"gid"`
	Command     []string  `json:"command,omitempty"`
	RootPID     int       `json:"root_pid,omitempty"`
	CgroupPath  string    `json:"cgroup_path,omitempty"`
	CgroupRel   string    `json:"cgroup_rel,omitempty"`
	ContainerID string    `json:"container_id,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Active      bool      `json:"active"`
}

type BypassRule struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	PID       int       `json:"pid,omitempty"`
	Command   string    `json:"command,omitempty"`
	UID       int       `json:"uid,omitempty"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}

type AuditEvent struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
	GPU     int       `json:"gpu,omitempty"`
	PID     int       `json:"pid,omitempty"`
	LeaseID string    `json:"lease_id,omitempty"`
	User    string    `json:"user,omitempty"`
}

type State struct {
	Tokens   []Token      `json:"tokens"`
	Leases   []Lease      `json:"leases"`
	Bypasses []BypassRule `json:"bypasses"`
	Audit    []AuditEvent `json:"audit"`
}

type Status struct {
	Now      time.Time    `json:"now"`
	Tokens   []TokenView  `json:"tokens,omitempty"`
	Leases   []Lease      `json:"leases,omitempty"`
	Bypasses []BypassRule `json:"bypasses,omitempty"`
}

type TokenView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}

type RegisterResult struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type RunResult struct {
	LeaseID  string `json:"lease_id"`
	ExitCode int    `json:"exit_code"`
}

type AllowResult struct {
	LeaseID     string    `json:"lease_id"`
	Mode        string    `json:"mode"`
	GPU         int       `json:"gpu"`
	ContainerID string    `json:"container_id,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
}
