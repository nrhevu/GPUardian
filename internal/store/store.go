package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rocguardd/internal/config"
	"rocguardd/internal/model"
)

const (
	DefaultTokenTTL = 2 * time.Hour
	MaxTokenTTL     = 24 * time.Hour
	maxAuditEvents  = 1000
)

var (
	ErrInvalidRootKey = errors.New("invalid root key")
	ErrTokenExpired   = errors.New("token expired")
	ErrTokenRevoked   = errors.New("token revoked")
	ErrTokenNotFound  = errors.New("token not found")
)

type Store struct {
	mu     sync.Mutex
	cfg    config.Config
	state  model.State
	loaded bool
}

func New(cfg config.Config) *Store {
	return &Store{cfg: cfg}
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func ParseTTL(value string, def, max time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return def, nil
	}
	ttl, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("ttl must be positive")
	}
	if ttl > max {
		return 0, fmt.Errorf("ttl exceeds max %s", max)
	}
	return ttl, nil
}

func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() error {
	if s.loaded {
		return nil
	}
	data, err := os.ReadFile(s.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		s.state = model.State{}
		s.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		s.state = model.State{}
	} else if err := json.Unmarshal(data, &s.state); err != nil {
		return err
	}
	s.loaded = true
	return nil
}

func (s *Store) Snapshot() (model.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return model.State{}, err
	}
	return cloneState(s.state), nil
}

func (s *Store) RegisterToken(rootKey, name, ttlText string, now time.Time) (string, model.Token, error) {
	ttl, err := ParseTTL(ttlText, DefaultTokenTTL, MaxTokenTTL)
	if err != nil {
		return "", model.Token{}, err
	}
	if ok, err := s.ValidateRootKey(rootKey); err != nil {
		return "", model.Token{}, err
	} else if !ok {
		return "", model.Token{}, ErrInvalidRootKey
	}
	tokenSecret := "rg_" + randomHex(24)
	token := model.Token{
		ID:        "tok_" + randomHex(8),
		Hash:      HashToken(tokenSecret),
		Name:      strings.TrimSpace(name),
		CreatedAt: now.UTC(),
		ExpiresAt: now.UTC().Add(ttl),
	}
	if token.Name == "" {
		token.Name = "anonymous"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return "", model.Token{}, err
	}
	s.state.Tokens = append(s.state.Tokens, token)
	if err := s.saveLocked(); err != nil {
		return "", model.Token{}, err
	}
	return tokenSecret, token, nil
}

func (s *Store) ValidateToken(secret string, now time.Time) (model.Token, string, error) {
	hash := HashToken(strings.TrimSpace(secret))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return model.Token{}, hash, err
	}
	for _, token := range s.state.Tokens {
		if token.Hash != hash {
			continue
		}
		if token.Revoked {
			return token, hash, ErrTokenRevoked
		}
		if !now.Before(token.ExpiresAt) {
			return token, hash, ErrTokenExpired
		}
		return token, hash, nil
	}
	return model.Token{}, hash, ErrTokenNotFound
}

func (s *Store) TokenView(secret string, now time.Time) (model.TokenView, error) {
	token, _, err := s.ValidateToken(secret, now)
	if err != nil {
		return model.TokenView{}, err
	}
	return model.TokenView{
		ID:        token.ID,
		Name:      token.Name,
		CreatedAt: token.CreatedAt,
		ExpiresAt: token.ExpiresAt,
		Revoked:   token.Revoked,
	}, nil
}

func (s *Store) AddLease(lease model.Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	s.state.Leases = append(s.state.Leases, lease)
	return s.saveLocked()
}

func (s *Store) UpdateLease(update model.Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	for i := range s.state.Leases {
		if s.state.Leases[i].ID == update.ID {
			s.state.Leases[i] = update
			return s.saveLocked()
		}
	}
	return fmt.Errorf("lease %s not found", update.ID)
}

func (s *Store) ReleaseLease(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	for i := range s.state.Leases {
		if s.state.Leases[i].ID == id {
			s.state.Leases[i].Active = false
			return s.saveLocked()
		}
	}
	return nil
}

