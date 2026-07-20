package telemetry

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	SchemaVersion    = 1
	DefaultPageLimit = 128
	MaxPageLimit     = 256
	MaxPageBytes     = 1 << 20
	maxSegmentBytes  = 8 << 20
	maxSegmentAge    = time.Hour
	maxOutboxBytes   = 256 << 20
	maxRetention     = 24 * time.Hour
)

const (
	EventDaemonStarted       = "daemon.started"
	EventReservationUpsert   = "reservation.upsert"
	EventReservationEnded    = "reservation.ended"
	EventAuthorizationUpsert = "authorization.upsert"
	EventAuthorizationEnded  = "authorization.ended"
	EventJobStarted          = "job.started"
	EventJobUpdated          = "job.updated"
	EventJobFinished         = "job.finished"
	EventGPUSample           = "gpu.sample"
	EventGap                 = "telemetry.gap"
)

type Event struct {
	Seq        uint64          `json:"seq"`
	OccurredAt time.Time       `json:"occurred_at"`
	BootID     string          `json:"boot_id,omitempty"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}

type Info struct {
	NodeID          string   `json:"node_id"`
	BootID          string   `json:"boot_id,omitempty"`
	StreamID        string   `json:"stream_id"`
	TelemetrySchema int      `json:"telemetry_schema"`
	Capabilities    []string `json:"capabilities"`
}

type Page struct {
	SchemaVersion int     `json:"schema_version"`
	NodeID        string  `json:"node_id"`
	StreamID      string  `json:"stream_id"`
	BootID        string  `json:"boot_id,omitempty"`
	Events        []Event `json:"events"`
	NextCursor    string  `json:"next_cursor"`
	HasMore       bool    `json:"has_more"`
	EarliestSeq   uint64  `json:"earliest_seq"`
	LatestSeq     uint64  `json:"latest_seq"`
}

type CursorGap struct {
	Code            string    `json:"code"`
	Reason          string    `json:"reason"`
	StreamID        string    `json:"stream_id"`
	ResumeCursor    string    `json:"resume_cursor"`
	EarliestSeq     uint64    `json:"earliest_seq"`
	LatestSeq       uint64    `json:"latest_seq"`
	FirstRetainedAt time.Time `json:"first_retained_at,omitempty"`
}

func (g *CursorGap) Error() string { return "telemetry cursor gap: " + g.Reason }

var ErrInvalidCursor = errors.New("invalid telemetry cursor")

type ReservationMember struct {
	ReservationID string `json:"reservation_id"`
	GPU           int    `json:"gpu"`
}

type ReservationUpsert struct {
	ExternalSessionID string              `json:"external_session_id,omitempty"`
	HistoryQuality    string              `json:"history_quality,omitempty"`
	GroupID           string              `json:"group_id"`
	Holder            string              `json:"holder"`
	Purpose           string              `json:"purpose,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	StartsAt          time.Time           `json:"starts_at"`
	ExpiresAt         time.Time           `json:"expires_at"`
	Members           []ReservationMember `json:"members"`
}

type ReservationEnded struct {
	GroupID string    `json:"group_id"`
	EndedAt time.Time `json:"ended_at"`
	Reason  string    `json:"reason"`
}

type AuthorizationUpsert struct {
	AuthorizationID  string    `json:"authorization_id"`
	GroupID          string    `json:"group_id"`
	Mode             string    `json:"mode"`
	Holder           string    `json:"holder"`
	Command          []string  `json:"command,omitempty"`
	ContainerID      string    `json:"container_id,omitempty"`
	ContainerPattern string    `json:"container_pattern,omitempty"`
	Namespace        string    `json:"namespace,omitempty"`
	Username         string    `json:"username,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
}

type AuthorizationEnded struct {
	AuthorizationID string    `json:"authorization_id"`
	EndedAt         time.Time `json:"ended_at"`
	Reason          string    `json:"reason"`
}

type JobEvent struct {
	ExecutionID     string     `json:"execution_id"`
	AuthorizationID string     `json:"authorization_id"`
	GroupID         string     `json:"group_id"`
	Source          string     `json:"source"`
	Mode            string     `json:"mode"`
	Holder          string     `json:"holder"`
	PID             int        `json:"pid,omitempty"`
	ProcStartTicks  uint64     `json:"proc_start_ticks,omitempty"`
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

type GPUSample struct {
	WindowStart time.Time        `json:"window_start"`
	WindowEnd   time.Time        `json:"window_end"`
	Status      string           `json:"status"`
	GPUs        []GPUSampleEntry `json:"gpus"`
}

type GPUSampleEntry struct {
	GPU              int      `json:"gpu"`
	GroupID          string   `json:"group_id,omitempty"`
	UtilizationPct   *float64 `json:"utilization_percent,omitempty"`
	MemoryUsedBytes  *uint64  `json:"memory_used_bytes,omitempty"`
	MemoryTotalBytes *uint64  `json:"memory_total_bytes,omitempty"`
}

type Gap struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"`
}

