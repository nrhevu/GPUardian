package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuardian/internal/config"
	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
)

const (
	DefaultTokenTTL   = 2 * time.Hour
	MaxTokenTTL       = 24 * time.Hour
	DefaultHardTTL    = 2 * time.Hour
	MaxHardTTL        = 24 * time.Hour
	maxAuditEvents    = 1000
	maxAuditLogBytes  = 10 << 20
	maxStateFileBytes = 64 << 20
	rootKeyHexBytes   = 32

	// Store admission limits keep the persisted state and its linear scans bounded
	// while leaving ample room for normal operation on a single GPU node.
	maxActiveTokensPerHolder        = 8
	maxTotalTokens                  = 4096
	maxTotalReservations            = 4096
	maxTotalAuthorizations          = 4096
	maxTotalSoftClaims              = 4096
	maxTotalLeases                  = 4096
	maxTotalBypasses                = 1024
	maxActiveAuthorizationsPerToken = 64
	maxGPUIndex                     = 1023
	maxPersistedValueBytes          = 4096
	maxPersistedCommandArgs         = 1024
	maxPersistedCommandBytes        = 256 << 10
	maxViewCommandArgs              = 128
	maxViewCommandBytes             = 16 << 10
	maxAuditMessageBytes            = 16 << 10
)

var (
	ErrInvalidRootKey = errors.New("invalid root key")
	ErrTokenExpired   = errors.New("token expired")
	ErrTokenRevoked   = errors.New("token revoked")
	ErrTokenNotFound  = errors.New("token not found")
)

type Store struct {
	mu           sync.Mutex
	rootKeyMu    sync.Mutex
	cfg          config.Config
	state        model.State
	committed    model.State
	loaded       bool
	syncStateDir func(string) error
}

func New(cfg config.Config) *Store {
	return &Store{cfg: cfg, syncStateDir: syncDir}
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) SyncManagedUserKeys(rootKey string, snapshot protocol.ManagedUserKeySnapshot, now time.Time) (protocol.ManagedUserKeySyncResult, error) {
	if ok, err := s.ValidateRootKey(rootKey); err != nil {
		return protocol.ManagedUserKeySyncResult{}, err
	} else if !ok {
		return protocol.ManagedUserKeySyncResult{}, ErrInvalidRootKey
	}
	snapshot.SnapshotID = strings.TrimSpace(snapshot.SnapshotID)
	if snapshot.SnapshotID == "" {
		return protocol.ManagedUserKeySyncResult{}, errors.New("snapshot_id is required")
	}
	if len(snapshot.Keys) > maxTotalTokens {
		return protocol.ManagedUserKeySyncResult{}, fmt.Errorf("managed key limit exceeded: maximum %d", maxTotalTokens)
	}
	managed := make([]model.Token, 0, len(snapshot.Keys))
	byOwner := make(map[string]model.Token, len(snapshot.Keys))
	ids := make(map[string]bool, len(snapshot.Keys))
	hashes := make(map[string]bool, len(snapshot.Keys))
	for _, key := range snapshot.Keys {
		key.ID = strings.TrimSpace(key.ID)
		key.Owner = normalizeHolder(key.Owner)
		key.Hash = strings.ToLower(strings.TrimSpace(key.Hash))
		if key.ID == "" || key.Owner == "" || key.Version < 1 || len(key.Hash) != sha256.Size*2 {
			return protocol.ManagedUserKeySyncResult{}, errors.New("managed key id, owner, positive version, and SHA-256 hash are required")
		}
		if _, err := hex.DecodeString(key.Hash); err != nil {
			return protocol.ManagedUserKeySyncResult{}, errors.New("managed key hash must be hexadecimal SHA-256")
		}
		if ids[key.ID] || hashes[key.Hash] {
			return protocol.ManagedUserKeySyncResult{}, errors.New("managed key ids and hashes must be unique")
		}
		if _, exists := byOwner[key.Owner]; exists {
			return protocol.ManagedUserKeySyncResult{}, errors.New("each owner may have only one managed key")
		}
		ids[key.ID], hashes[key.Hash] = true, true
		token := model.Token{ID: key.ID, Hash: key.Hash, Name: key.Owner, Mode: model.TokenModeManaged, Version: key.Version, Managed: true, CreatedAt: now.UTC()}
		managed = append(managed, token)
		byOwner[key.Owner] = token
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return protocol.ManagedUserKeySyncResult{}, err
	}
	if s.state.ManagedKeys && s.state.KeySnapshotID == snapshot.SnapshotID {
		return protocol.ManagedUserKeySyncResult{SnapshotID: snapshot.SnapshotID, Applied: len(managed), Managed: true}, nil
	}
	oldTokenIDs := make(map[string]string, len(s.state.Tokens))
	for _, token := range s.state.Tokens {
		oldTokenIDs[token.Hash] = token.ID
	}
	for i := range s.state.Reservations {
		reservation := &s.state.Reservations[i]
		if reservation.GroupID == "" {
			reservation.GroupID = oldTokenIDs[reservation.TokenHash]
			if reservation.GroupID == "" {
				reservation.GroupID = "grp_" + randomHex(8)
			}
		}
		token, ok := byOwner[normalizeHolder(reservation.Holder)]
		if !ok {
			reservation.Revoked = true
			reservation.Active = false
			continue
		}
		reservation.TokenHash = token.Hash
	}
	// Managed key replacement is an immediate credential cutover. Existing
	// authorizations keep their old hash/version so the eviction pass can close
	// them instead of silently transferring attacker-created scopes.
	s.state.Tokens = managed
	s.state.ManagedKeys = true
	s.state.KeySnapshotID = snapshot.SnapshotID
	if err := s.saveLocked(); err != nil {
		return protocol.ManagedUserKeySyncResult{}, err
	}
	return protocol.ManagedUserKeySyncResult{SnapshotID: snapshot.SnapshotID, Applied: len(managed), Managed: true}, nil
}

