package model

import "time"

const (
	ModeBare   = "bare"
	ModeDocker = "docker"
	ModeK8s    = "k8s"
	ModeUser   = "user"

	TokenModeReserved = "reserved"
	TokenModeClaimed  = "claimed"

	TokenKeyStatusStored    = "stored"
	TokenKeyStatusNotStored = "not_stored"

	BypassPID     = "pid"
	BypassCommand = "command"
)

type GPUProcess struct {
	GPU      int    `json:"gpu"`
	PID      int    `json:"pid"`
	Name     string `json:"name,omitempty"`
	MemBytes uint64 `json:"mem_bytes,omitempty"`
}

type GPUMetric struct {
	GPU              int      `json:"gpu"`
	MemoryUsedBytes  *uint64  `json:"memory_used_bytes,omitempty"`
	MemoryTotalBytes *uint64  `json:"memory_total_bytes,omitempty"`
	UtilizationPct   *float64 `json:"utilization_percent,omitempty"`
}

type ProcInfo struct {
	PID         int
	UID         int
	Username    string
	Cmdline     []string
	CommandPath string
	Cgroup      string
	ContainerID string
	StderrPath  string
}

type Token struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	Secret    string    `json:"secret,omitempty"`
	Name      string    `json:"name"`
	Mode      string    `json:"mode"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Revoked   bool      `json:"revoked,omitempty"`
}

type Reservation struct {
	ID        string    `json:"id"`
	GPU       int       `json:"gpu"`
	TokenHash string    `json:"token_hash"`
	Holder    string    `json:"holder"`
	Purpose   string    `json:"purpose,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	StartsAt  time.Time `json:"starts_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Active    bool      `json:"active"`
	Revoked   bool      `json:"revoked,omitempty"`
}

func ReservationStartsAt(reservation Reservation) time.Time {
	if !reservation.StartsAt.IsZero() {
		return reservation.StartsAt
	}
	return reservation.CreatedAt
}

func ReservationActiveAt(reservation Reservation, now time.Time) bool {
	return reservation.Active &&
		!reservation.Revoked &&
		!now.Before(ReservationStartsAt(reservation)) &&
		now.Before(reservation.ExpiresAt)
}

func ReservationOverlaps(reservation Reservation, startsAt, expiresAt time.Time) bool {
	if !reservation.Active || reservation.Revoked {
		return false
	}
	return ReservationStartsAt(reservation).Before(expiresAt) && startsAt.Before(reservation.ExpiresAt)
}

type Authorization struct {
	ID               string    `json:"id"`
	Mode             string    `json:"mode"`
	TokenHash        string    `json:"token_hash"`
	TokenMode        string    `json:"token_mode"`
	Holder           string    `json:"holder"`
	UID              int       `json:"uid,omitempty"`
	GID              int       `json:"gid,omitempty"`
	Username         string    `json:"username,omitempty"`
	Command          []string  `json:"command,omitempty"`
	RootPID          int       `json:"root_pid,omitempty"`
	CgroupPath       string    `json:"cgroup_path,omitempty"`
	CgroupRel        string    `json:"cgroup_rel,omitempty"`
	ContainerID      string    `json:"container_id,omitempty"`
	ContainerPattern string    `json:"container_pattern,omitempty"`
	Namespace        string    `json:"namespace,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	Active           bool      `json:"active"`
	Revoked          bool      `json:"revoked,omitempty"`
}

type SoftClaim struct {
	ID              string    `json:"id"`
	GPU             int       `json:"gpu"`
	TokenHash       string    `json:"token_hash"`
	AuthorizationID string    `json:"authorization_id"`
	Holder          string    `json:"holder"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
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
	Revoked   bool      `json:"revoked,omitempty"`
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
	Tokens         []Token         `json:"tokens"`
	Reservations   []Reservation   `json:"reservations,omitempty"`
	Authorizations []Authorization `json:"authorizations,omitempty"`
	SoftClaims     []SoftClaim     `json:"soft_claims,omitempty"`
	Leases         []Lease         `json:"leases,omitempty"`
	Bypasses       []BypassRule    `json:"bypasses"`
	Audit          []AuditEvent    `json:"audit"`
}

type Status struct {
	Now            time.Time           `json:"now"`
	Tokens         []TokenView         `json:"tokens,omitempty"`
	Reservations   []ReservationView   `json:"reservations,omitempty"`
	Authorizations []AuthorizationView `json:"authorizations,omitempty"`
	SoftClaims     []SoftClaimView     `json:"soft_claims,omitempty"`
	Leases         []Lease             `json:"leases,omitempty"`
	Bypasses       []BypassRule        `json:"bypasses,omitempty"`
}

