package web

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const fixedKeySecretBytes = 24

type userKeyCipher struct {
	aead cipher.AEAD
}

type FixedUserKey struct {
	ID        string                 `json:"id"`
	Owner     string                 `json:"owner"`
	Version   int64                  `json:"version"`
	CreatedAt time.Time              `json:"created_at"`
	Secret    string                 `json:"key,omitempty"`
	NodeSync  []ManagedKeyNodeStatus `json:"node_sync,omitempty"`
}

type FixedUserKeyVerifier struct {
	ID      string `json:"id"`
	Owner   string `json:"owner"`
	Version int64  `json:"version"`
	Hash    string `json:"hash"`
}

func newUserKeyCipher(master []byte) (*userKeyCipher, error) {
	if len(master) != 32 {
		return nil, errors.New("web user-key master must contain 32 bytes")
	}
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &userKeyCipher{aead: aead}, nil
}

func (c *userKeyCipher) assign(record *UserRecord, version int64) error {
	if c == nil || record == nil {
		return errors.New("user-key encryption is unavailable")
	}
	secretBytes := make([]byte, fixedKeySecretBytes)
	if _, err := io.ReadFull(rand.Reader, secretBytes); err != nil {
		return err
	}
	idBytes := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, idBytes); err != nil {
		return err
	}
	secret := "gk_" + hex.EncodeToString(secretBytes)
	record.KeyID = "uk_" + hex.EncodeToString(idBytes)
	record.KeyVersion = version
	record.KeyCreatedAt = time.Now().UTC()
	digest := sha256.Sum256([]byte(secret))
	record.KeyHash = hex.EncodeToString(digest[:])
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := c.aead.Seal(nil, nonce, []byte(secret), fixedKeyAAD(*record))
	record.KeyCipher = base64.RawStdEncoding.EncodeToString(append(nonce, sealed...))
	return nil
}

func (c *userKeyCipher) reveal(record UserRecord) (string, error) {
	if c == nil {
		return "", errors.New("user-key encryption is unavailable")
	}
	raw, err := base64.RawStdEncoding.DecodeString(record.KeyCipher)
	if err != nil || len(raw) <= c.aead.NonceSize() {
		return "", errors.New("stored user key is invalid")
	}
	nonce, ciphertext := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, fixedKeyAAD(record))
	if err != nil {
		return "", errors.New("decrypt stored user key: invalid master key or ciphertext")
	}
	return string(plain), nil
}

func fixedKeyAAD(record UserRecord) []byte {
	return []byte(fmt.Sprintf("gpuardian-user-key-v1\x00%s\x00%s\x00%d", record.Username, record.KeyID, record.KeyVersion))
}

func (s *UserStore) InitializeFixedKeys(master []byte) error {
	keyCipher, err := newUserKeyCipher(master)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	changed := false
	for i := range users {
		if users[i].Disabled {
			continue
		}
		if users[i].KeyID == "" {
			if err := keyCipher.assign(&users[i], 1); err != nil {
				return err
			}
			changed = true
			continue
		}
		if _, err := keyCipher.reveal(users[i]); err != nil {
			return fmt.Errorf("validate fixed key for %s: %w", users[i].Username, err)
		}
	}
	if changed {
		if err := s.saveLocked(users); err != nil {
			return err
		}
	}
	s.keyCipher = keyCipher
	return nil
}

func (s *UserStore) HasEncryptedFixedKeys() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	for _, user := range users {
		if !user.Disabled && user.KeyCipher != "" {
			return true, nil
		}
	}
	return false, nil
}

func loadOrCreateUserKeyMaster(path string, users *UserStore) ([]byte, error) {
	hasEncryptedKeys, err := users.HasEncryptedFixedKeys()
	if err != nil {
		return nil, err
	}
	if hasEncryptedKeys {
		master, err := readSessionKey(path)
		if err != nil {
			return nil, fmt.Errorf("read web user-key master: %w", err)
		}
		return master, nil
	}
	return loadOrCreateSessionKey(path)
}

func (s *UserStore) FixedKeys() ([]FixedUserKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]FixedUserKey, 0, len(users))
	for _, user := range users {
		if user.Disabled || user.KeyID == "" {
			continue
		}
		out = append(out, fixedUserKey(user, ""))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out, nil
}

func (s *UserStore) FixedKeyVerifiers() ([]FixedUserKeyVerifier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]FixedUserKeyVerifier, 0, len(users))
	for _, user := range users {
		if user.Disabled || user.KeyID == "" {
			continue
		}
		out = append(out, FixedUserKeyVerifier{ID: user.KeyID, Owner: user.Username, Version: user.KeyVersion, Hash: user.KeyHash})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *UserStore) RevealFixedKey(username string) (FixedUserKey, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return FixedUserKey{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return FixedUserKey{}, err
	}
	for _, user := range users {
		if user.Username != username || user.Disabled {
			continue
		}
		secret, err := s.keyCipher.reveal(user)
		if err != nil {
			return FixedUserKey{}, err
		}
		return fixedUserKey(user, secret), nil
	}
	return FixedUserKey{}, errors.New("user not found")
}

func (s *UserStore) RegenerateFixedKey(username string) (FixedUserKey, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return FixedUserKey{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return FixedUserKey{}, err
	}
	for i := range users {
		if users[i].Username != username || users[i].Disabled {
			continue
		}
		version := users[i].KeyVersion + 1
		if version < 1 {
			version = 1
		}
		if err := s.keyCipher.assign(&users[i], version); err != nil {
			return FixedUserKey{}, err
		}
		if err := s.saveLocked(users); err != nil {
			return FixedUserKey{}, err
		}
		secret, err := s.keyCipher.reveal(users[i])
		if err != nil {
			return FixedUserKey{}, err
		}
		return fixedUserKey(users[i], secret), nil
	}
	return FixedUserKey{}, errors.New("user not found")
}

func (s *UserStore) FixedKeyForUser(username string) (FixedUserKeyVerifier, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	verifiers, err := s.FixedKeyVerifiers()
	if err != nil {
		return FixedUserKeyVerifier{}, err
	}
	for _, verifier := range verifiers {
		if verifier.Owner == username {
			return verifier, nil
		}
	}
	return FixedUserKeyVerifier{}, errors.New("fixed key not found")
}

func fixedUserKey(user UserRecord, secret string) FixedUserKey {
	return FixedUserKey{ID: user.KeyID, Owner: user.Username, Version: user.KeyVersion, CreatedAt: user.KeyCreatedAt, Secret: secret}
}