type storedEvent struct {
	Event
	segment string
}

type segment struct {
	path    string
	first   uint64
	last    uint64
	size    int64
	created time.Time
}

type Outbox struct {
	mu       sync.Mutex
	dir      string
	nodeID   string
	streamID string
	bootID   string
	nextSeq  uint64
	events   []storedEvent
	segments []segment
	current  *os.File
}

type metadata struct {
	StreamID string `json:"stream_id"`
}

type cursor struct {
	Version  int    `json:"v"`
	StreamID string `json:"s"`
	Seq      uint64 `json:"q"`
}

func Open(nodeIDPath, dir, bootID string) (*Outbox, error) {
	nodeID, err := loadOrCreateID(nodeIDPath, "node_")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	box := &Outbox{dir: dir, nodeID: nodeID, bootID: bootID, nextSeq: 1}
	if err := box.load(); err != nil {
		return nil, err
	}
	return box, nil
}

func (o *Outbox) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.current == nil {
		return nil
	}
	err := o.current.Close()
	o.current = nil
	return err
}

func (o *Outbox) Info() Info {
	o.mu.Lock()
	defer o.mu.Unlock()
	return Info{
		NodeID: o.nodeID, BootID: o.bootID, StreamID: o.streamID, TelemetrySchema: SchemaVersion,
		Capabilities: []string{"telemetry_v1", "reservation_external_session_id", "job_tracking", "gpu_samples"},
	}
}

func (o *Outbox) Append(kind string, payload any, occurredAt time.Time) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	event := Event{Seq: o.nextSeq, OccurredAt: occurredAt.UTC(), BootID: o.bootID, Type: kind, Payload: raw}
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, err
	}
	line = append(line, '\n')
	if o.current == nil || len(o.segments) == 0 || o.segments[len(o.segments)-1].size+int64(len(line)) > maxSegmentBytes ||
		time.Since(o.segments[len(o.segments)-1].created) >= maxSegmentAge {
		if err := o.rotateLocked(event.Seq); err != nil {
			return Event{}, err
		}
	}
	if _, err := o.current.Write(line); err != nil {
		return Event{}, err
	}
	if err := o.current.Sync(); err != nil {
		return Event{}, err
	}
	index := len(o.segments) - 1
	o.segments[index].last = event.Seq
	o.segments[index].size += int64(len(line))
	o.events = append(o.events, storedEvent{Event: event, segment: o.segments[index].path})
	o.nextSeq++
	o.pruneLocked(time.Now().UTC())
	return event, nil
}

func (o *Outbox) Page(encoded string, limit int) (Page, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if limit <= 0 {
		limit = DefaultPageLimit
	}
	if limit > MaxPageLimit {
		limit = MaxPageLimit
	}
	earliest, latest := o.boundsLocked()
	position := uint64(0)
	if encoded != "" {
		decoded, err := decodeCursor(encoded)
		if err != nil || decoded.Version != SchemaVersion {
			return Page{}, ErrInvalidCursor
		}
		if decoded.StreamID != o.streamID {
			return Page{}, o.gapLocked("stream_reset", earliest, latest)
		}
		position = decoded.Seq
		if latest > 0 && position > latest {
			return Page{}, ErrInvalidCursor
		}
		if earliest > 0 && position+1 < earliest {
			return Page{}, o.gapLocked("retention", earliest, latest)
		}
	} else if earliest > 0 {
		position = earliest - 1
	}
	page := Page{SchemaVersion: SchemaVersion, NodeID: o.nodeID, StreamID: o.streamID, BootID: o.bootID, EarliestSeq: earliest, LatestSeq: latest}
	used := 512
	last := position
	for _, stored := range o.events {
		if stored.Seq <= position {
			continue
		}
		size := len(stored.Payload) + len(stored.Type) + 160
		if len(page.Events) >= limit || used+size > MaxPageBytes {
			break
		}
		page.Events = append(page.Events, stored.Event)
		used += size
		last = stored.Seq
	}
	page.NextCursor = encodeCursor(cursor{Version: SchemaVersion, StreamID: o.streamID, Seq: last})
	page.HasMore = last < latest
	return page, nil
}