func (s *Store) AddBypass(rule model.BypassRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	s.state.Bypasses = append(s.state.Bypasses, rule)
	return s.saveLocked()
}

func (s *Store) Revoke(idOrToken string) error {
	idOrToken = strings.TrimSpace(idOrToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	tokenHash := ""
	if strings.HasPrefix(idOrToken, "rg_") {
		tokenHash = HashToken(idOrToken)
	}
	changed := false
	for i := range s.state.Tokens {
		if s.state.Tokens[i].ID == idOrToken || s.state.Tokens[i].Hash == tokenHash {
			s.state.Tokens[i].Revoked = true
			changed = true
		}
	}
	for i := range s.state.Leases {
		if s.state.Leases[i].ID == idOrToken {
			s.state.Leases[i].Active = false
			changed = true
		}
	}
	for i := range s.state.Bypasses {
		if s.state.Bypasses[i].ID == idOrToken {
			s.state.Bypasses[i].Revoked = true
			changed = true
		}
	}
	if !changed {
		return fmt.Errorf("%s not found", idOrToken)
	}
	return s.saveLocked()
}

func (s *Store) AppendAudit(event model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	s.state.Audit = append(s.state.Audit, event)
	if len(s.state.Audit) > maxAuditEvents {
		s.state.Audit = s.state.Audit[len(s.state.Audit)-maxAuditEvents:]
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	return appendAuditLog(s.cfg.AuditLog, event)
}

func (s *Store) Status(now time.Time) (model.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return model.Status{}, err
	}
	status := model.Status{Now: now.UTC(), Bypasses: append([]model.BypassRule(nil), s.state.Bypasses...)}
	for _, token := range s.state.Tokens {
		status.Tokens = append(status.Tokens, model.TokenView{
			ID:        token.ID,
			Name:      token.Name,
			CreatedAt: token.CreatedAt,
			ExpiresAt: token.ExpiresAt,
			Revoked:   token.Revoked,
		})
	}
	for _, lease := range s.state.Leases {
		if lease.Active && now.Before(lease.ExpiresAt) {
			status.Leases = append(status.Leases, lease)
		}
	}
	return status, nil
}

func (s *Store) ReadOrCreateRootKey() (string, error) {
	if data, err := os.ReadFile(s.cfg.RootKeyPath); err == nil {
		return strings.TrimSpace(string(data)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	key := "rk_" + randomHex(32)
	if err := os.MkdirAll(filepath.Dir(s.cfg.RootKeyPath), 0700); err != nil {
		return "", err
	}
	tmp := s.cfg.RootKeyPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(key+"\n"), 0600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, s.cfg.RootKeyPath); err != nil {
		return "", err
	}
	_ = os.Chmod(s.cfg.RootKeyPath, 0600)
	return key, nil
}

func (s *Store) ValidateRootKey(candidate string) (bool, error) {
	key, err := s.ReadOrCreateRootKey()
	if err != nil {
		return false, err
	}
	a := []byte(strings.TrimSpace(candidate))
	b := []byte(strings.TrimSpace(key))
	if len(a) != len(b) {
		return false, nil
	}
	return subtle.ConstantTimeCompare(a, b) == 1, nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.StatePath), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.cfg.StatePath); err != nil {
		return err
	}
	_ = os.Chmod(s.cfg.StatePath, 0600)
	return nil
}

func appendAuditLog(path string, event model.AuditEvent) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(data, '\n'))
	return err
}

func cloneState(state model.State) model.State {
	out := model.State{}
	out.Tokens = append(out.Tokens, state.Tokens...)
	out.Leases = append(out.Leases, state.Leases...)
	out.Bypasses = append(out.Bypasses, state.Bypasses...)
	out.Audit = append(out.Audit, state.Audit...)
	return out
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func NewLeaseID() string {
	return "lease_" + randomHex(8)
}

func NewBypassID() string {
	return "bp_" + randomHex(8)
}
