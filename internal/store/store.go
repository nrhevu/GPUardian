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
	DefaultHardTTL  = 2 * time.Hour
	MaxHardTTL      = 8 * time.Hour
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

func (s *Store) RegisterSoftToken(rootKey, name string, now time.Time) (string, model.Token, error) {
	if ok, err := s.ValidateRootKey(rootKey); err != nil {
		return "", model.Token{}, err
	} else if !ok {
		return "", model.Token{}, ErrInvalidRootKey
	}
	tokenSecret, token := newToken(model.TokenModeClaimed, name, time.Time{}, now)
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

func (s *Store) RegisterHardReservations(rootKey, name string, gpus []int, ttlText string, now time.Time) (string, model.Token, []model.Reservation, error) {
	ttl, err := ParseTTL(ttlText, DefaultHardTTL, MaxHardTTL)
	if err != nil {
		return "", model.Token{}, nil, err
	}
	if len(gpus) == 0 {
		return "", model.Token{}, nil, fmt.Errorf("at least one gpu is required")
	}
	seen := map[int]bool{}
	for _, gpu := range gpus {
		if gpu < 0 {
			return "", model.Token{}, nil, fmt.Errorf("gpu must be >= 0")
		}
		if seen[gpu] {
			return "", model.Token{}, nil, fmt.Errorf("duplicate gpu %d", gpu)
		}
		seen[gpu] = true
	}
	if ok, err := s.ValidateRootKey(rootKey); err != nil {
		return "", model.Token{}, nil, err
	} else if !ok {
		return "", model.Token{}, nil, ErrInvalidRootKey
	}
	expiresAt := now.UTC().Add(ttl)
	tokenSecret, token := newToken(model.TokenModeReserved, name, expiresAt, now)
	reservations := make([]model.Reservation, 0, len(gpus))
	for _, gpu := range gpus {
		reservations = append(reservations, model.Reservation{
			ID:        NewReservationID(),
			GPU:       gpu,
			TokenHash: token.Hash,
			Holder:    token.Name,
			CreatedAt: now.UTC(),
			ExpiresAt: expiresAt,
			Active:    true,
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return "", model.Token{}, nil, err
	}
	s.state.Tokens = append(s.state.Tokens, token)
	s.state.Reservations = append(s.state.Reservations, reservations...)
	if err := s.saveLocked(); err != nil {
		return "", model.Token{}, nil, err
	}
	return tokenSecret, token, reservations, nil
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
	tokenSecret, token := newToken(model.TokenModeClaimed, name, now.UTC().Add(ttl), now)
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
		token.Mode = NormalizeTokenMode(token.Mode)
		if timeIsSet(token.ExpiresAt) && !now.Before(token.ExpiresAt) {
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
		Mode:      NormalizeTokenMode(token.Mode),
		CreatedAt: token.CreatedAt,
		ExpiresAt: timePtrIfSet(token.ExpiresAt),
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

func (s *Store) AddReservation(reservation model.Reservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	s.state.Reservations = append(s.state.Reservations, reservation)
	return s.saveLocked()
}

func (s *Store) ReleaseReservation(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	for i := range s.state.Reservations {
		if s.state.Reservations[i].ID == id {
			s.state.Reservations[i].Active = false
			return s.saveLocked()
		}
	}
	return nil
}

func (s *Store) AddAuthorization(authorization model.Authorization) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	s.state.Authorizations = append(s.state.Authorizations, authorization)
	return s.saveLocked()
}

func (s *Store) UpdateAuthorization(update model.Authorization) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	for i := range s.state.Authorizations {
		if s.state.Authorizations[i].ID == update.ID {
			s.state.Authorizations[i] = update
			return s.saveLocked()
		}
	}
	return fmt.Errorf("authorization %s not found", update.ID)
}

func (s *Store) ReleaseAuthorization(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	for i := range s.state.Authorizations {
		if s.state.Authorizations[i].ID == id {
			s.state.Authorizations[i].Active = false
			return s.saveLocked()
		}
	}
	return nil
}

func (s *Store) UpsertSoftClaim(claim model.SoftClaim, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	if claim.ID == "" {
		claim.ID = NewSoftClaimID()
	}
	if claim.CreatedAt.IsZero() {
		claim.CreatedAt = now.UTC()
	}
	claim.UpdatedAt = now.UTC()
	for i := range s.state.SoftClaims {
		if s.state.SoftClaims[i].GPU == claim.GPU && s.state.SoftClaims[i].TokenHash == claim.TokenHash {
			claim.ID = s.state.SoftClaims[i].ID
			claim.CreatedAt = s.state.SoftClaims[i].CreatedAt
			s.state.SoftClaims[i] = claim
			return s.saveLocked()
		}
	}
	s.state.SoftClaims = append(s.state.SoftClaims, claim)
	return s.saveLocked()
}

func (s *Store) ReleaseSoftClaim(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	filtered := s.state.SoftClaims[:0]
	changed := false
	for _, claim := range s.state.SoftClaims {
		if claim.ID == id {
			changed = true
			continue
		}
		filtered = append(filtered, claim)
	}
	if !changed {
		return nil
	}
	s.state.SoftClaims = filtered
	return s.saveLocked()
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
	tokens := s.state.Tokens[:0]
	for _, token := range s.state.Tokens {
		if token.ID == idOrToken || token.Hash == tokenHash {
			tokenHash = token.Hash
			changed = true
			continue
		}
		tokens = append(tokens, token)
	}
	s.state.Tokens = tokens

	reservations := s.state.Reservations[:0]
	for _, reservation := range s.state.Reservations {
		if reservation.ID == idOrToken || (tokenHash != "" && reservation.TokenHash == tokenHash) {
			changed = true
			continue
		}
		reservations = append(reservations, reservation)
	}
	s.state.Reservations = reservations

	authorizations := s.state.Authorizations[:0]
	for _, authorization := range s.state.Authorizations {
		if authorization.ID == idOrToken || (tokenHash != "" && authorization.TokenHash == tokenHash) {
			changed = true
			continue
		}
		authorizations = append(authorizations, authorization)
	}
	s.state.Authorizations = authorizations

	leases := s.state.Leases[:0]
	for _, lease := range s.state.Leases {
		if lease.ID == idOrToken {
			changed = true
			continue
		}
		leases = append(leases, lease)
	}
	s.state.Leases = leases

	bypasses := s.state.Bypasses[:0]
	for _, bypass := range s.state.Bypasses {
		if bypass.ID == idOrToken {
			changed = true
			continue
		}
		bypasses = append(bypasses, bypass)
	}
	s.state.Bypasses = bypasses

	if tokenHash != "" || changed {
		filtered := s.state.SoftClaims[:0]
		for _, claim := range s.state.SoftClaims {
			if claim.ID == idOrToken || claim.AuthorizationID == idOrToken || (tokenHash != "" && claim.TokenHash == tokenHash) {
				changed = true
				continue
			}
			filtered = append(filtered, claim)
		}
		s.state.SoftClaims = filtered
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
	if s.pruneExpiredLocked(now) {
		if err := s.saveLocked(); err != nil {
			return model.Status{}, err
		}
	}
	status := model.Status{Now: now.UTC()}
	activeTokenHashes := map[string]bool{}
	for _, token := range s.state.Tokens {
		if token.Revoked || tokenExpired(token, now) {
			continue
		}
		activeTokenHashes[token.Hash] = true
		status.Tokens = append(status.Tokens, model.TokenView{
			ID:        token.ID,
			Name:      token.Name,
			Mode:      NormalizeTokenMode(token.Mode),
			CreatedAt: token.CreatedAt,
			ExpiresAt: timePtrIfSet(token.ExpiresAt),
			Revoked:   token.Revoked,
		})
	}
	for _, reservation := range s.state.Reservations {
		if reservation.Active && !reservation.Revoked && now.Before(reservation.ExpiresAt) {
			status.Reservations = append(status.Reservations, model.ReservationView{
				ID:        reservation.ID,
				GPU:       reservation.GPU,
				Holder:    reservation.Holder,
				CreatedAt: reservation.CreatedAt,
				ExpiresAt: reservation.ExpiresAt,
				Active:    reservation.Active,
				Revoked:   reservation.Revoked,
			})
		}
	}
	activeAuthorizationIDs := map[string]bool{}
	for _, authorization := range s.state.Authorizations {
		if authorization.Active && !authorization.Revoked && !authorizationExpired(authorization, now) {
			activeAuthorizationIDs[authorization.ID] = true
			status.Authorizations = append(status.Authorizations, authorizationView(authorization))
		}
	}
	for _, claim := range s.state.SoftClaims {
		if !activeTokenHashes[claim.TokenHash] || !activeAuthorizationIDs[claim.AuthorizationID] {
			continue
		}
		status.SoftClaims = append(status.SoftClaims, model.SoftClaimView{
			ID:              claim.ID,
			GPU:             claim.GPU,
			AuthorizationID: claim.AuthorizationID,
			Holder:          claim.Holder,
			CreatedAt:       claim.CreatedAt,
			UpdatedAt:       claim.UpdatedAt,
		})
	}
	for _, lease := range s.state.Leases {
		if lease.Active && now.Before(lease.ExpiresAt) {
			status.Leases = append(status.Leases, lease)
		}
	}
	for _, bypass := range s.state.Bypasses {
		if bypass.Revoked || !now.Before(bypass.ExpiresAt) {
			continue
		}
		status.Bypasses = append(status.Bypasses, bypass)
	}
	return status, nil
}

func (s *Store) KeyStatus(rootKey string, now time.Time) (model.KeyStatus, error) {
	if ok, err := s.ValidateRootKey(rootKey); err != nil {
		return model.KeyStatus{}, err
	} else if !ok {
		return model.KeyStatus{}, ErrInvalidRootKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return model.KeyStatus{}, err
	}
	if s.pruneExpiredLocked(now) {
		if err := s.saveLocked(); err != nil {
			return model.KeyStatus{}, err
		}
	}
	status := model.KeyStatus{Now: now.UTC()}
	for _, token := range s.state.Tokens {
		if token.Revoked || tokenExpired(token, now) {
			continue
		}
		view := model.TokenView{
			ID:        token.ID,
			Name:      token.Name,
			Mode:      NormalizeTokenMode(token.Mode),
			CreatedAt: token.CreatedAt,
			ExpiresAt: timePtrIfSet(token.ExpiresAt),
			Revoked:   token.Revoked,
		}
		if token.Secret != "" {
			view.Key = token.Secret
			view.KeyStatus = model.TokenKeyStatusStored
		} else {
			view.KeyStatus = model.TokenKeyStatusNotStored
		}
		status.Tokens = append(status.Tokens, view)
	}
	for _, reservation := range s.state.Reservations {
		if reservation.Active && !reservation.Revoked && now.Before(reservation.ExpiresAt) {
			status.Reservations = append(status.Reservations, model.ReservationView{
				ID:        reservation.ID,
				GPU:       reservation.GPU,
				Holder:    reservation.Holder,
				CreatedAt: reservation.CreatedAt,
				ExpiresAt: reservation.ExpiresAt,
				Active:    reservation.Active,
				Revoked:   reservation.Revoked,
			})
		}
	}
	for _, authorization := range s.state.Authorizations {
		if authorization.Active && !authorization.Revoked && !authorizationExpired(authorization, now) {
			status.Authorizations = append(status.Authorizations, authorizationView(authorization))
		}
	}
	for _, bypass := range s.state.Bypasses {
		if bypass.Revoked || !now.Before(bypass.ExpiresAt) {
			continue
		}
		status.Bypasses = append(status.Bypasses, bypass)
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

func (s *Store) pruneExpiredLocked(now time.Time) bool {
	changed := false
	expiredTokenHashes := map[string]bool{}

	tokens := s.state.Tokens[:0]
	for _, token := range s.state.Tokens {
		if token.Revoked || tokenExpired(token, now) {
			if token.Hash != "" {
				expiredTokenHashes[token.Hash] = true
			}
			changed = true
			continue
		}
		tokens = append(tokens, token)
	}
	s.state.Tokens = tokens

	reservations := s.state.Reservations[:0]
	for _, reservation := range s.state.Reservations {
		if reservation.Revoked || !reservation.Active || !now.Before(reservation.ExpiresAt) || expiredTokenHashes[reservation.TokenHash] {
			changed = true
			continue
		}
		reservations = append(reservations, reservation)
	}
	s.state.Reservations = reservations

	authorizations := s.state.Authorizations[:0]
	for _, authorization := range s.state.Authorizations {
		if authorization.Revoked || !authorization.Active || authorizationExpired(authorization, now) || expiredTokenHashes[authorization.TokenHash] {
			changed = true
			continue
		}
		authorizations = append(authorizations, authorization)
	}
	s.state.Authorizations = authorizations

	leases := s.state.Leases[:0]
	for _, lease := range s.state.Leases {
		if !lease.Active || !now.Before(lease.ExpiresAt) || expiredTokenHashes[lease.TokenHash] {
			changed = true
			continue
		}
		leases = append(leases, lease)
	}
	s.state.Leases = leases

	bypasses := s.state.Bypasses[:0]
	for _, bypass := range s.state.Bypasses {
		if bypass.Revoked || !now.Before(bypass.ExpiresAt) {
			changed = true
			continue
		}
		bypasses = append(bypasses, bypass)
	}
	s.state.Bypasses = bypasses

	if len(expiredTokenHashes) > 0 || changed {
		activeAuthorizationIDs := map[string]bool{}
		for _, authorization := range s.state.Authorizations {
			activeAuthorizationIDs[authorization.ID] = true
		}
		claims := s.state.SoftClaims[:0]
		for _, claim := range s.state.SoftClaims {
			if expiredTokenHashes[claim.TokenHash] || !activeAuthorizationIDs[claim.AuthorizationID] {
				changed = true
				continue
			}
			claims = append(claims, claim)
		}
		s.state.SoftClaims = claims
	}

	return changed
}

func cloneState(state model.State) model.State {
	out := model.State{}
	out.Tokens = append(out.Tokens, state.Tokens...)
	out.Reservations = append(out.Reservations, state.Reservations...)
	out.Authorizations = append(out.Authorizations, state.Authorizations...)
	out.SoftClaims = append(out.SoftClaims, state.SoftClaims...)
	out.Leases = append(out.Leases, state.Leases...)
	out.Bypasses = append(out.Bypasses, state.Bypasses...)
	out.Audit = append(out.Audit, state.Audit...)
	return out
}

func newToken(mode, name string, expiresAt time.Time, now time.Time) (string, model.Token) {
	tokenSecret := "rg_" + randomHex(24)
	token := model.Token{
		ID:        "tok_" + randomHex(8),
		Hash:      HashToken(tokenSecret),
		Secret:    tokenSecret,
		Name:      strings.TrimSpace(name),
		Mode:      NormalizeTokenMode(mode),
		CreatedAt: now.UTC(),
		ExpiresAt: expiresAt.UTC(),
	}
	if expiresAt.IsZero() {
		token.ExpiresAt = time.Time{}
	}
	if token.Name == "" {
		token.Name = "anonymous"
	}
	return tokenSecret, token
}

func NormalizeTokenMode(mode string) string {
	mode = strings.TrimSpace(strings.ToLower(mode))
	switch mode {
	case "":
		return model.TokenModeClaimed
	default:
		return mode
	}
}

func authorizationView(authorization model.Authorization) model.AuthorizationView {
	return model.AuthorizationView{
		ID:               authorization.ID,
		Mode:             authorization.Mode,
		TokenMode:        NormalizeTokenMode(authorization.TokenMode),
		Holder:           authorization.Holder,
		UID:              authorization.UID,
		GID:              authorization.GID,
		Username:         authorization.Username,
		Command:          append([]string(nil), authorization.Command...),
		RootPID:          authorization.RootPID,
		ContainerID:      authorization.ContainerID,
		ContainerPattern: authorization.ContainerPattern,
		Namespace:        authorization.Namespace,
		CreatedAt:        authorization.CreatedAt,
		ExpiresAt:        timePtrIfSet(authorization.ExpiresAt),
		Active:           authorization.Active,
		Revoked:          authorization.Revoked,
	}
}

func authorizationExpired(authorization model.Authorization, now time.Time) bool {
	return timeIsSet(authorization.ExpiresAt) && !now.Before(authorization.ExpiresAt)
}

func tokenExpired(token model.Token, now time.Time) bool {
	return timeIsSet(token.ExpiresAt) && !now.Before(token.ExpiresAt)
}

func timeIsSet(value time.Time) bool {
	return !value.IsZero()
}

func timePtrIfSet(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	out := value
	return &out
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

func NewReservationID() string {
	return "res_" + randomHex(8)
}

func NewAuthorizationID() string {
	return "auth_" + randomHex(8)
}

func NewSoftClaimID() string {
	return "claim_" + randomHex(8)
}

func NewBypassID() string {
	return "bp_" + randomHex(8)
}
