package gpusmi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuardian/internal/model"
)

func TestProcessFlightCoalescesConcurrentCallers(t *testing.T) {
	var flight ProcessFlight
	var calls atomic.Int32
	release := make(chan struct{})
	sample := func(context.Context) ([]model.GPUProcess, error) {
		calls.Add(1)
		<-release
		return []model.GPUProcess{{GPU: 1, PID: 42}}, nil
	}
	const callers = 8
	results := make(chan []model.GPUProcess, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := flight.Do(context.Background(), time.Time{}, sample)
			if err != nil {
				t.Error(err)
			}
			results <- rows
		}()
	}
	waitFor(t, func() bool { return calls.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	if got := calls.Load(); got != 1 {
		t.Fatalf("samples = %d, want 1 shared sample", got)
	}
	var first []model.GPUProcess
	for rows := range results {
		if len(rows) != 1 || rows[0].PID != 42 {
			t.Fatalf("rows = %+v", rows)
		}
		if first == nil {
			first = rows
			continue
		}
		rows[0].PID = 7
		if first[0].PID != 42 {
			t.Fatal("callers share one rows slice")
		}
	}
}

func TestProcessFlightFreshnessAndCancellation(t *testing.T) {
	tests := []struct {
		name        string
		notBefore   func(started time.Time) time.Time
		wantSamples int32
	}{
		{name: "joins sample started after request", notBefore: func(started time.Time) time.Time { return started.Add(-time.Second) }, wantSamples: 1},
		{name: "never joins older sample", notBefore: func(started time.Time) time.Time { return started.Add(time.Second) }, wantSamples: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var flight ProcessFlight
			var calls atomic.Int32
			release := make(chan struct{})
			sample := func(context.Context) ([]model.GPUProcess, error) {
				calls.Add(1)
				<-release
				return nil, nil
			}
			started := time.Now()
			first := make(chan error, 1)
			go func() {
				_, err := flight.Do(context.Background(), time.Time{}, sample)
				first <- err
			}()
			waitFor(t, func() bool { return calls.Load() == 1 })
			second := make(chan error, 1)
			go func() {
				_, err := flight.Do(context.Background(), tc.notBefore(started), sample)
				second <- err
			}()
			time.Sleep(50 * time.Millisecond)
			close(release)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if err := <-second; err != nil {
				t.Fatal(err)
			}
			if got := calls.Load(); got != tc.wantSamples {
				t.Fatalf("samples = %d, want %d", got, tc.wantSamples)
			}
		})
	}

	t.Run("cancelled caller does not cancel shared sample", func(t *testing.T) {
		var flight ProcessFlight
		release := make(chan struct{})
		var sampleErr atomic.Value
		sample := func(ctx context.Context) ([]model.GPUProcess, error) {
			<-release
			if err := ctx.Err(); err != nil {
				sampleErr.Store(err)
				return nil, err
			}
			return []model.GPUProcess{{PID: 1}}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancelled := make(chan error, 1)
		go func() {
			_, err := flight.Do(ctx, time.Time{}, sample)
			cancelled <- err
		}()
		other := make(chan []model.GPUProcess, 1)
		go func() {
			time.Sleep(10 * time.Millisecond)
			rows, _ := flight.Do(context.Background(), time.Time{}, sample)
			other <- rows
		}()
		time.Sleep(20 * time.Millisecond)
		cancel()
		if err := <-cancelled; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller err = %v", err)
		}
		close(release)
		if rows := <-other; len(rows) != 1 {
			t.Fatalf("surviving caller rows = %+v, sample err = %v", rows, sampleErr.Load())
		}
	})
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
