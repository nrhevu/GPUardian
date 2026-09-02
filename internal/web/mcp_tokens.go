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
	mcpAccessTokenPrefix       = "ga_"
	mcpAccessTokenSecretBytes  = 32
	mcpAccessTokenIDBytes      = 9
	mcpAccessTokenMaxNameBytes = 80
	mcpAccessTokenMaxPerUser   = 32
	mcpAccessTokenMaxStored    = 128
	mcpAccessTokenMaxDays      = 365
	mcpLastUsedWriteInterval   = 5 * time.Minute
	mcpInactiveTokenRetention  = 90 * 24 * time.Hour
)

const (
	mcpScopeNodesRead           = "nodes:read"
	mcpScopeReservationsWrite   = "reservations:write"
	mcpScopeAuthorizationsWrite = "authorizations:write"
	mcpScopeResourcesRevoke     = "resources:revoke"
	mcpScopeKeysRead            = "keys:read"
	mcpScopeKeysReveal          = "keys:reveal"
	mcpScopeKeysRotate          = "keys:rotate"
	mcpScopeHistoryRead         = "history:read"
	mcpScopeHistoryWrite        = "history:write"
)

var mcpSupportedScopes = map[string]struct{}{
	mcpScopeNodesRead:           {},
	mcpScopeReservationsWrite:   {},
	mcpScopeAuthorizationsWrite: {},
	mcpScopeResourcesRevoke:     {},
	mcpScopeKeysRead:            {},
	mcpScopeKeysReveal:          {},
	mcpScopeKeysRotate:          {},
	mcpScopeHistoryRead:         {},
	mcpScopeHistoryWrite:        {},
}

var mcpDefaultScopes = []string{
	mcpScopeNodesRead,
	mcpScopeReservationsWrite,
	mcpScopeAuthorizationsWrite,
	mcpScopeResourcesRevoke,
	mcpScopeKeysRead,
	mcpScopeHistoryRead,
}

type MCPAccessTokenRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Hash       string     `json:"hash"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type PublicMCPAccessToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type CreatedMCPAccessToken struct {
	PublicMCPAccessToken
	Secret string `json:"secret"`
}

type AuthenticatedMCPToken struct {
	User   string
	Role   string
	ID     string
	Scopes []string
}

func (s *UserStore) CreateMCPAccessToken(username, name string, scopes []string, expiresAt time.Time) (CreatedMCPAccessToken, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return CreatedMCPAccessToken{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return CreatedMCPAccessToken{}, errors.New("token name is required")
	}
	if len(name) > mcpAccessTokenMaxNameBytes {
		return CreatedMCPAccessToken{}, fmt.Errorf("token name must be at most %d bytes", mcpAccessTokenMaxNameBytes)
	}
	scopes, err = normalizeMCPScopes(scopes)
	if err != nil {
		return CreatedMCPAccessToken{}, err
	}
	now := time.Now().UTC()
	expiresAt = expiresAt.UTC()
	if !expiresAt.After(now) {
		return CreatedMCPAccessToken{}, errors.New("token expiry must be in the future")
	}
	if expiresAt.After(now.Add(mcpAccessTokenMaxDays * 24 * time.Hour)) {
		return CreatedMCPAccessToken{}, fmt.Errorf("token expiry must be within %d days", mcpAccessTokenMaxDays)
	}
	secretBytes := make([]byte, mcpAccessTokenSecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return CreatedMCPAccessToken{}, err
	}
	idBytes := make([]byte, mcpAccessTokenIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return CreatedMCPAccessToken{}, err
	}
	secret := mcpAccessTokenPrefix + base64.RawURLEncoding.EncodeToString(secretBytes)
	sum := sha256.Sum256([]byte(secret))
	record := MCPAccessTokenRecord{
		ID:        "mcp_" + base64.RawURLEncoding.EncodeToString(idBytes),
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
		return CreatedMCPAccessToken{}, err
	}
	for i := range users {
		if users[i].Username != username || users[i].Disabled {
			continue
		}
		users[i].MCPAccessTokens = compactMCPAccessTokens(users[i].MCPAccessTokens, now)
		active := 0
		for _, token := range users[i].MCPAccessTokens {
			if token.RevokedAt == nil && token.ExpiresAt.After(now) {
				active++
			}
		}
		if active >= mcpAccessTokenMaxPerUser {
			return CreatedMCPAccessToken{}, fmt.Errorf("maximum %d active MCP access tokens reached", mcpAccessTokenMaxPerUser)
		}
		users[i].MCPAccessTokens = append(users[i].MCPAccessTokens, record)
		if err := s.saveLocked(users); err != nil {
			return CreatedMCPAccessToken{}, err
		}
		return CreatedMCPAccessToken{PublicMCPAccessToken: publicMCPAccessToken(record), Secret: secret}, nil
	}
	return CreatedMCPAccessToken{}, errors.New("user not found")
}

func compactMCPAccessTokens(tokens []MCPAccessTokenRecord, now time.Time) []MCPAccessTokenRecord {
	kept := make([]MCPAccessTokenRecord, 0, len(tokens))
	for _, token := range tokens {
		inactiveAt := token.ExpiresAt
		if token.RevokedAt != nil {
			inactiveAt = *token.RevokedAt
		}
		if (token.RevokedAt != nil || !token.ExpiresAt.After(now)) && now.Sub(inactiveAt) > mcpInactiveTokenRetention {
			continue
		}
		kept = append(kept, token)
	}
	if len(kept) < mcpAccessTokenMaxStored {
		return kept
	}
	drop := len(kept) - (mcpAccessTokenMaxStored - 1)
	out := make([]MCPAccessTokenRecord, 0, len(kept)-drop)
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

func (s *UserStore) ListMCPAccessTokens(username string) ([]PublicMCPAccessToken, error) {
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
		out := make([]PublicMCPAccessToken, 0, len(user.MCPAccessTokens))
		for _, token := range user.MCPAccessTokens {
			out = append(out, publicMCPAccessToken(token))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
		return out, nil
	}
	return nil, errors.New("user not found")
}

func (s *UserStore) RevokeMCPAccessToken(username, id string) error {
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
		for j := range users[i].MCPAccessTokens {
			token := &users[i].MCPAccessTokens[j]
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

func (s *UserStore) AuthenticateMCPAccessToken(secret string) (AuthenticatedMCPToken, bool) {
	secret = strings.TrimSpace(secret)
	if !strings.HasPrefix(secret, mcpAccessTokenPrefix) || len(secret) > 256 {
		return AuthenticatedMCPToken{}, false
	}
	sum := sha256.Sum256([]byte(secret))
	want := []byte(hex.EncodeToString(sum[:]))
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return AuthenticatedMCPToken{}, false
	}
	for i := range users {
		if users[i].Disabled {
			continue
		}
		for j := range users[i].MCPAccessTokens {
			token := &users[i].MCPAccessTokens[j]
			if len(token.Hash) != len(want) || subtle.ConstantTimeCompare([]byte(token.Hash), want) != 1 {
				continue
			}
			if token.RevokedAt != nil || !token.ExpiresAt.After(now) {
				return AuthenticatedMCPToken{}, false
			}
			if token.LastUsedAt == nil || now.Sub(*token.LastUsedAt) >= mcpLastUsedWriteInterval {
				token.LastUsedAt = &now
				_ = s.saveLocked(users)
			}
			return AuthenticatedMCPToken{
				User: users[i].Username, Role: users[i].Role, ID: token.ID, Scopes: append([]string(nil), token.Scopes...),
			}, true
		}
	}
	return AuthenticatedMCPToken{}, false
}

func normalizeMCPScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		scopes = mcpDefaultScopes
	}
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if _, ok := mcpSupportedScopes[scope]; !ok {
			return nil, fmt.Errorf("unsupported MCP scope %q", scope)
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

func publicMCPAccessToken(token MCPAccessTokenRecord) PublicMCPAccessToken {
	return PublicMCPAccessToken{
		ID: token.ID, Name: token.Name, Scopes: append([]string(nil), token.Scopes...), CreatedAt: token.CreatedAt,
		ExpiresAt: token.ExpiresAt, LastUsedAt: token.LastUsedAt, RevokedAt: token.RevokedAt,
	}
}
