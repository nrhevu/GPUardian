package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("history database path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = absolutePath
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	dsnURL := url.URL{Scheme: "file", Path: path}
	query := dsnURL.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)", "busy_timeout(5000)", "trusted_schema(OFF)"} {
		query.Add("_pragma", pragma)
	}
	dsnURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsnURL.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	store := &Store{db: db}
	if err := store.configure(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA trusted_schema=OFF",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure sqlite: %w", err)
		}
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, migrationV1); err != nil {
		return fmt.Errorf("apply history migration v1: %w", err)
	}
	checksumBytes := sha256.Sum256([]byte(migrationV1))
	checksum := hex.EncodeToString(checksumBytes[:])
	var existing string
	err = tx.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version=1").Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,checksum,applied_at_ms) VALUES(1,?,?)", checksum, time.Now().UTC().UnixMilli()); err != nil {
			return err
		}
	case err != nil:
		return err
	case existing != checksum:
		return errors.New("history migration v1 checksum mismatch")
	}
	var newest int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&newest); err != nil {
		return err
	}
	if newest > 1 {
		return fmt.Errorf("history database schema %d is newer than this binary", newest)
	}
	return tx.Commit()
}

func sessionID(nodeID, groupID string) string {
	sum := sha256.Sum256([]byte(nodeID + "\x00" + groupID))
	return "sess_" + hex.EncodeToString(sum[:12])
}

func publicJobID(nodeID, jobID string) string {
	sum := sha256.Sum256([]byte(nodeID + "\x00" + jobID))
	return "job_" + hex.EncodeToString(sum[:12])
}

func millis(value time.Time) int64 { return value.UTC().UnixMilli() }

func nullableMillis(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return millis(*value)
}

func timeFromMillis(value int64) time.Time { return time.UnixMilli(value).UTC() }

func timePtrFromNull(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	t := timeFromMillis(value.Int64)
	return &t
}
