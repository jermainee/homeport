package agent

import (
	"math/rand/v2"
	"time"
)

// backoff spaces out reconnection attempts. Each delay doubles up to max and
// is jittered to between half and all of it, so agents that lost the same
// gateway do not all come back at once.
type backoff struct {
	min, max time.Duration
	attempt  uint
}

func newBackoff(minDelay, maxDelay time.Duration) *backoff {
	return &backoff{min: minDelay, max: maxDelay}
}

// next returns the delay before the next attempt.
func (b *backoff) next() time.Duration {
	d := b.min
	for i := uint(0); i < b.attempt && d < b.max; i++ {
		d *= 2
	}
	b.attempt++
	return jitter(min(d, b.max))
}

// longest returns a jittered delay at the maximum.
func (b *backoff) longest() time.Duration { return jitter(b.max) }

// reset starts the next delay at the minimum again.
func (b *backoff) reset() { b.attempt = 0 }

func jitter(d time.Duration) time.Duration {
	half := d / 2
	return half + rand.N(d-half+1) //nolint:gosec // Jitter needs no cryptographic randomness.
}
