package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	accessTokenPrefix                = "ga_"
	accessTokenSecretBytes           = 32
	accessTokenIDBytes               = 9
	accessTokenMaxNameBytes          = 80
	accessTokenMaxPerUser            = 32
	accessTokenMaxStored             = 128
	accessTokenMaxDays               = 365
	accessTokenLastUsedWriteInterval = 5 * time.Minute
	accessTokenInactiveRetention     = 90 * 24 * time.Hour
)

const (
	accessTokenScopeNodesRead           = "nodes:read"
	accessTokenScopeReservationsWrite   = "reservations:write"
	accessTokenScopeAuthorizationsWrite = "authorizations:write"
	accessTokenScopeResourcesRevoke     = "resources:revoke"
	accessTokenScopeKeysRead            = "keys:read"
	accessTokenScopeKeysReveal          = "keys:reveal"
	accessTokenScopeKeysRotate          = "keys:rotate"
	accessTokenScopeHistoryRead         = "history:read"
	accessTokenScopeHistoryWrite        = "history:write"
)

var accessTokenSupportedScopes = map[string]struct{}{
	accessTokenScopeNodesRead:           {},
	accessTokenScopeReservationsWrite:   {},
	accessTokenScopeAuthorizationsWrite: {},
	accessTokenScopeResourcesRevoke:     {},
	accessTokenScopeKeysRead:            {},
	accessTokenScopeKeysReveal:          {},
	accessTokenScopeKeysRotate:          {},
	accessTokenScopeHistoryRead:         {},
	accessTokenScopeHistoryWrite:        {},
}

var accessTokenDefaultScopes = []string{
	accessTokenScopeNodesRead,
	accessTokenScopeReservationsWrite,
	accessTokenScopeAuthorizationsWrite,
	accessTokenScopeResourcesRevoke,
	accessTokenScopeKeysRead,
	accessTokenScopeHistoryRead,
}

type AccessTokenRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Hash       string     `json:"hash"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type PublicAccessToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type CreatedAccessToken struct {
	PublicAccessToken
	Secret string `json:"secret"`
}

type AuthenticatedAccessToken struct {
	User   string
	Role   string
	ID     string
	Scopes []string
}

func (s *UserStore) CreateAccessToken(username, name string, scopes []string, expiresAt time.Time) (CreatedAccessToken, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return CreatedAccessToken{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return CreatedAccessToken{}, errors.New("token name is required")
	}
	if len(name) > accessTokenMaxNameBytes {
		return CreatedAccessToken{}, fmt.Errorf("token name must be at most %d bytes", accessTokenMaxNameBytes)
	}
	scopes, err = normalizeAccessTokenScopes(scopes)
	if err != nil {
		return CreatedAccessToken{}, err
	}
	now := time.Now().UTC()
	expiresAt = expiresAt.UTC()
	if !expiresAt.After(now) {
		return CreatedAccessToken{}, errors.New("token expiry must be in the future")
	}
	if expiresAt.After(now.Add(accessTokenMaxDays * 24 * time.Hour)) {
		return CreatedAccessToken{}, fmt.Errorf("token expiry must be within %d days", accessTokenMaxDays)
	}
	secretBytes := make([]byte, accessTokenSecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return CreatedAccessToken{}, err
	}
	idBytes := make([]byte, accessTokenIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return CreatedAccessToken{}, err
	}
	secret := accessTokenPrefix + base64.RawURLEncoding.EncodeToString(secretBytes)
	sum := sha256.Sum256([]byte(secret))
	record := AccessTokenRecord{
		ID:        "at_" + base64.RawURLEncoding.EncodeToString(idBytes),
		Name:      name,
		Hash:      hex.EncodeToString(sum[:]),
		Scopes:    scopes,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return CreatedAccessToken{}, err
	}
	for i := range users {
		if users[i].Username != username || users[i].Disabled {
			continue
		}
		users[i].AccessTokens = compactAccessTokens(users[i].AccessTokens, now)
		active := 0
		for _, token := range users[i].AccessTokens {
			if token.RevokedAt == nil && token.ExpiresAt.After(now) {
				active++
			}
		}
		if active >= accessTokenMaxPerUser {
			return CreatedAccessToken{}, fmt.Errorf("maximum %d active access tokens reached", accessTokenMaxPerUser)
		}
		users[i].AccessTokens = append(users[i].AccessTokens, record)
		if err := s.saveLocked(users); err != nil {
			return CreatedAccessToken{}, err
		}
		return CreatedAccessToken{PublicAccessToken: publicAccessToken(record), Secret: secret}, nil
	}
	return CreatedAccessToken{}, errors.New("user not found")
}

func compactAccessTokens(tokens []AccessTokenRecord, now time.Time) []AccessTokenRecord {
	kept := make([]AccessTokenRecord, 0, len(tokens))
	for _, token := range tokens {
		inactiveAt := token.ExpiresAt
		if token.RevokedAt != nil {
			inactiveAt = *token.RevokedAt
		}
		if (token.RevokedAt != nil || !token.ExpiresAt.After(now)) && now.Sub(inactiveAt) > accessTokenInactiveRetention {
			continue
		}
		kept = append(kept, token)
	}
	if len(kept) < accessTokenMaxStored {
		return kept
	}
	drop := len(kept) - (accessTokenMaxStored - 1)
	out := make([]AccessTokenRecord, 0, len(kept)-drop)
	for _, token := range kept {
		inactive := token.RevokedAt != nil || !token.ExpiresAt.After(now)
		if drop > 0 && inactive {
			drop--
			continue
		}
		out = append(out, token)
	}
	return out
}

func (s *UserStore) ListAccessTokens(username string) ([]PublicAccessToken, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		if user.Username != username || user.Disabled {
			continue
		}
		out := make([]PublicAccessToken, 0, len(user.AccessTokens))
		for _, token := range user.AccessTokens {
			out = append(out, publicAccessToken(token))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
		return out, nil
	}
	return nil, errors.New("user not found")
}

func (s *UserStore) RevokeAccessToken(username, id string) error {
	username, err := normalizeUsername(username)
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("token id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i := range users {
		if users[i].Username != username || users[i].Disabled {
			continue
		}
		for j := range users[i].AccessTokens {
			token := &users[i].AccessTokens[j]
			if token.ID != id {
				continue
			}
			if token.RevokedAt == nil {
				now := time.Now().UTC()
				token.RevokedAt = &now
			}
			return s.saveLocked(users)
		}
		return errors.New("token not found")
	}
	return errors.New("user not found")
}

func (s *UserStore) AuthenticateAccessToken(secret string) (AuthenticatedAccessToken, bool) {
	secret = strings.TrimSpace(secret)
	if !strings.HasPrefix(secret, accessTokenPrefix) || len(secret) > 256 {
		return AuthenticatedAccessToken{}, false
	}
	sum := sha256.Sum256([]byte(secret))
	want := []byte(hex.EncodeToString(sum[:]))
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return AuthenticatedAccessToken{}, false
	}
	for i := range users {
		if users[i].Disabled {
			continue
		}
		for j := range users[i].AccessTokens {
			token := &users[i].AccessTokens[j]
			if len(token.Hash) != len(want) || subtle.ConstantTimeCompare([]byte(token.Hash), want) != 1 {
				continue
			}
			if token.RevokedAt != nil || !token.ExpiresAt.After(now) {
				return AuthenticatedAccessToken{}, false
			}
			if token.LastUsedAt == nil || now.Sub(*token.LastUsedAt) >= accessTokenLastUsedWriteInterval {
				token.LastUsedAt = &now
				_ = s.saveLocked(users)
			}
			return AuthenticatedAccessToken{
				User: users[i].Username, Role: users[i].Role, ID: token.ID, Scopes: append([]string(nil), token.Scopes...),
			}, true
		}
	}
	return AuthenticatedAccessToken{}, false
}

func normalizeAccessTokenScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		scopes = accessTokenDefaultScopes
	}
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if _, ok := accessTokenSupportedScopes[scope]; !ok {
			return nil, fmt.Errorf("unsupported access token scope %q", scope)
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	sort.Strings(out)
	return out, nil
}

func publicAccessToken(token AccessTokenRecord) PublicAccessToken {
	return PublicAccessToken{
		ID: token.ID, Name: token.Name, Scopes: append([]string(nil), token.Scopes...), CreatedAt: token.CreatedAt,
		ExpiresAt: token.ExpiresAt, LastUsedAt: token.LastUsedAt, RevokedAt: token.RevokedAt,
	}
}