func (s *Store) ManagedKeysEnabled() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return false, err
	}
	return s.state.ManagedKeys, nil
}

func (s *Store) ManagedTokenByID(id string, now time.Time) (model.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return model.Token{}, err
	}
	for _, token := range s.state.Tokens {
		if token.ID == strings.TrimSpace(id) && token.Managed && !token.Revoked && !tokenExpired(token, now) {
			return token, nil
		}
	}
	return model.Token{}, ErrTokenNotFound
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
	data, err := readStateFile(s.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		s.state = model.State{}
		s.committed = model.State{}
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
	if err := validateStateBounds(s.state); err != nil {
		return fmt.Errorf("invalid persisted state: %w", err)
	}
	s.committed = cloneState(s.state)
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

// EnforcementSnapshot omits secrets, display-only command arrays, and audit
// history that the enforcement hot path never reads.
func (s *Store) EnforcementSnapshot() (model.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return model.State{}, err
	}
	return cloneEnforcementState(s.state), nil
}

// EnforcementSnapshotForToken filters before copying so a non-root ps request
// cannot duplicate unrelated state.
func (s *Store) EnforcementSnapshotForToken(tokenHash string) (model.State, error) {
	if tokenHash == "" {
		return model.State{}, errors.New("token hash is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return model.State{}, err
	}
	var out model.State
	for _, token := range s.state.Tokens {
		if token.Hash == tokenHash {
			token.Secret = ""
			out.Tokens = append(out.Tokens, token)
		}
	}
	for _, reservation := range s.state.Reservations {
		if reservation.TokenHash == tokenHash {
			out.Reservations = append(out.Reservations, reservation)
		}
	}
	allowedAuthorizationIDs := make(map[string]struct{})
	for _, authorization := range s.state.Authorizations {
		if authorization.TokenHash != tokenHash {
			continue
		}
		authorization.Command = nil
		out.Authorizations = append(out.Authorizations, authorization)
		allowedAuthorizationIDs[authorization.ID] = struct{}{}
	}
	for _, claim := range s.state.SoftClaims {
		if claim.TokenHash != tokenHash {
			continue
		}
		if _, ok := allowedAuthorizationIDs[claim.AuthorizationID]; ok {
			out.SoftClaims = append(out.SoftClaims, claim)
		}
	}
	return out, nil
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
	if err := s.checkTokenCapacityLocked(token, now); err != nil {
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
	return s.RegisterScheduledReservations(rootKey, name, "", gpus, now, now.Add(ttl), now)
}

func (s *Store) RegisterScheduledReservations(rootKey, name, purpose string, gpus []int, startsAt, expiresAt, now time.Time) (string, model.Token, []model.Reservation, error) {
	return s.RegisterScheduledReservationsWithSession(rootKey, name, purpose, "", gpus, startsAt, expiresAt, now)
}

func (s *Store) RegisterScheduledReservationsWithSession(rootKey, name, purpose, externalSessionID string, gpus []int, startsAt, expiresAt, now time.Time) (string, model.Token, []model.Reservation, error) {
	startsAt = startsAt.UTC()
	expiresAt = expiresAt.UTC()
	now = now.UTC()
	if startsAt.IsZero() {
		startsAt = now
	}
	if !expiresAt.After(startsAt) {
		return "", model.Token{}, nil, fmt.Errorf("reservation end must be after start")
	}
	if !expiresAt.After(now) {
		return "", model.Token{}, nil, fmt.Errorf("reservation end must be in the future")
	}
	if expiresAt.Sub(startsAt) > MaxHardTTL {
		return "", model.Token{}, nil, fmt.Errorf("reservation duration exceeds max %s", MaxHardTTL)
	}
	if len(gpus) == 0 {
		return "", model.Token{}, nil, fmt.Errorf("at least one gpu is required")
	}
	if err := checkTotalCapacity("reservation", 0, len(gpus), maxTotalReservations); err != nil {
		return "", model.Token{}, nil, err
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
	tokenSecret, token := newToken(model.TokenModeReserved, name, expiresAt, now)
	reservations := make([]model.Reservation, 0, len(gpus))
	for _, gpu := range gpus {
		reservations = append(reservations, model.Reservation{
			ID:                NewReservationID(),
			ExternalSessionID: strings.TrimSpace(externalSessionID),
			GPU:               gpu,
			TokenHash:         token.Hash,
			Holder:            token.Name,
			Purpose:           strings.TrimSpace(purpose),
			CreatedAt:         now,
			StartsAt:          startsAt,
			ExpiresAt:         expiresAt,
			Active:            true,
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return "", model.Token{}, nil, err
	}
	if externalSessionID = strings.TrimSpace(externalSessionID); externalSessionID != "" {
		var existingReservations []model.Reservation
		for _, reservation := range s.state.Reservations {
			if reservation.ExternalSessionID == externalSessionID {
				existingReservations = append(existingReservations, reservation)
			}
		}
		if len(existingReservations) > 0 {
			tokenHash := existingReservations[0].TokenHash
			var existingToken model.Token
			for _, candidate := range s.state.Tokens {
				if candidate.Hash == tokenHash {
					existingToken = candidate
					break
				}
			}
			if existingToken.ID == "" || existingToken.Secret == "" {
				return "", model.Token{}, nil, errors.New("existing reservation session has no recoverable key")
			}
			if !sameReservationRequest(existingReservations, name, purpose, gpus, startsAt, expiresAt) {
				return "", model.Token{}, nil, errors.New("external session id was already used for a different reservation")
			}
			return existingToken.Secret, existingToken, existingReservations, nil
		}
	}
	if err := s.checkTokenCapacityLocked(token, now); err != nil {
		return "", model.Token{}, nil, err
	}
	if err := checkTotalCapacity("reservation", len(s.state.Reservations), len(reservations), maxTotalReservations); err != nil {
		return "", model.Token{}, nil, err
	}
	for _, requested := range reservations {
		for _, existing := range s.state.Reservations {
			if existing.GPU == requested.GPU && model.ReservationOverlaps(existing, requested.StartsAt, requested.ExpiresAt) {
				return "", model.Token{}, nil, fmt.Errorf("gpu %d reservation overlaps %s", requested.GPU, existing.ID)
			}
		}
	}
	s.state.Tokens = append(s.state.Tokens, token)
	s.state.Reservations = append(s.state.Reservations, reservations...)
	if err := s.saveLocked(); err != nil {
		return "", model.Token{}, nil, err
	}
	return tokenSecret, token, reservations, nil
}

func (s *Store) RegisterManagedReservations(rootKey, userKeyID, purpose, externalSessionID string, gpus []int, startsAt, expiresAt, now time.Time) (model.Token, string, []model.Reservation, error) {
	if ok, err := s.ValidateRootKey(rootKey); err != nil {
		return model.Token{}, "", nil, err
	} else if !ok {
		return model.Token{}, "", nil, ErrInvalidRootKey
	}
	startsAt, expiresAt, now = startsAt.UTC(), expiresAt.UTC(), now.UTC()
	if startsAt.IsZero() {
		startsAt = now
	}
	if !expiresAt.After(startsAt) || !expiresAt.After(now) {
		return model.Token{}, "", nil, errors.New("reservation end must be after start and in the future")
	}
	if expiresAt.Sub(startsAt) > MaxHardTTL {
		return model.Token{}, "", nil, fmt.Errorf("reservation duration exceeds max %s", MaxHardTTL)
	}
	if len(gpus) == 0 {
		return model.Token{}, "", nil, errors.New("at least one gpu is required")
	}
	seen := make(map[int]bool, len(gpus))
	for _, gpu := range gpus {
		if err := validateGPUIndex(gpu); err != nil {
			return model.Token{}, "", nil, err
		}
		if seen[gpu] {
			return model.Token{}, "", nil, fmt.Errorf("duplicate gpu %d", gpu)
		}
		seen[gpu] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return model.Token{}, "", nil, err
	}
	if !s.state.ManagedKeys {
		return model.Token{}, "", nil, errors.New("managed user keys are not configured")
	}
	var token model.Token
	for _, candidate := range s.state.Tokens {
		if candidate.ID == strings.TrimSpace(userKeyID) && candidate.Managed && !candidate.Revoked {
			token = candidate
			break
		}
	}
	if token.ID == "" {
		return model.Token{}, "", nil, ErrTokenNotFound
	}
	externalSessionID = strings.TrimSpace(externalSessionID)
	if externalSessionID != "" {
		var existing []model.Reservation
		for _, reservation := range s.state.Reservations {
			if reservation.ExternalSessionID == externalSessionID {
				existing = append(existing, reservation)
			}
		}
		if len(existing) > 0 {
			if existing[0].TokenHash != token.Hash || !sameReservationRequest(existing, token.Name, purpose, gpus, startsAt, expiresAt) {
				return model.Token{}, "", nil, errors.New("external session id was already used for a different reservation")
			}
			return token, existing[0].GroupID, existing, nil
		}
	}
	if err := checkTotalCapacity("reservation", len(s.state.Reservations), len(gpus), maxTotalReservations); err != nil {
		return model.Token{}, "", nil, err
	}
	for _, gpu := range gpus {
		for _, existing := range s.state.Reservations {
			if existing.GPU == gpu && model.ReservationOverlaps(existing, startsAt, expiresAt) {
				return model.Token{}, "", nil, fmt.Errorf("gpu %d reservation overlaps %s", gpu, existing.ID)
			}
		}
	}
	groupID := "grp_" + randomHex(8)
	reservations := make([]model.Reservation, 0, len(gpus))
	for _, gpu := range gpus {
		reservations = append(reservations, model.Reservation{
			ID: NewReservationID(), GroupID: groupID, ExternalSessionID: externalSessionID,
			GPU: gpu, TokenHash: token.Hash, Holder: token.Name, Purpose: strings.TrimSpace(purpose),
			CreatedAt: now, StartsAt: startsAt, ExpiresAt: expiresAt, Active: true,
		})
	}
	s.state.Reservations = append(s.state.Reservations, reservations...)
	if err := s.saveLocked(); err != nil {
		return model.Token{}, "", nil, err
	}
	return token, groupID, reservations, nil
}

func sameReservationRequest(existing []model.Reservation, name, purpose string, gpus []int, startsAt, expiresAt time.Time) bool {
	if len(existing) != len(gpus) || len(existing) == 0 {
		return false
	}
	want := make(map[int]bool, len(gpus))
	for _, gpu := range gpus {
		want[gpu] = true
	}
	for _, reservation := range existing {
		if !want[reservation.GPU] || !strings.EqualFold(strings.TrimSpace(reservation.Holder), strings.TrimSpace(name)) ||
			strings.TrimSpace(reservation.Purpose) != strings.TrimSpace(purpose) ||
			!model.ReservationStartsAt(reservation).Equal(startsAt) || !reservation.ExpiresAt.Equal(expiresAt) {
			return false
		}
	}
	return true
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
	if err := s.checkTokenCapacityLocked(token, now); err != nil {
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
		Version:   token.Version,
		Managed:   token.Managed,
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
	if err := checkTotalCapacity("reservation", len(s.state.Reservations), 1, maxTotalReservations); err != nil {
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
	if err := checkTotalCapacity("authorization", len(s.state.Authorizations), 1, maxTotalAuthorizations); err != nil {
		return err
	}
	if err := s.checkActiveAuthorizationCapacityLocked(authorization, time.Now().UTC(), -1); err != nil {
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
			if err := s.checkActiveAuthorizationCapacityLocked(update, time.Now().UTC(), i); err != nil {
				return err
			}
			s.state.Authorizations[i] = update
			return s.saveLocked()
		}
	}
	return fmt.Errorf("authorization %s not found", update.ID)
}

// ActivateAuthorization records the PID of a command that was prepared while
// the authorization was pending. It deliberately updates the current stored
// row instead of accepting a caller-owned copy, so a concurrent revocation can
// never be overwritten.
func (s *Store) ActivateAuthorization(id string, rootPID int) (model.Authorization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rootPID <= 0 {
		return model.Authorization{}, errors.New("authorization root pid must be positive")
	}
	if err := s.loadLocked(); err != nil {
		return model.Authorization{}, err
	}
	for i := range s.state.Authorizations {
		authorization := &s.state.Authorizations[i]
		if authorization.ID != id {
			continue
		}
		if !authorization.Active || authorization.Revoked {
			return model.Authorization{}, fmt.Errorf("authorization %s was revoked before activation", id)
		}
		if authorization.RootPID != 0 {
			return model.Authorization{}, fmt.Errorf("authorization %s is already active", id)
		}
		authorization.RootPID = rootPID
		if err := s.saveLocked(); err != nil {
			return model.Authorization{}, err
		}
		return *authorization, nil
	}
	return model.Authorization{}, fmt.Errorf("authorization %s not found", id)
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
	if err := checkTotalCapacity("bypass", len(s.state.Bypasses), 1, maxTotalBypasses); err != nil {
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
	managedHashes := make(map[string]bool)
	for _, token := range s.state.Tokens {
		if token.Managed {
			managedHashes[token.Hash] = true
		}
	}
	managedGroupID := ""
	for _, reservation := range s.state.Reservations {
		groupID := reservation.GroupID
		if groupID == "" {
			groupID = reservation.ID
		}
		if managedHashes[reservation.TokenHash] && (reservation.ID == idOrToken || groupID == idOrToken) {
			managedGroupID = groupID
			break
		}
	}
	if managedGroupID != "" {
		changed := false
		for i := range s.state.Reservations {
			reservation := &s.state.Reservations[i]
			groupID := reservation.GroupID
			if groupID == "" {
				groupID = reservation.ID
			}
			if groupID == managedGroupID && !reservation.Revoked {
				reservation.Revoked = true
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return s.saveLocked()
	}
	tokenHash := ""
	if strings.HasPrefix(idOrToken, "rg_") {
		tokenHash = HashToken(idOrToken)
	}
	for _, token := range s.state.Tokens {
		if token.ID == idOrToken || (tokenHash != "" && token.Hash == tokenHash) {
			tokenHash = token.Hash
			break
		}
	}
	if tokenHash == "" {
		for _, reservation := range s.state.Reservations {
			if reservation.ID == idOrToken {
				tokenHash = reservation.TokenHash
				break
			}
		}
	}
	changed := false
	for i := range s.state.Tokens {
		token := &s.state.Tokens[i]
		if token.ID == idOrToken || (tokenHash != "" && token.Hash == tokenHash) {
			tokenHash = token.Hash
			token.Revoked = true
			changed = true
		}
	}

	for i := range s.state.Reservations {
		reservation := &s.state.Reservations[i]
		if reservation.ID == idOrToken || (tokenHash != "" && reservation.TokenHash == tokenHash) {
			reservation.Revoked = true
			changed = true
		}
	}

	for i := range s.state.Authorizations {
		authorization := &s.state.Authorizations[i]
		if authorization.ID == idOrToken || (tokenHash != "" && authorization.TokenHash == tokenHash) {
			authorization.Revoked = true
			changed = true
		}
	}

	for i := range s.state.Leases {
		lease := &s.state.Leases[i]
		if lease.ID == idOrToken || (tokenHash != "" && lease.TokenHash == tokenHash) {
			lease.ExpiresAt = time.Now().UTC()
			changed = true
		}
	}

	for i := range s.state.Bypasses {
		bypass := &s.state.Bypasses[i]
		if bypass.ID == idOrToken {
			bypass.Revoked = true
			changed = true
		}
	}

	if tokenHash != "" || changed {
		filtered := s.state.SoftClaims[:0]
		for _, claim := range s.state.SoftClaims {
			if claim.ID == idOrToken {
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
	return s.AppendAudits([]model.AuditEvent{event})
}

func (s *Store) AppendAudits(events []model.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	s.state.Audit = append(s.state.Audit, events...)
	if len(s.state.Audit) > maxAuditEvents {
		s.state.Audit = s.state.Audit[len(s.state.Audit)-maxAuditEvents:]
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	return appendAuditLogs(s.cfg.AuditLog, events)
}

func (s *Store) Status(now time.Time) (model.Status, error) {
	return s.status(now, "", true)
}

func (s *Store) StatusForToken(tokenHash string, now time.Time) (model.Status, error) {
	if tokenHash == "" {
		return model.Status{}, errors.New("token hash is required")
	}
	return s.status(now, tokenHash, false)
}

func (s *Store) status(now time.Time, allowedTokenHash string, all bool) (model.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return model.Status{}, err
	}
	status := model.Status{Now: now.UTC()}
	activeTokenHashes := map[string]bool{}
	tokenIDsByHash := map[string]string{}
	reservedTokenHashes := liveReservationTokenHashes(s.state.Reservations, now)
	for _, token := range s.state.Tokens {
		if !all && token.Hash != allowedTokenHash {
			continue
		}
		if token.Revoked || tokenExpired(token, now) {
			continue
		}
		if NormalizeTokenMode(token.Mode) == model.TokenModeReserved && !reservedTokenHashes[token.Hash] {
			continue
		}
		activeTokenHashes[token.Hash] = true
		tokenIDsByHash[token.Hash] = token.ID
		status.Tokens = append(status.Tokens, model.TokenView{
			ID:        token.ID,
			Name:      token.Name,
			Mode:      NormalizeTokenMode(token.Mode),
			Version:   token.Version,
			Managed:   token.Managed,
			CreatedAt: token.CreatedAt,
			ExpiresAt: timePtrIfSet(token.ExpiresAt),
			Revoked:   token.Revoked,
		})
	}
	for _, reservation := range s.state.Reservations {
		if !all && reservation.TokenHash != allowedTokenHash {
			continue
		}
		if reservation.Active && !reservation.Revoked && now.Before(reservation.ExpiresAt) {
			status.Reservations = append(status.Reservations, model.ReservationView{
				ID:                reservation.ID,
				GroupID:           reservationGroupID(reservation, tokenIDsByHash),
				ExternalSessionID: reservation.ExternalSessionID,
				GPU:               reservation.GPU,
				Holder:            reservation.Holder,
				Purpose:           reservation.Purpose,
				CreatedAt:         reservation.CreatedAt,
				StartsAt:          model.ReservationStartsAt(reservation),
				ExpiresAt:         reservation.ExpiresAt,
				Active:            reservation.Active,
				Revoked:           reservation.Revoked,
			})
		}
	}
	activeAuthorizationIDs := map[string]bool{}
	for _, authorization := range s.state.Authorizations {
		if !all && authorization.TokenHash != allowedTokenHash {
			continue
		}
		if authorization.Active && !authorization.Revoked && !authorizationExpired(authorization, now) {
			activeAuthorizationIDs[authorization.ID] = true
			status.Authorizations = append(status.Authorizations, authorizationView(authorization, tokenIDsByHash[authorization.TokenHash]))
		}
	}
	for _, claim := range s.state.SoftClaims {
		if !all && claim.TokenHash != allowedTokenHash {
			continue
		}
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
	if all {
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
	status := model.KeyStatus{Now: now.UTC()}
	tokenIDsByHash := map[string]string{}
	reservedTokenHashes := liveReservationTokenHashes(s.state.Reservations, now)
	for _, token := range s.state.Tokens {
		if token.Revoked || tokenExpired(token, now) {
			continue
		}
		if NormalizeTokenMode(token.Mode) == model.TokenModeReserved && !reservedTokenHashes[token.Hash] {
			continue
		}
		tokenIDsByHash[token.Hash] = token.ID
		view := model.TokenView{
			ID:        token.ID,
			Name:      token.Name,
			Mode:      NormalizeTokenMode(token.Mode),
			Version:   token.Version,
			Managed:   token.Managed,
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
				ID:                reservation.ID,
				GroupID:           reservationGroupID(reservation, tokenIDsByHash),
				ExternalSessionID: reservation.ExternalSessionID,
				GPU:               reservation.GPU,
				Holder:            reservation.Holder,
				Purpose:           reservation.Purpose,
				CreatedAt:         reservation.CreatedAt,
				StartsAt:          model.ReservationStartsAt(reservation),
				ExpiresAt:         reservation.ExpiresAt,
				Active:            reservation.Active,
				Revoked:           reservation.Revoked,
			})
		}
	}
	for _, authorization := range s.state.Authorizations {
		if authorization.Active && !authorization.Revoked && !authorizationExpired(authorization, now) {
			status.Authorizations = append(status.Authorizations, authorizationView(authorization, tokenIDsByHash[authorization.TokenHash]))
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

// Prune removes inactive, revoked, and expired records after the daemon has had
// an opportunity to enforce their former scopes. Status reads intentionally do
// not prune because doing so could erase the evidence needed for eviction.
func (s *Store) Prune(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	if !s.pruneExpiredLocked(now) {
		return nil
	}
	return s.saveLocked()
}

// PruneInactiveManagedScopes removes only explicitly named, inactive records
// that refer to managed cgroups. The daemon uses this narrow operation after
// independently confirming those cgroups are empty or gone; non-cgroup rows
// retain the stale scope needed for GPU-process eviction.
func (s *Store) PruneInactiveManagedScopes(authorizationIDs, leaseIDs []string) error {
	authorizations := make(map[string]struct{}, len(authorizationIDs))
	for _, id := range authorizationIDs {
		authorizations[id] = struct{}{}
	}
	leases := make(map[string]struct{}, len(leaseIDs))
	for _, id := range leaseIDs {
		leases[id] = struct{}{}
	}
	if len(authorizations) == 0 && len(leases) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	changed := false
	keptAuthorizations := make([]model.Authorization, 0, len(s.state.Authorizations))
	for _, authorization := range s.state.Authorizations {
		_, selected := authorizations[authorization.ID]
		if selected && !authorization.Active && authorization.CgroupPath != "" {
			changed = true
			continue
		}
		keptAuthorizations = append(keptAuthorizations, authorization)
	}
	keptLeases := make([]model.Lease, 0, len(s.state.Leases))
	for _, lease := range s.state.Leases {
		_, selected := leases[lease.ID]
		if selected && !lease.Active && lease.CgroupPath != "" {
			changed = true
			continue
		}
		keptLeases = append(keptLeases, lease)
	}
	if !changed {
		return nil
	}
	s.state.Authorizations = keptAuthorizations
	s.state.Leases = keptLeases
	return s.saveLocked()
}

// PruneUnreferencedInvalidEntitlements removes expired/revoked token and
// reservation rows only when no non-cgroup authorization or lease still needs
// their scope for process eviction. It is safe to call while GPU telemetry is
// unavailable.
func (s *Store) PruneUnreferencedInvalidEntitlements(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	now = now.UTC()
	referenced := make(map[string]struct{})
	for _, authorization := range s.state.Authorizations {
		if authorization.CgroupPath == "" && authorization.TokenHash != "" {
			referenced[authorization.TokenHash] = struct{}{}
		}
	}
	for _, lease := range s.state.Leases {
		if lease.CgroupPath == "" && lease.TokenHash != "" {
			referenced[lease.TokenHash] = struct{}{}
		}
	}
	tokenValid := make(map[string]bool, len(s.state.Tokens))
	for _, token := range s.state.Tokens {
		tokenValid[token.Hash] = !token.Revoked && (token.ExpiresAt.IsZero() || now.Before(token.ExpiresAt))
	}
	changed := false
	reservations := make([]model.Reservation, 0, len(s.state.Reservations))
	for _, reservation := range s.state.Reservations {
		_, needed := referenced[reservation.TokenHash]
		invalid := !reservation.Active || reservation.Revoked || !now.Before(reservation.ExpiresAt) || !tokenValid[reservation.TokenHash]
		if invalid && !needed {
			changed = true
			continue
		}
		reservations = append(reservations, reservation)
	}
	tokens := make([]model.Token, 0, len(s.state.Tokens))
	for _, token := range s.state.Tokens {
		_, needed := referenced[token.Hash]
		if !tokenValid[token.Hash] && !needed {
			changed = true
			continue
		}
		tokens = append(tokens, token)
	}
	if !changed {
		return nil
	}
	s.state.Reservations = reservations
	s.state.Tokens = tokens
	return s.saveLocked()
}

func (s *Store) ReadOrCreateRootKey() (string, error) {
	s.rootKeyMu.Lock()
	defer s.rootKeyMu.Unlock()

	key, err := readRootKeyFile(s.cfg.RootKeyPath)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	key = "rk_" + randomHex(rootKeyHexBytes)
	if err := os.MkdirAll(filepath.Dir(s.cfg.RootKeyPath), 0700); err != nil {
		return "", err
	}
	if err := createRootKeyFile(s.cfg.RootKeyPath, []byte(key+"\n")); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readRootKeyFile(s.cfg.RootKeyPath)
		}
		return "", err
	}
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

func (s *Store) saveLocked() (err error) {
	defer func() {
		if err != nil {
			s.state = cloneState(s.committed)
		}
	}()

	if err := validateStateBounds(s.state); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.StatePath), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxStateFileBytes {
		return fmt.Errorf("state exceeds %d-byte limit", maxStateFileBytes)
	}
	syncStateDir := s.syncStateDir
	if syncStateDir == nil {
		syncStateDir = syncDir
	}
	committedToDisk, err := writeFileAtomically(s.cfg.StatePath, append(data, '\n'), 0600, syncStateDir)
	if committedToDisk {
		// Rename is the commit point. Even when the following directory fsync
		// fails, keeping the previous in-memory state would diverge from the
		// file readers and could resurrect a revoked entitlement later.
		s.committed = cloneState(s.state)
	}
	if err != nil {
		return err
	}
	return nil
}

func appendAuditLog(path string, event model.AuditEvent) error {
	return appendAuditLogs(path, []model.AuditEvent{event})
}

func appendAuditLogs(path string, events []model.AuditEvent) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var lines []byte
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		lines = append(lines, data...)
		lines = append(lines, '\n')
	}
	if int64(len(lines)) > maxAuditLogBytes {
		return fmt.Errorf("audit batch exceeds %d-byte log limit", maxAuditLogBytes)
	}
	if err := rotateAuditLogIfNeeded(path, int64(len(lines))); err != nil {
		return err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_APPEND|syscall.O_WRONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("open audit log %s: invalid file descriptor", path)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return fmt.Errorf("audit log %s is not a regular file", path)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	if err := writeAll(file, lines); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDir(dir)
}

func readStateFile(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("open state %s: invalid file descriptor", path)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("state %s is not a regular file", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("state %s permissions %04o allow group or other access", path, info.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxStateFileBytes {
		return nil, fmt.Errorf("state %s exceeds %d-byte limit", path, maxStateFileBytes)
	}
	return data, nil
}

func readRootKeyFile(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return "", fmt.Errorf("open root key %s: invalid file descriptor", path)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("root key %s is not a regular file", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("root key %s permissions %04o allow group or other access", path, info.Mode().Perm())
	}

	const maxRootKeyFileBytes = 3 + rootKeyHexBytes*2 + 1
	data, err := io.ReadAll(io.LimitReader(file, maxRootKeyFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxRootKeyFileBytes {
		return "", fmt.Errorf("root key %s has invalid length", path)
	}
	raw := string(data)
	key := strings.TrimSuffix(raw, "\n")
	if len(key) != 3+rootKeyHexBytes*2 || !strings.HasPrefix(key, "rk_") {
		return "", fmt.Errorf("root key %s has invalid format", path)
	}
	decoded, err := hex.DecodeString(key[3:])
	if err != nil || len(decoded) != rootKeyHexBytes {
		return "", fmt.Errorf("root key %s has invalid format", path)
	}
	return key, nil
}

func createRootKeyFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmpPath, err := writeSyncedTempFile(dir, "."+filepath.Base(path)+".tmp-*", data, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	if err = os.Link(tmpPath, path); err != nil {
		return err
	}
	if err = syncDir(dir); err != nil {
		return err
	}
	if err = os.Remove(tmpPath); err != nil {
		return err
	}
	return syncDir(dir)
}

func writeFileAtomically(path string, data []byte, mode os.FileMode, syncDirectory func(string) error) (committed bool, err error) {
	dir := filepath.Dir(path)
	tmpPath, err := writeSyncedTempFile(dir, "."+filepath.Base(path)+".tmp-*", data, mode)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmpPath)
	if err = os.Rename(tmpPath, path); err != nil {
		return false, err
	}
	return true, syncDirectory(dir)
}

func writeSyncedTempFile(dir, pattern string, data []byte, mode os.FileMode) (path string, err error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	tmpPath := file.Name()
	path = tmpPath
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err = file.Chmod(mode); err != nil {
		return "", err
	}
	if err = writeAll(file, data); err != nil {
		return "", err
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	return path, nil
}

func rotateAuditLogIfNeeded(path string, incomingBytes int64) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("audit log %s is not a regular file", path)
	}
	if info.Size() <= int64(maxAuditLogBytes)-incomingBytes {
		return nil
	}

	backup := path + ".1"
	backupInfo, err := os.Lstat(backup)
	if err == nil && !backupInfo.Mode().IsRegular() {
		return fmt.Errorf("audit log backup %s is not a regular file", backup)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		err = os.Remove(backup)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, backup); err != nil {
		return err
	}
	if err := os.Chmod(backup, 0600); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func (s *Store) pruneExpiredLocked(now time.Time) bool {
	changed := false
	expiredTokenHashes := map[string]bool{}
	reservedTokenHashes := liveReservationTokenHashes(s.state.Reservations, now)

	tokens := s.state.Tokens[:0]
	for _, token := range s.state.Tokens {
		orphanReserved := NormalizeTokenMode(token.Mode) == model.TokenModeReserved && !reservedTokenHashes[token.Hash]
		if token.Revoked || tokenExpired(token, now) || orphanReserved {
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

func liveReservationTokenHashes(reservations []model.Reservation, now time.Time) map[string]bool {
	hashes := make(map[string]bool)
	for _, reservation := range reservations {
		if reservation.Active && !reservation.Revoked && now.Before(reservation.ExpiresAt) && reservation.TokenHash != "" {
			hashes[reservation.TokenHash] = true
		}
	}
	return hashes
}

func cloneState(state model.State) model.State {
	out := model.State{ManagedKeys: state.ManagedKeys, KeySnapshotID: state.KeySnapshotID}
	out.Tokens = append(out.Tokens, state.Tokens...)
	out.Reservations = append(out.Reservations, state.Reservations...)
	out.Authorizations = append(out.Authorizations, state.Authorizations...)
	for i := range out.Authorizations {
		out.Authorizations[i].Command = append([]string(nil), out.Authorizations[i].Command...)
	}
	out.SoftClaims = append(out.SoftClaims, state.SoftClaims...)
	out.Leases = append(out.Leases, state.Leases...)
	for i := range out.Leases {
		out.Leases[i].Command = append([]string(nil), out.Leases[i].Command...)
	}
	out.Bypasses = append(out.Bypasses, state.Bypasses...)
	out.Audit = append(out.Audit, state.Audit...)
	return out
}

func cloneEnforcementState(state model.State) model.State {
	out := model.State{ManagedKeys: state.ManagedKeys, KeySnapshotID: state.KeySnapshotID}
	out.Tokens = append(out.Tokens, state.Tokens...)
	for i := range out.Tokens {
		out.Tokens[i].Secret = ""
	}
	out.Reservations = append(out.Reservations, state.Reservations...)
	out.Authorizations = append(out.Authorizations, state.Authorizations...)
	for i := range out.Authorizations {
		out.Authorizations[i].Command = nil
	}
	out.SoftClaims = append(out.SoftClaims, state.SoftClaims...)
	out.Leases = append(out.Leases, state.Leases...)
	for i := range out.Leases {
		out.Leases[i].Command = nil
	}
	out.Bypasses = append(out.Bypasses, state.Bypasses...)
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

func authorizationView(authorization model.Authorization, tokenID string) model.AuthorizationView {
	return model.AuthorizationView{
		ID:               authorization.ID,
		TokenID:          tokenID,
		Mode:             authorization.Mode,
		TokenMode:        NormalizeTokenMode(authorization.TokenMode),
		Holder:           authorization.Holder,
		UID:              authorization.UID,
		GID:              authorization.GID,
		Username:         authorization.Username,
		Command:          boundedViewCommand(authorization.Command),
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

func reservationGroupID(reservation model.Reservation, tokenIDsByHash map[string]string) string {
	if reservation.GroupID != "" {
		return reservation.GroupID
	}
	return tokenIDsByHash[reservation.TokenHash]
}

func boundedViewCommand(command []string) []string {
	out := make([]string, 0, min(len(command), maxViewCommandArgs))
	remaining := maxViewCommandBytes
	for _, argument := range command {
		if len(out) >= maxViewCommandArgs || remaining <= 0 {
			break
		}
		if len(argument) > remaining {
			argument = argument[:remaining]
		}
		out = append(out, argument)
		remaining -= len(argument)
	}
	return out
}

func checkTotalCapacity(kind string, current, adding, maximum int) error {
	if adding > maximum || current > maximum-adding {
		return fmt.Errorf("%s limit exceeded: adding %d to %d would exceed maximum %d", kind, adding, current, maximum)
	}
	return nil
}

func validateStateBounds(state model.State) error {
	if err := validatePersistedValues("state", state.KeySnapshotID); err != nil {
		return err
	}
	counts := []struct {
		kind    string
		current int
		maximum int
	}{
		{"token", len(state.Tokens), maxTotalTokens},
		{"reservation", len(state.Reservations), maxTotalReservations},
		{"authorization", len(state.Authorizations), maxTotalAuthorizations},
		{"soft claim", len(state.SoftClaims), maxTotalSoftClaims},
		{"lease", len(state.Leases), maxTotalLeases},
		{"bypass", len(state.Bypasses), maxTotalBypasses},
		{"audit event", len(state.Audit), maxAuditEvents},
	}
	for _, count := range counts {
		if count.current > count.maximum {
			return fmt.Errorf("%s limit exceeded: %d is greater than maximum %d", count.kind, count.current, count.maximum)
		}
	}
	for _, token := range state.Tokens {
		if err := validatePersistedValues("token", token.ID, token.Hash, token.Secret, token.Name, token.Mode); err != nil {
			return err
		}
	}
	for _, reservation := range state.Reservations {
		if err := validateGPUIndex(reservation.GPU); err != nil {
			return fmt.Errorf("reservation %q: %w", reservation.ID, err)
		}
		if err := validatePersistedValues("reservation", reservation.ID, reservation.GroupID, reservation.ExternalSessionID, reservation.TokenHash, reservation.Holder, reservation.Purpose); err != nil {
			return err
		}
	}
	for _, authorization := range state.Authorizations {
		if err := validatePersistedValues(
			"authorization",
			authorization.ID,
			authorization.Mode,
			authorization.TokenHash,
			authorization.TokenMode,
			authorization.Holder,
			authorization.Username,
			authorization.CgroupPath,
			authorization.CgroupRel,
			authorization.ContainerID,
			authorization.ContainerPattern,
			authorization.Namespace,
		); err != nil {
			return err
		}
		if err := validatePersistedCommand(authorization.Command); err != nil {
			return fmt.Errorf("authorization %q: %w", authorization.ID, err)
		}
	}
	for _, claim := range state.SoftClaims {
		if err := validateGPUIndex(claim.GPU); err != nil {
			return fmt.Errorf("soft claim %q: %w", claim.ID, err)
		}
		if err := validatePersistedValues("soft claim", claim.ID, claim.TokenHash, claim.AuthorizationID, claim.Holder); err != nil {
			return err
		}
	}
	for _, lease := range state.Leases {
		if err := validateGPUIndex(lease.GPU); err != nil {
			return fmt.Errorf("lease %q: %w", lease.ID, err)
		}
		if err := validatePersistedValues(
			"lease",
			lease.ID,
			lease.Mode,
			lease.TokenHash,
			lease.Holder,
			lease.CgroupPath,
			lease.CgroupRel,
			lease.ContainerID,
			lease.Namespace,
		); err != nil {
			return err
		}
		if err := validatePersistedCommand(lease.Command); err != nil {
			return fmt.Errorf("lease %q: %w", lease.ID, err)
		}
	}
	for _, bypass := range state.Bypasses {
		if err := validatePersistedValues("bypass", bypass.ID, bypass.Type, bypass.BootID, bypass.Command, bypass.Reason); err != nil {
			return err
		}
	}
	for _, event := range state.Audit {
		if err := validatePersistedValues("audit event", event.Kind, event.LeaseID, event.User); err != nil {
			return err
		}
		if len(event.Message) > maxAuditMessageBytes {
			return fmt.Errorf("audit event message exceeds %d bytes", maxAuditMessageBytes)
		}
	}
	return nil
}

func validatePersistedValues(kind string, values ...string) error {
	for _, value := range values {
		if len(value) > maxPersistedValueBytes {
			return fmt.Errorf("%s value exceeds %d bytes", kind, maxPersistedValueBytes)
		}
	}
	return nil
}

func validatePersistedCommand(command []string) error {
	if len(command) > maxPersistedCommandArgs {
		return fmt.Errorf("command has %d arguments; maximum is %d", len(command), maxPersistedCommandArgs)
	}
	total := 0
	for _, argument := range command {
		if len(argument) > maxPersistedCommandBytes-total {
			return fmt.Errorf("command exceeds %d bytes", maxPersistedCommandBytes)
		}
		total += len(argument)
	}
	return nil
}

func validateGPUIndex(gpu int) error {
	if gpu < 0 || gpu > maxGPUIndex {
		return fmt.Errorf("gpu index %d is outside 0..%d", gpu, maxGPUIndex)
	}
	return nil
}

func (s *Store) checkTokenCapacityLocked(candidate model.Token, now time.Time) error {
	if err := checkTotalCapacity("token", len(s.state.Tokens), 1, maxTotalTokens); err != nil {
		return err
	}
	if !tokenActiveAt(candidate, now) {
		return nil
	}
	holder := normalizeHolder(candidate.Name)
	active := 0
	for _, token := range s.state.Tokens {
		if tokenActiveAt(token, now) && normalizeHolder(token.Name) == holder {
			active++
		}
	}
	if active >= maxActiveTokensPerHolder {
		return fmt.Errorf("active token limit reached for holder %q: maximum %d", holder, maxActiveTokensPerHolder)
	}
	return nil
}

func (s *Store) checkActiveAuthorizationCapacityLocked(candidate model.Authorization, now time.Time, excludeIndex int) error {
	if !authorizationActiveAt(candidate, now) {
		return nil
	}
	active := 0
	for i, authorization := range s.state.Authorizations {
		if i != excludeIndex && authorization.TokenHash == candidate.TokenHash && authorizationActiveAt(authorization, now) {
			active++
		}
	}
	if active >= maxActiveAuthorizationsPerToken {
		return fmt.Errorf("active authorization limit reached for token: maximum %d", maxActiveAuthorizationsPerToken)
	}
	return nil
}

func normalizeHolder(holder string) string {
	holder = strings.ToLower(strings.TrimSpace(holder))
	if holder == "" {
		return "anonymous"
	}
	return holder
}

func authorizationActiveAt(authorization model.Authorization, now time.Time) bool {
	return authorization.Active && !authorization.Revoked && !authorizationExpired(authorization, now)
}

func authorizationExpired(authorization model.Authorization, now time.Time) bool {
	return timeIsSet(authorization.ExpiresAt) && !now.Before(authorization.ExpiresAt)
}

func tokenActiveAt(token model.Token, now time.Time) bool {
	return !token.Revoked && !tokenExpired(token, now)
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
