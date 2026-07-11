package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
	mu   sync.Mutex
	path string
}

func NewRegistry(path string) *Registry {
	return &Registry{path: path}
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

func (r *Registry) loadLocked() ([]ServerRecord, error) {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var records []ServerRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func (r *Registry) saveLocked(records []ServerRecord) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	return os.Chmod(r.path, 0600)
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