type GPUSnapshot struct {
	ID               int              `json:"id"`
	State            string           `json:"state"`
	MemoryUsedBytes  *uint64          `json:"memory_used_bytes,omitempty"`
	MemoryTotalBytes *uint64          `json:"memory_total_bytes,omitempty"`
	UtilizationPct   *float64         `json:"utilization_percent,omitempty"`
	Processes        []GPUProcess     `json:"processes,omitempty"`
	Reservation      *ReservationView `json:"reservation,omitempty"`
	Claim            *SoftClaimView   `json:"claim,omitempty"`
}

type NodeSnapshot struct {
	Now            time.Time           `json:"now"`
	Hostname       string              `json:"hostname,omitempty"`
	GPUs           []GPUSnapshot       `json:"gpus"`
	Tokens         []TokenView         `json:"tokens,omitempty"`
	Reservations   []ReservationView   `json:"reservations,omitempty"`
	Authorizations []AuthorizationView `json:"authorizations,omitempty"`
	SoftClaims     []SoftClaimView     `json:"soft_claims,omitempty"`
	Leases         []Lease             `json:"leases,omitempty"`
	Bypasses       []BypassRule        `json:"bypasses,omitempty"`
	PS             []PSRow             `json:"ps,omitempty"`
}

type TokenView struct {
	ID        string     `json:"id"`
	Key       string     `json:"key,omitempty"`
	KeyStatus string     `json:"key_status,omitempty"`
	Name      string     `json:"name"`
	Mode      string     `json:"mode"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Revoked   bool       `json:"revoked,omitempty"`
}

type ReservationView struct {
	ID        string    `json:"id"`
	GroupID   string    `json:"group_id,omitempty"`
	GPU       int       `json:"gpu"`
	Holder    string    `json:"holder"`
	Purpose   string    `json:"purpose,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	StartsAt  time.Time `json:"starts_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Active    bool      `json:"active"`
	Revoked   bool      `json:"revoked,omitempty"`
}

type AuthorizationView struct {
	ID               string     `json:"id"`
	Mode             string     `json:"mode"`
	TokenMode        string     `json:"token_mode"`
	Holder           string     `json:"holder"`
	UID              int        `json:"uid,omitempty"`
	GID              int        `json:"gid,omitempty"`
	Username         string     `json:"username,omitempty"`
	Command          []string   `json:"command,omitempty"`
	RootPID          int        `json:"root_pid,omitempty"`
	ContainerID      string     `json:"container_id,omitempty"`
	ContainerPattern string     `json:"container_pattern,omitempty"`
	Namespace        string     `json:"namespace,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	Active           bool       `json:"active"`
	Revoked          bool       `json:"revoked,omitempty"`
}

type SoftClaimView struct {
	ID              string    `json:"id"`
	GPU             int       `json:"gpu"`
	AuthorizationID string    `json:"authorization_id"`
	Holder          string    `json:"holder"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type KeyStatus struct {
	Now            time.Time           `json:"now"`
	Tokens         []TokenView         `json:"tokens,omitempty"`
	Reservations   []ReservationView   `json:"reservations,omitempty"`
	Authorizations []AuthorizationView `json:"authorizations,omitempty"`
	Bypasses       []BypassRule        `json:"bypasses,omitempty"`
}

type RegisterResult struct {
	Token          string     `json:"token"`
	Mode           string     `json:"mode"`
	ReservationIDs []string   `json:"reservation_ids,omitempty"`
	GPUs           []int      `json:"gpus,omitempty"`
	StartsAt       *time.Time `json:"starts_at,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
}

type RunResult struct {
	AuthorizationID string `json:"authorization_id"`
	LeaseID         string `json:"lease_id,omitempty"`
	ExitCode        int    `json:"exit_code"`
}

type AllowResult struct {
	AuthorizationID  string     `json:"authorization_id"`
	LeaseID          string     `json:"lease_id,omitempty"`
	Mode             string     `json:"mode"`
	ContainerID      string     `json:"container_id,omitempty"`
	ContainerPattern string     `json:"container_pattern,omitempty"`
	Namespace        string     `json:"namespace,omitempty"`
	Username         string     `json:"username,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

type PSRow struct {
	ID      string `json:"id"`
	GPU     string `json:"gpu"`
	User    string `json:"user"`
	Command string `json:"command"`
}