func (o *Outbox) load() error {
	metaPath := filepath.Join(o.dir, "meta.json")
	data, err := os.ReadFile(metaPath)
	if errors.Is(err, os.ErrNotExist) {
		o.streamID = randomID("stream_")
		encoded, _ := json.Marshal(metadata{StreamID: o.streamID})
		if err := writeAtomic(metaPath, append(encoded, '\n'), 0600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		var meta metadata
		if json.Unmarshal(data, &meta) != nil || strings.TrimSpace(meta.StreamID) == "" {
			return errors.New("invalid telemetry metadata")
		}
		o.streamID = meta.StreamID
	}
	paths, err := filepath.Glob(filepath.Join(o.dir, "*.jsonl"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := o.loadSegment(path); err != nil {
			return err
		}
	}
	if len(o.events) > 0 {
		o.nextSeq = o.events[len(o.events)-1].Seq + 1
	}
	o.pruneLocked(time.Now().UTC())
	return nil
}

func (o *Outbox) loadSegment(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	seg := segment{path: path, size: info.Size(), created: info.ModTime().UTC()}
	scanner := bufio.NewScanner(io.LimitReader(file, maxSegmentBytes+1))
	scanner.Buffer(make([]byte, 64<<10), 512<<10)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Seq == 0 || event.Type == "" {
			return fmt.Errorf("invalid telemetry segment %s", filepath.Base(path))
		}
		if seg.first == 0 {
			seg.first = event.Seq
		}
		seg.last = event.Seq
		o.events = append(o.events, storedEvent{Event: event, segment: path})
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if seg.first != 0 {
		o.segments = append(o.segments, seg)
	}
	return nil
}

func (o *Outbox) rotateLocked(first uint64) error {
	if o.current != nil {
		if err := o.current.Close(); err != nil {
			return err
		}
	}
	path := filepath.Join(o.dir, fmt.Sprintf("%020d.jsonl", first))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	o.current = file
	o.segments = append(o.segments, segment{path: path, first: first, created: time.Now().UTC()})
	return nil
}

func (o *Outbox) pruneLocked(now time.Time) {
	total := int64(0)
	for _, seg := range o.segments {
		total += seg.size
	}
	cutoff := now.Add(-maxRetention)
	remove := map[string]bool{}
	for len(o.segments) > 1 {
		seg := o.segments[0]
		lastTime := seg.created
		for i := len(o.events) - 1; i >= 0; i-- {
			if o.events[i].segment == seg.path {
				lastTime = o.events[i].OccurredAt
				break
			}
		}
		if total <= maxOutboxBytes && !lastTime.Before(cutoff) {
			break
		}
		remove[seg.path] = true
		total -= seg.size
		_ = os.Remove(seg.path)
		o.segments = o.segments[1:]
	}
	if len(remove) > 0 {
		kept := o.events[:0]
		for _, event := range o.events {
			if !remove[event.segment] {
				kept = append(kept, event)
			}
		}
		o.events = kept
	}
}

func (o *Outbox) boundsLocked() (uint64, uint64) {
	if len(o.events) == 0 {
		return 0, 0
	}
	return o.events[0].Seq, o.events[len(o.events)-1].Seq
}

func (o *Outbox) gapLocked(reason string, earliest, latest uint64) *CursorGap {
	resume := uint64(0)
	first := time.Time{}
	if earliest > 0 {
		resume = earliest - 1
		first = o.events[0].OccurredAt
	}
	return &CursorGap{Code: "telemetry_cursor_gap", Reason: reason, StreamID: o.streamID, ResumeCursor: encodeCursor(cursor{Version: SchemaVersion, StreamID: o.streamID, Seq: resume}), EarliestSeq: earliest, LatestSeq: latest, FirstRetainedAt: first}
}

func encodeCursor(value cursor) string {
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(value string) (cursor, error) {
	if len(value) > 512 {
		return cursor{}, ErrInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor{}, err
	}
	var out cursor
	if err := json.Unmarshal(data, &out); err != nil || out.StreamID == "" {
		return cursor{}, ErrInvalidCursor
	}
	return out, nil
}

func loadOrCreateID(path, prefix string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		value := strings.TrimSpace(string(data))
		if value == "" || len(value) > 128 {
			return "", fmt.Errorf("invalid id in %s", path)
		}
		return value, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	value := randomID(prefix)
	if err := writeAtomic(path, []byte(value+"\n"), 0600); err != nil {
		return "", err
	}
	return value, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func randomID(prefix string) string {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(data)
}
