package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a hand-driven time source, so refill arithmetic is checked to the
// nanosecond rather than against whatever the scheduler did.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func TestBurstThenRefusal(t *testing.T) {
	c := newClock()
	l := New(2, 5, c.now)
	for i := range 5 {
		if ok, _ := l.Allow("server-a"); !ok {
			t.Fatalf("request %d of a burst of 5 was refused", i+1)
		}
	}
	ok, wait := l.Allow("server-a")
	if ok {
		t.Fatal("the sixth request inside the same instant was admitted past a burst of 5")
	}
	// Empty bucket, 2 tokens/s: one token is half a second away.
	if wait != 500*time.Millisecond {
		t.Fatalf("wait = %s, want 500ms", wait)
	}
	if got := RetryAfterSeconds(wait); got != 1 {
		t.Fatalf("Retry-After for %s = %d, want 1 (rounded up, never below 1)", wait, got)
	}
}

func TestRefillArithmetic(t *testing.T) {
	c := newClock()
	l := New(4, 4, c.now)
	for range 4 {
		l.Allow("k")
	}
	// 250 ms refills exactly one token at 4/s.
	c.advance(249 * time.Millisecond)
	// Float refill rounds up to the next nanosecond, never down: a wait
	// that came out short would send a client back before its token.
	if ok, wait := l.Allow("k"); ok || wait < time.Millisecond || wait > time.Millisecond+time.Microsecond {
		t.Fatalf("at 249ms: allowed=%v wait=%s, want refused with 1ms to go", ok, wait)
	}
	c.advance(time.Millisecond)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("at 250ms one token had refilled and the request was refused")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("the refilled token was spent twice")
	}
	// A long quiet period refills to the burst and no further.
	c.advance(time.Hour)
	for i := range 4 {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d after an hour of quiet was refused; the bucket should be full", i+1)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("an hour of quiet banked more than the burst")
	}
}

// A refused request spends nothing, so a key hammering far above the limit
// still gets exactly the refill rate through, and its wait never grows.
func TestRefusalsDoNotBankDebt(t *testing.T) {
	c := newClock()
	l := New(10, 1, c.now)
	admitted := 0
	for range 1000 { // 1000 attempts over one second, 1 ms apart
		if ok, wait := l.Allow("flood"); ok {
			admitted++
		} else if wait > 100*time.Millisecond {
			t.Fatalf("wait grew to %s under a flood; refusals must not accrue debt", wait)
		}
		c.advance(time.Millisecond)
	}
	// The burst of 1 at t=0, then one per 100 ms for the rest of the second.
	if admitted != 10 {
		t.Fatalf("admitted %d of 1000 in one second at 10/s with burst 1, want 10", admitted)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	c := newClock()
	l := New(1, 2, c.now)
	l.Allow("flooder")
	l.Allow("flooder")
	if ok, _ := l.Allow("flooder"); ok {
		t.Fatal("flooder was admitted past its burst")
	}
	for i := range 2 {
		if ok, _ := l.Allow("quiet"); !ok {
			t.Fatalf("request %d from a key within its limit was refused because another key was flooding", i+1)
		}
	}
}

func TestDisabledAdmitsEverythingAndStoresNothing(t *testing.T) {
	for _, l := range []*Limiter{New(0, 10, nil), New(-1, 10, nil), nil} {
		for range 10000 {
			if ok, _ := l.Allow("k"); !ok {
				t.Fatal("a disabled limiter refused a request")
			}
		}
		if l.Enabled() || l.Len() != 0 {
			t.Fatalf("a disabled limiter reports enabled=%v and holds %d buckets", l.Enabled(), l.Len())
		}
	}
}

func TestBurstBelowOneIsOne(t *testing.T) {
	l := New(1, 0, newClock().now)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("burst 0 refused every request; it must be raised to 1")
	}
}

func TestClockGoingBackwardsRefillsNothing(t *testing.T) {
	c := newClock()
	l := New(1, 1, c.now)
	l.Allow("k")
	c.advance(-time.Hour)
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("a clock stepped back an hour refilled the bucket")
	}
}

