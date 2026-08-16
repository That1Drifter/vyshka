package hub

import (
	"testing"
	"time"
)

// The bound has to hold in seconds, before the conversion. Multiplying a large
// request by time.Second overflows int64 and wraps, and a wrapped value sails
// under whichever bound it was meant to hit: the same trap clampActionTTL
// avoids for action deadlines.
func TestClampTokenTTL(t *testing.T) {
	t.Parallel()

	for _, one := range []struct {
		seconds int
		want    time.Duration
		why     string
	}{
		{0, 0, "zero means no expiry"},
		{-1, 0, "so does a negative, rather than a clamp to the floor"},
		{-9223372037, 0, "a negative large enough to overflow the conversion"},
		{1, minTokenTTL, "below the floor, clamped up"},
		{30, minTokenTTL, "still below the floor"},
		{3600, time.Hour, "an ordinary request, untouched"},
		{9223372037, maxTokenTTL, "the overflow case: ~292 years, clamped to the ceiling"},
		{1 << 62, maxTokenTTL, "far past the ceiling"},
	} {
		if got := clampTokenTTL(one.seconds); got != one.want {
			t.Errorf("clampTokenTTL(%d) = %s, want %s (%s)",
				one.seconds, got, one.want, one.why)
		}
	}

	// Whatever the request, the result is never negative and never a lifetime
	// nobody could have meant.
	for _, seconds := range []int{-1 << 62, -1, 0, 1, 1 << 31, 1 << 62} {
		got := clampTokenTTL(seconds)
		if got < 0 || got > maxTokenTTL {
			t.Errorf("clampTokenTTL(%d) = %s, outside [0, %s]", seconds, got, maxTokenTTL)
		}
	}
}
