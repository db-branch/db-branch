// Package retry provides context-aware sleep and exponential backoff helpers
// backed by github.com/cenkalti/backoff/v5.
package retry

import (
	"context"
	"math"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// Config parameterizes exponential backoff retries for Do.
type Config struct {
	InitialInterval time.Duration
	MaxInterval     time.Duration
	Multiplier      float64
	Jitter          float64
	MaxElapsedTime  time.Duration
	MaxTries        uint
	OnRetry         func(err error, wait time.Duration)
}

// Sleep pauses for d or until ctx is cancelled.
func Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Permanent marks err as non-retryable so Do stops immediately and returns it.
func Permanent(err error) error {
	return backoff.Permanent(err)
}

// ExponentialDelay returns the deterministic exponential backoff wait after
// failures consecutive errors, starting at base and capped at maxDelay.
func ExponentialDelay(base, maxDelay time.Duration, failures int) time.Duration {
	if base <= 0 {
		return 0
	}
	if maxDelay > 0 && base > maxDelay {
		base = maxDelay
	}
	if failures <= 0 {
		return base
	}
	capDelay := maxDelay
	if capDelay <= 0 {
		capDelay = time.Duration(math.MaxInt64)
	}
	b := &backoff.ExponentialBackOff{
		InitialInterval:     base,
		RandomizationFactor: 0,
		Multiplier:          2,
		MaxInterval:         capDelay,
	}
	b.Reset()
	d := b.NextBackOff()
	for i := 0; i < failures; i++ {
		d = b.NextBackOff()
		if d >= capDelay {
			return capDelay
		}
	}
	return d
}

// Do retries op until it returns nil, returns a Permanent error, hits MaxTries /
// MaxElapsedTime, or ctx is cancelled. A zero MaxElapsedTime retries until ctx
// ends or a permanent error occurs.
func Do(ctx context.Context, cfg Config, op func() error) error {
	b := backoff.NewExponentialBackOff()
	if cfg.InitialInterval > 0 {
		b.InitialInterval = cfg.InitialInterval
	}
	if cfg.MaxInterval > 0 {
		b.MaxInterval = cfg.MaxInterval
	}
	if cfg.Multiplier > 0 {
		b.Multiplier = cfg.Multiplier
	} else {
		b.Multiplier = 2
	}
	b.RandomizationFactor = cfg.Jitter

	opts := []backoff.RetryOption{
		backoff.WithBackOff(b),
		backoff.WithMaxElapsedTime(cfg.MaxElapsedTime),
	}
	if cfg.MaxTries > 0 {
		opts = append(opts, backoff.WithMaxTries(cfg.MaxTries))
	}
	if cfg.OnRetry != nil {
		opts = append(opts, backoff.WithNotify(cfg.OnRetry))
	}
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		return struct{}{}, op()
	}, opts...)
	return err
}
