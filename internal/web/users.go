package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	passwordHashScheme        = "pbkdf2-sha256"
	legacyPasswordHashScheme  = "sha256"
	passwordHashIterations    = 600_000
	passwordSaltBytes         = 16
	passwordKeyBytes          = 32
	minPasswordBytes          = 12
	maxPasswordBytes          = 1024
	maxUsernameBytes          = 64
	maxUsersFileBytes         = 10 << 20
	maxUsers                  = 256
	maxConcurrentPasswordWork = 4
	maxQueuedPasswordWork     = 32
)

var (
	errPasswordWorkBusy = errors.New("password authentication is temporarily busy")
	errUsernameInUse    = errors.New("username is already in use or was previously deleted")
	errUserLimitReached = errors.New("user limit reached")
)

type UserRecord struct {
	Username           string              `json:"username"`
	Role               string              `json:"role"`
	PasswordHash       string              `json:"password_hash"`
	KeyID              string              `json:"key_id,omitempty"`
	KeyHash            string              `json:"key_hash,omitempty"`
	KeyVersion         int64               `json:"key_version,omitempty"`
	KeyCipher          string              `json:"key_cipher,omitempty"`
	KeyCreatedAt       time.Time           `json:"key_created_at,omitempty"`
	AccessTokens       []AccessTokenRecord `json:"access_tokens,omitempty"`
	LegacyAccessTokens []AccessTokenRecord `json:"mcp_access_tokens,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
	Disabled           bool                `json:"disabled,omitempty"`
}

type PublicUser struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type UserStore struct {
	mu            sync.Mutex
	path          string
	loaded        bool
	users         []UserRecord
	passwordWork  chan struct{}
	passwordQueue chan struct{}
	keyCipher     *userKeyCipher
}

func NewUserStore(path string) *UserStore {
	return &UserStore{
		path:          path,
		passwordWork:  make(chan struct{}, maxConcurrentPasswordWork),
		passwordQueue: make(chan struct{}, maxQueuedPasswordWork),
	}
}

func (s *UserStore) BootstrapAdmin(username, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return nil
	}
	record, err := newUserRecord(username, password, RoleAdmin)
	if err != nil {
		return err
	}
	return s.saveLocked([]UserRecord{record})
}

func (s *UserStore) List() ([]PublicUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	users, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]PublicUser, 0, len(users))
	for _, user := range users {
		if user.Disabled {
			continue
		}
		out = append(out, publicUser(user))
	}
	return out, nil
}

func (s *UserStore) Get(username string) (UserRecord, bool, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return UserRecord{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	users, err := s.loadLocked()
	if err != nil {
		return UserRecord{}, false, err
	}
	for _, user := range users {
		if user.Username == username && !user.Disabled {
			return user, true, nil
		}
	}
	return UserRecord{}, false, nil
}

func (s *UserStore) Authenticate(username, password string) (PublicUser, error) {
	return s.authenticate(context.Background(), username, password, false)
}

func (s *UserStore) AuthenticateContext(ctx context.Context, username, password string) (PublicUser, error) {
	return s.authenticate(ctx, username, password, true)
}

func (s *UserStore) authenticate(ctx context.Context, username, password string, wait bool) (PublicUser, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return PublicUser{}, err
	}
	s.mu.Lock()
	users, err := s.loadLocked()
	if err != nil {
		s.mu.Unlock()
		return PublicUser{}, err
	}
	var candidate UserRecord
	for _, user := range users {
		if user.Username != username {
			continue
		}
		if user.Disabled {
			break
		}
		candidate = user
		break
	}
	s.mu.Unlock()

	releasePasswordWork, err := s.acquirePasswordWork(ctx, wait)
	if err != nil {
		return PublicUser{}, err
	}
	verified := false
	if candidate.Username == "" {
		verifyDummyPassword(password)
	} else {
		verified = verifyPasswordHash(candidate.PasswordHash, password)
	}
	var upgradedHash string
	if verified && passwordHashNeedsUpgrade(candidate.PasswordHash) {
		upgradedHash, err = hashPasswordForMigration(password)
	}
	releasePasswordWork()
	if err != nil {
		return PublicUser{}, err
	}
	if !verified {
		return PublicUser{}, errors.New("invalid username or password")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	users, err = s.loadLocked()
	if err != nil {
		return PublicUser{}, err
	}
	for i := range users {
		current := users[i]
		if current.Username != username || current.Disabled {
			continue
		}
		if current.PasswordHash != candidate.PasswordHash || !current.UpdatedAt.Equal(candidate.UpdatedAt) {
			return PublicUser{}, errors.New("invalid username or password")
		}
		if upgradedHash != "" {
			users[i].PasswordHash = upgradedHash
			users[i].UpdatedAt = time.Now().UTC()
			if err := s.saveLocked(users); err != nil {
				return PublicUser{}, err
			}
			current = users[i]
		}
		return publicUser(current), nil
	}
	return PublicUser{}, errors.New("invalid username or password")
}

func (s *UserStore) Create(username, password, role string) (PublicUser, error) {
	if !s.tryAcquirePasswordWork() {
		return PublicUser{}, errPasswordWorkBusy
	}
	record, err := newUserRecord(username, password, role)
	s.releasePasswordWork()
	if err != nil {
		return PublicUser{}, err
	}
	if s.keyCipher != nil {
		if err := s.keyCipher.assign(&record, 1); err != nil {
			return PublicUser{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	users, err := s.loadLocked()
	if err != nil {
		return PublicUser{}, err
	}
	for _, user := range users {
		if user.Username == record.Username {
			return PublicUser{}, errUsernameInUse
		}
	}
	if len(users) >= maxUsers {
		return PublicUser{}, fmt.Errorf("%w: maximum %d", errUserLimitReached, maxUsers)
	}
	users = append(users, record)
	if err := s.saveLocked(users); err != nil {
		return PublicUser{}, err
	}
	return publicUser(record), nil
}

func (s *UserStore) Delete(username string) error {
	username, err := normalizeUsername(username)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i, user := range users {
		if user.Username != username {
			continue
		}
		if user.Disabled {
			return errors.New("user not found")
		}
		if user.Role == RoleAdmin {
			adminCount := 0
			for _, candidate := range users {
				if candidate.Role == RoleAdmin && !candidate.Disabled {
					adminCount++
				}
			}
			if adminCount == 1 {
				return errors.New("cannot delete the last admin")
			}
		}
		users[i].PasswordHash = ""
		users[i].KeyCipher = ""
		users[i].AccessTokens = nil
		users[i].Disabled = true
		users[i].UpdatedAt = time.Now().UTC()
		return s.saveLocked(users)
	}
	return errors.New("user not found")
}

func (s *UserStore) ChangePassword(username, currentPassword, newPassword string) (PublicUser, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return PublicUser{}, err
	}
	if strings.TrimSpace(newPassword) == "" {
		return PublicUser{}, errors.New("new password is required")
	}
	if err := validatePassword(newPassword); err != nil {
		return PublicUser{}, err
	}

	s.mu.Lock()
	users, err := s.loadLocked()
	if err != nil {
		s.mu.Unlock()
		return PublicUser{}, err
	}
	var candidate UserRecord
	for _, user := range users {
		if user.Username != username || user.Disabled {
			continue
		}
		candidate = user
		break
	}
	s.mu.Unlock()
	if candidate.Username == "" {
		if !s.tryAcquirePasswordWork() {
			return PublicUser{}, errPasswordWorkBusy
		}
		verifyDummyPassword(currentPassword)
		s.releasePasswordWork()
		return PublicUser{}, errors.New("user not found")
	}
	if !s.tryAcquirePasswordWork() {
		return PublicUser{}, errPasswordWorkBusy
	}
	verified := verifyPasswordHash(candidate.PasswordHash, currentPassword)
	var newHash string
	if verified {
		newHash, err = hashPassword(newPassword)
	}
	s.releasePasswordWork()
	if err != nil {
		return PublicUser{}, err
	}
	if !verified {
		return PublicUser{}, errors.New("current password is invalid")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	users, err = s.loadLocked()
	if err != nil {
		return PublicUser{}, err
	}
	for i := range users {
		current := users[i]
		if current.Username != username || current.Disabled {
			continue
		}
		if current.PasswordHash != candidate.PasswordHash || !current.UpdatedAt.Equal(candidate.UpdatedAt) {
			return PublicUser{}, errors.New("current password is invalid")
		}
		users[i].PasswordHash = newHash
		users[i].UpdatedAt = time.Now().UTC()
		if err := s.saveLocked(users); err != nil {
			return PublicUser{}, err
		}
		return publicUser(users[i]), nil
	}
	return PublicUser{}, errors.New("user not found")
}

func (s *UserStore) tryAcquirePasswordWork() bool {
	select {
	case s.passwordWork <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *UserStore) acquirePasswordWork(ctx context.Context, wait bool) (func(), error) {
	if !wait {
		if !s.tryAcquirePasswordWork() {
			return nil, errPasswordWorkBusy
		}
		return s.releasePasswordWork, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.passwordQueue <- struct{}{}:
	default:
		return nil, errPasswordWorkBusy
	}
	select {
	case s.passwordWork <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.passwordWork
			<-s.passwordQueue
			return nil, err
		}
		return func() {
			<-s.passwordWork
			<-s.passwordQueue
		}, nil
	case <-ctx.Done():
		<-s.passwordQueue
		return nil, ctx.Err()
	}
}

func (s *UserStore) releasePasswordWork() {
	<-s.passwordWork
}

func (s *UserStore) loadLocked() ([]UserRecord, error) {
	if s.loaded {
		return cloneUserRecords(s.users), nil
	}
	data, err := readPrivateFile(s.path, "users file", maxUsersFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		s.loaded = true
		s.users = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		s.loaded = true
		s.users = nil
		return nil, nil
	}
	var users []UserRecord
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, err
	}
	if len(users) > maxUsers {
		return nil, fmt.Errorf("users file contains %d records; maximum is %d", len(users), maxUsers)
	}
	for i := range users {
		if len(users[i].AccessTokens) == 0 {
			users[i].AccessTokens = users[i].LegacyAccessTokens
		}
		users[i].LegacyAccessTokens = nil
	}
	s.users = cloneUserRecords(users)
	s.loaded = true
	return cloneUserRecords(users), nil
}

func (s *UserStore) saveLocked(users []UserRecord) error {
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	committed, err := writePrivateFile(s.path, append(data, '\n'))
	if committed {
		s.users = cloneUserRecords(users)
		s.loaded = true
	}
	return err
}

func cloneUserRecords(users []UserRecord) []UserRecord {
	out := append([]UserRecord(nil), users...)
	for i := range out {
		out[i].AccessTokens = append([]AccessTokenRecord(nil), users[i].AccessTokens...)
		for j := range out[i].AccessTokens {
			out[i].AccessTokens[j].Scopes = append([]string(nil), users[i].AccessTokens[j].Scopes...)
			if users[i].AccessTokens[j].LastUsedAt != nil {
				lastUsed := *users[i].AccessTokens[j].LastUsedAt
				out[i].AccessTokens[j].LastUsedAt = &lastUsed
			}
			if users[i].AccessTokens[j].RevokedAt != nil {
				revoked := *users[i].AccessTokens[j].RevokedAt
				out[i].AccessTokens[j].RevokedAt = &revoked
			}
		}
	}
	return out
}

func newUserRecord(username, password, role string) (UserRecord, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return UserRecord{}, err
	}
	role = normalizeRole(role)
	if err := validatePassword(password); err != nil {
		return UserRecord{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return UserRecord{}, err
	}
	now := time.Now().UTC()
	return UserRecord{
		Username:     username,
		Role:         role,
		PasswordHash: hash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

func normalizeUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return "", errors.New("username is required")
	}
	if len(username) > maxUsernameBytes {
		return "", fmt.Errorf("username must be at most %d bytes", maxUsernameBytes)
	}
	for _, r := range username {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("username cannot contain whitespace or control characters")
		}
	}
	return username, nil
}

func normalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case RoleAdmin:
		return RoleAdmin
	default:
		return RoleUser
	}
}

func publicUser(user UserRecord) PublicUser {
	return PublicUser{
		Username:  user.Username,
		Role:      user.Role,
		CreatedAt: user.CreatedAt,
		UpdatedAt: user.UpdatedAt,
	}
}

func hashPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	return encodePasswordHash(password)
}

func hashPasswordForMigration(password string) (string, error) {
	if len(password) > maxPasswordBytes {
		return "", fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	}
	return encodePasswordHash(password)
}

func encodePasswordHash(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := pbkdf2SHA256([]byte(password), salt, passwordHashIterations, passwordKeyBytes)
	return fmt.Sprintf("%s$%d$%s$%s", passwordHashScheme, passwordHashIterations, hex.EncodeToString(salt), hex.EncodeToString(sum)), nil
}

func verifyPasswordHash(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) > 0 && parts[0] == legacyPasswordHashScheme {
		verified := len(parts) == 3 && verifyLegacyPasswordHash(parts, password)
		if !verified {
			verifyDummyPassword(password)
		}
		return verified
	}
	if len(parts) != 4 || parts[0] != passwordHashScheme {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 100_000 || iterations > 2_000_000 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil || len(salt) < passwordSaltBytes {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) != passwordKeyBytes {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iterations, len(want))
	return len(want) == len(got) && subtle.ConstantTimeCompare(want, got) == 1
}

func verifyLegacyPasswordHash(parts []string, password string) bool {
	salt, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	h := sha256.New()
	_, _ = h.Write(salt)
	_, _ = h.Write([]byte(password))
	got := h.Sum(nil)
	return len(want) == len(got) && subtle.ConstantTimeCompare(want, got) == 1
}

func passwordHashNeedsUpgrade(encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != passwordHashScheme {
		return true
	}
	iterations, err := strconv.Atoi(parts[1])
	return err != nil || iterations < passwordHashIterations
}

func validatePassword(password string) error {
	if strings.TrimSpace(password) == "" {
		return errors.New("password is required")
	}
	if len(password) < minPasswordBytes {
		return fmt.Errorf("password must be at least %d bytes", minPasswordBytes)
	}
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	}
	return nil
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	var counter [4]byte
	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		_, _ = mac.Write(counter[:])
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			mac.Reset()
			_, _ = mac.Write(u)
			u = mac.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func verifyDummyPassword(password string) {
	var salt [passwordSaltBytes]byte
	got := pbkdf2SHA256([]byte(password), salt[:], passwordHashIterations, passwordKeyBytes)
	var want [passwordKeyBytes]byte
	_ = subtle.ConstantTimeCompare(want[:], got)
}

func writePrivateFile(path string, data []byte) (committed bool, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return false, err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return true, err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return true, err
	}
	return true, nil
}

func readPrivateFile(path, description string, limit int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("open %s: invalid file descriptor", description)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", description)
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s permissions must not allow group or other access", description)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", description, limit)
	}
	return data, nil
}