func TestIdleBucketsAreSwept(t *testing.T) {
	c := newClock()
	l := New(1, 10, c.now)
	for i := range 50 {
		l.Allow(string(rune('a'+i%26)) + "-" + string(rune('0'+i/26)))
	}
	l.Allow("busy")
	if l.Len() != 51 {
		t.Fatalf("holds %d buckets after 51 keys, want 51", l.Len())
	}
	// A minute later every bucket that spent one token is full again (10
	// tokens at 1/s refill well inside it), except one kept busy.
	c.advance(sweepEvery)
	for range 10 {
		l.Allow("busy")
	}
	if l.Len() != 1 {
		t.Fatalf("holds %d buckets after the sweep, want only the busy one", l.Len())
	}
	// And a swept key starts again from a full bucket, as if never seen.
	for i := range 10 {
		if ok, _ := l.Allow("a-0"); !ok {
			t.Fatalf("request %d of a swept key's fresh burst was refused", i+1)
		}
	}
}

func TestConcurrentSpendingNeverExceedsTheBurst(t *testing.T) {
	c := newClock()
	l := New(1, 100, c.now)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if ok, _ := l.Allow("shared"); ok {
					admitted.Add(1)
				}
				l.Allow("other")
			}
		}()
	}
	wg.Wait()
	// The clock never moved, so exactly the burst is admitted out of 1600.
	if got := admitted.Load(); got != 100 {
		t.Fatalf("admitted %d concurrent requests against a burst of 100 with no refill, want 100", got)
	}
}

func TestReporterCollapsesARun(t *testing.T) {
	c := newClock()
	r := NewReporter(time.Minute, c.now)
	if report, skipped := r.Refused("t", 0); !report || skipped != 0 {
		t.Fatalf("first refusal: report=%v skipped=%d, want reported with nothing skipped", report, skipped)
	}
	r.Recorded("t", true)
	for i := range 499 {
		c.advance(100 * time.Millisecond)
		if report, _ := r.Refused("t", i+1); report {
			t.Fatal("a refusal inside the interval was reported again")
		}
	}
	c.advance(10*time.Second + 100*time.Millisecond) // 60.0 s after the first
	if report, skipped := r.Refused("t", 500); !report || skipped != 499 {
		t.Fatalf("refusal a minute on: report=%v skipped=%d, want reported carrying the 499 in between", report, skipped)
	}
	r.Recorded("t", true)
	// Another key runs its own count.
	if report, skipped := r.Refused("u", 0); !report || skipped != 0 {
		t.Fatalf("another key's first refusal: report=%v skipped=%d", report, skipped)
	}
}

// R2(a): an outstanding count is never discarded, however long the key stays
// quiet; it comes out of Due, and only a written closing report forgets it.
func TestReporterNeverDropsAnOutstandingCount(t *testing.T) {
	c := newClock()
	r := NewReporter(time.Minute, c.now)
	r.Refused("t", "first")
	r.Recorded("t", true)
	for i := range 100 {
		c.advance(100 * time.Millisecond)
		r.Refused("t", i)
	}
	c.advance(2 * time.Hour)
	// A refusal for another key, and Due itself, run whatever housekeeping
	// the reporter does; neither may lose t's count.
	r.Refused("other", nil)
	r.Recorded("other", true)
	due := r.Due()
	if len(due) != 1 || due[0].Key != "t" || due[0].Count != 100 || due[0].Sample != 0 {
		t.Fatalf("Due after two quiet hours = %+v, want t's 100 unreported refusals, the first as the sample", due)
	}
	r.Closed("t", true)
	if report, skipped := r.Refused("t", "new run"); !report || skipped != 0 {
		t.Fatalf("after the closing report: report=%v skipped=%d, want a new run reported at once", report, skipped)
	}
}

