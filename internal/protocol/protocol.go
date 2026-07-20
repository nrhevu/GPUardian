package protocol

import (
	"encoding/json"
	"time"
)

type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Token  string          `json:"token,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
}

type Response struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Data   string          `json:"data,omitempty"`
}

const (
	KindResult = "result"
	KindStdout = "stdout"
	KindStderr = "stderr"
)

type RegisterArgs struct {
	RootKey           string     `json:"root_key"`
	Mode              string     `json:"mode"`
	Name              string     `json:"name"`
	Purpose           string     `json:"purpose,omitempty"`
	ExternalSessionID string     `json:"external_session_id,omitempty"`
	GPUs              []int      `json:"gpus,omitempty"`
	TTL               string     `json:"ttl"`
	StartsAt          *time.Time `json:"starts_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

type RunArgs struct {
	Command []string `json:"command"`
	Workdir string   `json:"workdir,omitempty"`
	Env     []string `json:"env,omitempty"`
}

type DockerAllowArgs struct {
	Container string `json:"container"`
}

type K8sAllowArgs struct {
	Namespace string `json:"namespace"`
}

type UserAllowArgs struct {
	User string `json:"user"`
}

type AllowArgs struct {
	ID        string `json:"id"`
	Mode      string `json:"mode"`
	Container string `json:"container,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	User      string `json:"user,omitempty"`
}

type WhoArgs struct {
	GPU int `json:"gpu"`
}

type TokenInfoArgs struct {
	Token string `json:"token"`
}

type BypassAddArgs struct {
	RootKey string `json:"root_key,omitempty"`
	Type    string `json:"type"`
	PID     int    `json:"pid,omitempty"`
	Command string `json:"command,omitempty"`
	UID     int    `json:"uid,omitempty"`
	TTL     string `json:"ttl"`
	Reason  string `json:"reason"`
}

type RevokeArgs struct {
	RootKey string `json:"root_key,omitempty"`
	ID      string `json:"id"`
}

type RootKeyArgs struct {
	RootKey string `json:"root_key"`
}
