package retry_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/retry"
)

func TestSleep_待機とコンテキストキャンセルを処理する(t *testing.T) {
	if err := retry.Sleep(context.Background(), 0); err != nil {
		t.Fatalf("Sleep(0) = %v, want nil", err)
	}
	if err := retry.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep(1ms) = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := retry.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Sleep = %v, want context.Canceled", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel2()
	if err := retry.Sleep(ctx2, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout Sleep = %v, want DeadlineExceeded", err)
	}
}

func TestExponentialDelay_指数バックオフと上限を返す(t *testing.T) {
	base := 30 * time.Second
	maxDelay := 30 * time.Minute
	want := []time.Duration{
		30 * time.Second,
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		30 * time.Minute,
		30 * time.Minute,
	}
	for failures, w := range want {
		if got := retry.ExponentialDelay(base, maxDelay, failures); got != w {
			t.Errorf("ExponentialDelay(failures=%d) = %v, want %v", failures, got, w)
		}
	}
	if got := retry.ExponentialDelay(base, maxDelay, -1); got != base {
		t.Errorf("negative failures = %v, want %v", got, base)
	}
	if got := retry.ExponentialDelay(time.Hour, 10*time.Minute, 0); got != 10*time.Minute {
		t.Errorf("base > max = %v, want 10m", got)
	}
	if got := retry.ExponentialDelay(0, 10*time.Minute, 1); got != 0 {
		t.Errorf("zero base = %v, want 0", got)
	}
	if got := retry.ExponentialDelay(base, maxDelay, math.MaxInt); got != maxDelay {
		t.Errorf("MaxInt failures = %v, want %v", got, maxDelay)
	}
	if got := retry.ExponentialDelay(time.Second, 0, 2); got != 4*time.Second {
		t.Errorf("uncapped delay = %v, want 4s", got)
	}
}

func TestDo_成功するまで再試行し永続エラーとキャンセルで止まる(t *testing.T) {
	var attempts int
	var notified []time.Duration
	err := retry.Do(context.Background(), retry.Config{
		InitialInterval: time.Millisecond,
		MaxInterval:     5 * time.Millisecond,
		Multiplier:      2,
		OnRetry: func(_ error, wait time.Duration) {
			notified = append(notified, wait)
		},
	}, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if attempts != 3 || len(notified) != 2 {
		t.Fatalf("attempts=%d notified=%v", attempts, notified)
	}

	// Permanent stops immediately without retrying.
	permCalls := 0
	sentinel := errors.New("already exists")
	err = retry.Do(context.Background(), retry.Config{
		InitialInterval: time.Millisecond,
		MaxInterval:     5 * time.Millisecond,
	}, func() error {
		permCalls++
		return retry.Permanent(sentinel)
	})
	if !errors.Is(err, sentinel) || permCalls != 1 {
		t.Fatalf("permanent: err=%v calls=%d", err, permCalls)
	}

	// Context cancellation stops retrying.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	err = retry.Do(ctx, retry.Config{
		InitialInterval: 2 * time.Millisecond,
		MaxInterval:     4 * time.Millisecond,
		Jitter:          0.2,
	}, func() error {
		return errors.New("still down")
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled Do = %v, want DeadlineExceeded", err)
	}

	// MaxTries and default intervals stop after exact attempts.
	maxTryCalls := 0
	err = retry.Do(context.Background(), retry.Config{
		MaxTries: 2,
	}, func() error {
		maxTryCalls++
		return errors.New("fail")
	})
	if err == nil || maxTryCalls != 2 {
		t.Fatalf("MaxTries: err=%v calls=%d, want err and 2 calls", err, maxTryCalls)
	}
}
