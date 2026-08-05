package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	maxRegistryFileBytes = 2 << 20
	maxServerRecords     = 256
	maxServerNameBytes   = 128
	maxServerEndpoint    = 2048
	maxServerRootKey     = 256
)

type ServerRecord struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Endpoint      string    `json:"endpoint"`
	RootKey       string    `json:"root_key"`
	TLSSkipVerify bool      `json:"tls_skip_verify,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type PublicServerRecord struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Endpoint      string    `json:"endpoint"`
	TLSSkipVerify bool      `json:"tls_skip_verify,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Registry struct {
	mu                 sync.Mutex
	path               string
	allowInsecureNodes bool
	loaded             bool
	records            []ServerRecord
}

func NewRegistry(path string, allowInsecureNodes bool) *Registry {
	return &Registry{path: path, allowInsecureNodes: allowInsecureNodes}
}

func (r *Registry) List() ([]ServerRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked()
}

func (r *Registry) PublicList() ([]PublicServerRecord, error) {
	records, err := r.List()
	if err != nil {
		return nil, err
	}
	out := make([]PublicServerRecord, 0, len(records))
	for _, record := range records {
		out = append(out, publicServerRecord(record))
	}
	return out, nil
}

func (r *Registry) Get(id string) (ServerRecord, bool, error) {
	records, err := r.List()
	if err != nil {
		return ServerRecord{}, false, err
	}
	for _, record := range records {
		if record.ID == id {
			return record, true, nil
		}
	}
	return ServerRecord{}, false, nil
}

func (r *Registry) Upsert(record ServerRecord) (ServerRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.loadLocked()
	if err != nil {
		return ServerRecord{}, err
	}
	now := time.Now().UTC()
	if strings.TrimSpace(record.ID) == "" {
		record.ID = "srv_" + randomHex(8)
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	if strings.TrimSpace(record.Name) == "" {
		record.Name = record.Endpoint
	}
	if strings.TrimSpace(record.Endpoint) == "" {
		return ServerRecord{}, errors.New("endpoint is required")
	}
	if strings.TrimSpace(record.RootKey) == "" {
		return ServerRecord{}, errors.New("root key is required")
	}
	if err := validateServerRecord(record); err != nil {
		return ServerRecord{}, err
	}
	if _, err := joinURL(record.Endpoint, "/healthz", r.allowInsecureNodes); err != nil {
		return ServerRecord{}, err
	}
	found := false
	for i := range records {
		if records[i].ID == record.ID {
			record.CreatedAt = records[i].CreatedAt
			records[i] = record
			found = true
			break
		}
	}
	if !found {
		if len(records) >= maxServerRecords {
			return ServerRecord{}, fmt.Errorf("server limit reached: maximum %d", maxServerRecords)
		}
		records = append(records, record)
	}
	if err := r.saveLocked(records); err != nil {
		return ServerRecord{}, err
	}
	return record, nil
}

func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.loadLocked()
	if err != nil {
		return err
	}
	filtered := records[:0]
	found := false
	for _, record := range records {
		if record.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, record)
	}
	if !found {
		return os.ErrNotExist
	}
	return r.saveLocked(filtered)
}

func (r *Registry) Rename(id, name string) (ServerRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, err := r.loadLocked()
	if err != nil {
		return ServerRecord{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ServerRecord{}, errors.New("server name is required")
	}
	if len(name) > maxServerNameBytes {
		return ServerRecord{}, fmt.Errorf("server name must be at most %d bytes", maxServerNameBytes)
	}
	for i := range records {
		if records[i].ID != id {
			continue
		}
		records[i].Name = name
		records[i].UpdatedAt = time.Now().UTC()
		if err := r.saveLocked(records); err != nil {
			return ServerRecord{}, err
		}
		return records[i], nil
	}
	return ServerRecord{}, os.ErrNotExist
}

func (r *Registry) loadLocked() ([]ServerRecord, error) {
	if r.loaded {
		return append([]ServerRecord(nil), r.records...), nil
	}
	data, err := readPrivateFile(r.path, "registry file", maxRegistryFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		r.loaded = true
		r.records = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		r.loaded = true
		r.records = nil
		return nil, nil
	}
	var records []ServerRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	if len(records) > maxServerRecords {
		return nil, fmt.Errorf("registry contains %d records; maximum is %d", len(records), maxServerRecords)
	}
	for _, record := range records {
		if err := validateServerRecord(record); err != nil {
			return nil, err
		}
	}
	r.records = append([]ServerRecord(nil), records...)
	r.loaded = true
	return append([]ServerRecord(nil), records...), nil
}

func validateServerRecord(record ServerRecord) error {
	if len(record.Name) > maxServerNameBytes {
		return fmt.Errorf("server name must be at most %d bytes", maxServerNameBytes)
	}
	if len(record.Endpoint) > maxServerEndpoint {
		return fmt.Errorf("server endpoint must be at most %d bytes", maxServerEndpoint)
	}
	if len(record.RootKey) > maxServerRootKey {
		return fmt.Errorf("root key must be at most %d bytes", maxServerRootKey)
	}
	// Keep plaintext records readable so an administrator can remove them or
	// enable the explicit gateway opt-in. NodeClient enforces the configured
	// transport policy before constructing a request that carries a root key.
	if _, err := parseNodeEndpoint(record.Endpoint); err != nil {
		return err
	}
	return nil
}

func (r *Registry) saveLocked(records []ServerRecord) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	committed, err := writePrivateFile(r.path, append(data, '\n'))
	if committed {
		r.records = append([]ServerRecord(nil), records...)
		r.loaded = true
	}
	return err
}

func publicServerRecord(record ServerRecord) PublicServerRecord {
	return PublicServerRecord{
		ID:            record.ID,
		Name:          record.Name,
		Endpoint:      record.Endpoint,
		TLSSkipVerify: record.TLSSkipVerify,
		CreatedAt:     record.CreatedAt,
		UpdatedAt:     record.UpdatedAt,
	}
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}
