package history

import (
	"context"
	"fmt"
	"time"
)

const (
	ingestedEventRetention = 48 * time.Hour
	minuteRollupRetention  = 90 * 24 * time.Hour
	retentionBatchSize     = 5000
)

// PruneRetention deletes telemetry dedupe keys older than 48 hours and GPU
// minute rollups older than 90 days. Rows are removed in bounded batches, each
// in its own short write transaction under writeMu, so telemetry ingest can
// interleave with a large backlog. It returns early when ctx is cancelled;
// a later call resumes where it stopped.
func (s *Store) PruneRetention(ctx context.Context, now time.Time) error {
	return s.pruneRetention(ctx, now, retentionBatchSize)
}

func (s *Store) pruneRetention(ctx context.Context, now time.Time, batchSize int) error {
	now = now.UTC()
	targets := []struct {
		table  string
		column string
		cutoff int64
	}{
		{table: "ingested_event_ids", column: "ingested_at_ms", cutoff: millis(now.Add(-ingestedEventRetention))},
		{table: "gpu_minute_rollups", column: "minute_ms", cutoff: millis(now.Add(-minuteRollupRetention))},
		{table: "node_gpu_minute_rollups", column: "minute_ms", cutoff: millis(now.Add(-minuteRollupRetention))},
	}
	for _, target := range targets {
		statement := retentionDeleteSQL(target.table, target.column)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			deleted, err := s.pruneBatch(ctx, statement, target.cutoff, batchSize)
			if err != nil {
				return fmt.Errorf("prune %s: %w", target.table, err)
			}
			if deleted < int64(batchSize) {
				break
			}
		}
	}
	return nil
}

// retentionDeleteSQL selects one batch through the table's time index and
// deletes it by rowid. Table and column names are package constants.
func retentionDeleteSQL(table, column string) string {
	return "DELETE FROM " + table + " WHERE rowid IN (SELECT rowid FROM " + table + " WHERE " + column + "<? LIMIT ?)"
}

func (s *Store) pruneBatch(ctx context.Context, statement string, cutoff int64, batchSize int) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, statement, cutoff, batchSize)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