// R2(b): a run that ends is closed out an interval after its last refusal, not
// at the next refusal; a run with nothing outstanding is simply forgotten,
// and Drain hands back every outstanding count at shutdown, ended or not.
func TestReporterClosesARunThatEnds(t *testing.T) {
	c := newClock()
	r := NewReporter(time.Minute, c.now)
	r.Refused("t", "first")
	r.Recorded("t", true)
	c.advance(time.Second)
	r.Refused("t", "second")
	r.Refused("t", "third")
	r.Refused("quiet", "only")
	r.Recorded("quiet", true)

	c.advance(59 * time.Second) // 59 s after t's last refusal
	if due := r.Due(); len(due) != 0 {
		t.Fatalf("Due before a quiet interval has passed = %+v, want nothing", due)
	}
	c.advance(time.Second)
	due := r.Due()
	if len(due) != 1 || due[0].Key != "t" || due[0].Count != 2 || due[0].Sample != "second" {
		t.Fatalf("Due a quiet interval after t's last refusal = %+v, want its 2 outstanding, from the second", due)
	}
	if r.Len() != 1 {
		t.Fatalf("holds %d keys; the quiet key had nothing outstanding and should be forgotten", r.Len())
	}
	r.Closed("t", true)
	if r.Len() != 0 {
		t.Fatalf("holds %d keys after the closing report", r.Len())
	}

	r.Refused("live", "a")
	r.Recorded("live", true)
	r.Refused("live", "b")
	drained := r.Drain()
	if len(drained) != 1 || drained[0].Count != 1 || drained[0].Sample != "b" {
		t.Fatalf("Drain at shutdown = %+v, want the live run's one outstanding refusal", drained)
	}
}

// R2(c): the run moves on only once its report is written. A failed report
// keeps everything it stood for, the refusal it was for included, and the
// next refusal tries again; so does a failed closing report.
func TestReporterFailedReportKeepsItsCount(t *testing.T) {
	c := newClock()
	r := NewReporter(time.Minute, c.now)
	r.Refused("t", "first")
	r.Recorded("t", true)
	for i := range 10 {
		r.Refused("t", i)
	}
	c.advance(time.Minute)
	report, skipped := r.Refused("t", "eleventh")
	if !report || skipped != 10 {
		t.Fatalf("a minute on: report=%v skipped=%d, want reported carrying 10", report, skipped)
	}
	r.Refused("t", "during the write")
	r.Recorded("t", false) // the audit write failed
	report, skipped = r.Refused("t", "retry")
	if !report || skipped != 12 {
		t.Fatalf("after a failed report: report=%v skipped=%d, want reported again carrying 10 + the failed one + the one during the write", report, skipped)
	}
	r.Recorded("t", true)

	r.Refused("t", "x")
	c.advance(time.Minute)
	if due := r.Due(); len(due) != 1 || due[0].Count != 1 {
		t.Fatalf("Due = %+v, want 1 outstanding", due)
	}
	r.Closed("t", false) // the closing write failed
	due := r.Due()
	if len(due) != 1 || due[0].Count != 1 || due[0].Sample != "x" {
		t.Fatalf("Due after a failed closing report = %+v, want the same 1 outstanding again", due)
	}
}

func TestRefundGivesBackOneTokenUpToTheBurst(t *testing.T) {
	c := newClock()
	l := New(1, 2, c.now)
	l.Allow("k")
	l.Allow("k")
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("admitted past the burst before any refund")
	}
	l.Refund("k")
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("a refunded token was not there to spend")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("one refund bought two requests")
	}
	// Refunds never bank above the burst.
	for range 10 {
		l.Refund("k")
	}
	for i := range 2 {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d after refunds to the burst was refused", i+1)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("ten refunds banked more than the burst of 2")
	}
	// No bucket, no refund, no bucket created.
	l.Refund("never-seen")
	if l.Len() != 1 {
		t.Fatalf("a refund for an unseen key created a bucket: %d buckets", l.Len())
	}
	New(0, 1, c.now).Refund("k") // disabled: a no-op, not a panic
}
