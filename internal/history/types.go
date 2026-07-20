package history

import (
	"encoding/json"
	"time"
)

const (
	OutcomeSuccess = "success"
	OutcomePartial = "partial"
	OutcomeFailed  = "failed"
	OutcomeAborted = "aborted"
)

type Artifact struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type Result struct {
	Outcome   string     `json:"outcome,omitempty"`
	Note      string     `json:"note,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Version   int        `json:"version"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type GPUSummary struct {
	GPU                int      `json:"gpu"`
	ObservedMS         int64    `json:"observed_ms"`
	BusyMS             int64    `json:"busy_ms"`
	AverageUtilization *float64 `json:"average_utilization_percent,omitempty"`
	Coverage           float64  `json:"coverage"`
	AverageMemoryBytes *float64 `json:"average_memory_used_bytes,omitempty"`
	PeakMemoryBytes    *uint64  `json:"peak_memory_used_bytes,omitempty"`
	ValidSamples       int64    `json:"valid_samples"`
	MissingSamples     int64    `json:"missing_samples"`
}

type MinuteRollup struct {
	GPU                int       `json:"gpu"`
	Minute             time.Time `json:"minute"`
	ObservedMS         int64     `json:"observed_ms"`
	BusyMS             int64     `json:"busy_ms"`
	AverageUtilization *float64  `json:"average_utilization_percent,omitempty"`
}

type AuthorizationScope struct {
	Mode      string     `json:"mode"`
	Holder    string     `json:"holder"`
	Selector  string     `json:"selector,omitempty"`
	Command   []string   `json:"command,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
}

type Session struct {
	ID             string               `json:"id"`
	ServerID       string               `json:"server_id"`
	ServerName     string               `json:"server_name"`
	NodeID         string               `json:"-"`
	Owner          string               `json:"owner"`
	ResultEditable bool                 `json:"result_editable"`
	Purpose        string               `json:"purpose,omitempty"`
	Source         string               `json:"source"`
	CreatedAt      time.Time            `json:"created_at"`
	StartsAt       time.Time            `json:"starts_at"`
	ExpiresAt      time.Time            `json:"expires_at"`
	RevokedAt      *time.Time           `json:"revoked_at,omitempty"`
	FinalizedAt    *time.Time           `json:"finalized_at,omitempty"`
	Status         string               `json:"status"`
	HistoryQuality string               `json:"history_quality"`
	GPUs           []int                `json:"gpus"`
	GPUSummaries   []GPUSummary         `json:"gpu_summaries,omitempty"`
	Timeline       []MinuteRollup       `json:"timeline,omitempty"`
	Authorizations []AuthorizationScope `json:"authorization_scopes,omitempty"`
	JobCount       int64                `json:"job_count"`
	FirstJobAt     *time.Time           `json:"first_job_at,omitempty"`
	LastJobAt      *time.Time           `json:"last_job_at,omitempty"`
	Result         Result               `json:"result"`
}

type Job struct {
	ID              string     `json:"id"`
	CursorID        string     `json:"-"`
	Source          string     `json:"source"`
	Mode            string     `json:"mode"`
	Holder          string     `json:"holder"`
	Command         []string   `json:"command,omitempty"`
	GPUs            []int      `json:"gpus,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	RootExitedAt    *time.Time `json:"root_exited_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	StartPrecision  string     `json:"start_precision,omitempty"`
	FinishPrecision string     `json:"finish_precision,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	Reason          string     `json:"reason,omitempty"`
}

type DashboardSummary struct {
	Sessions           int64    `json:"sessions"`
	ReservedGPUHours   float64  `json:"reserved_gpu_hours"`
	BusyGPUHours       float64  `json:"busy_gpu_hours"`
	BusyRatio          float64  `json:"busy_ratio"`
	AverageUtilization *float64 `json:"average_utilization_percent,omitempty"`
	TelemetryCoverage  float64  `json:"telemetry_coverage"`
	Jobs               int64    `json:"jobs"`
}

type SessionFilter struct {
	ServerID string
	Owner    string
	Status   string
	From     *time.Time
	To       *time.Time
	Limit    int
	BeforeMS int64
	BeforeID string
}

type SearchExpression struct {
	Groups []SearchGroup `json:"groups"`
}

type SearchGroup struct {
	Rules []SearchRule `json:"rules"`
}

type SearchRule struct {
	Field    string          `json:"field"`
	Operator string          `json:"operator"`
	Value    json.RawMessage `json:"value,omitempty"`
}
