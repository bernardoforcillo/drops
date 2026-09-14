package cloudflare

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// RetryPolicy decides whether a failed request is tried again, and
// how long to wait first.
//
// The zero value retries nothing. [DefaultRetryPolicy] is what a
// Client gets when [WithRetryPolicy] is not used.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, retries
	// included. 1 (or 0) means no retry.
	MaxAttempts int

	// Backoff returns how long to wait before attempt n+1, given
	// the 1-based number of the attempt that just failed. nil means
	// no wait. A Retry-After header on the response overrides it
	// when RespectRetryAfter is set.
	Backoff func(attempt int) time.Duration

	// RespectRetryAfter uses the response's Retry-After header in
	// place of Backoff when the server sends one. Cloudflare sends
	// it on 429, and it is a better number than any local guess.
	RespectRetryAfter bool

	// MaxRetryAfter caps how long a Retry-After header may park the
	// caller. A server asking for ten minutes should not silently
	// become a ten-minute call; past the cap the request fails with
	// the rate-limit error instead of waiting. Zero means one
	// minute.
	MaxRetryAfter time.Duration

	// RetryOn reports whether a reply is worth another attempt.
	// status is 0 when the request failed at the transport level
	// (err is then non-nil). nil means [RetryableStatus].
	RetryOn func(status int, err error) bool
}

// DefaultRetryPolicy retries up to three times on 429 and 5xx,
// honouring Retry-After and otherwise backing off exponentially from
// 250ms with full jitter.
//
// Jitter is not decoration. A Worker that fans out to D1 and hits a
// rate limit retries from every colo at once; a fixed backoff has
// them all come back at the same instant and hit it again.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:       3,
		Backoff:           ExponentialBackoff(250*time.Millisecond, 5*time.Second),
		RespectRetryAfter: true,
		MaxRetryAfter:     time.Minute,
		RetryOn:           RetryableStatus,
	}
}

// ExponentialBackoff returns a Backoff that doubles from base, caps at
// max, and applies full jitter — a uniform draw from [0, d) rather
// than d itself.
func ExponentialBackoff(base, maxWait time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		d := base
		for i := 1; i < attempt && d < maxWait; i++ {
			d *= 2
		}
		if d > maxWait {
			d = maxWait
		}
		if d <= 0 {
			return 0
		}
		return time.Duration(rand.Int64N(int64(d)))
	}
}

// RetryableStatus is the default RetryOn: 429 and every 5xx are worth
// another attempt, and so is a transport-level failure.
//
// 5xx from Cloudflare's edge is frequently a 520–526 — the edge could
// not reach, or could not understand, the origin — and those clear on
// their own often enough to be worth a second look. A 4xx other than
// 429 does not: the request will be malformed the second time too.
func RetryableStatus(status int, err error) bool {
	if err != nil {
		return true
	}
	return status == http.StatusTooManyRequests || status >= 500
}

// attempts returns the effective attempt count, never below 1.
func (p RetryPolicy) attempts() int {
	if p.MaxAttempts < 1 {
		return 1
	}
	return p.MaxAttempts
}

// shouldRetry applies RetryOn, defaulting to RetryableStatus.
func (p RetryPolicy) shouldRetry(status int, err error) bool {
	if p.RetryOn == nil {
		return RetryableStatus(status, err)
	}
	return p.RetryOn(status, err)
}

// maxRetryAfter returns the cap on a Retry-After wait.
func (p RetryPolicy) maxRetryAfter() time.Duration {
	if p.MaxRetryAfter <= 0 {
		return time.Minute
	}
	return p.MaxRetryAfter
}

// wait returns how long to pause after the given 1-based attempt,
// and whether waiting is allowed at all. A Retry-After longer than
// the cap returns ok=false: the caller then stops rather than parking
// the goroutine for however long the server asked.
func (p RetryPolicy) wait(attempt int, header http.Header) (d time.Duration, ok bool) {
	if p.RespectRetryAfter && header != nil {
		if after, has := RetryAfter(header); has {
			if after > p.maxRetryAfter() {
				return 0, false
			}
			return after, true
		}
	}
	if p.Backoff == nil {
		return 0, true
	}
	return p.Backoff(attempt), true
}

// RetryAfter reads a Retry-After header in either of the two forms
// RFC 9110 allows — a delay in seconds, or an HTTP date — and reports
// whether one was present and usable.
//
// A date in the past yields zero rather than a negative duration, so
// a clock skewed the wrong way costs a caller nothing.
func RetryAfter(header http.Header) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}
	raw := header.Get("Retry-After")
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(raw); err == nil {
		d := time.Until(when)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// sleep waits for d, or returns the context's error if it is done
// first. A zero or negative d returns immediately without consulting
// the context, so a policy with no backoff does not turn an
// already-cancelled context into an extra failure mode.
func sleep(ctx context.Context, d time.Duration) error {
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

// isContextError reports whether err came from the caller's context
// rather than the network. Those are never retried: the caller has
// already given up.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
