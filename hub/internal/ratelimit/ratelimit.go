// Package ratelimit is the per-credential request limiter behind spec section
// 2.2.1: one token bucket per authenticated credential, held in memory.
//
// It is in memory on purpose. One hub per database is the supported
// deployment, so there is no second process to share counts with, and a
// limiter that went to the store would put a write on SQLite's single
// connection in front of every request it is meant to protect.
//
// Memory is bounded by what callers may create a bucket for: the hub asks
// only after a credential has authenticated, so the map never holds more
// keys than there are live servers and tokens, and a bucket that has refilled
// to its burst is indistinguishable from a fresh one and is swept away.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// sweepEvery is how often a call may pay for walking the map to drop buckets
// that have refilled. The walk is O(credentials), which is small, but there is
// no reason to do it per request.
const sweepEvery = time.Minute

// Limiter is a set of token buckets keyed by credential. Every bucket refills
// at Rate tokens a second up to Burst; a request spends one. The zero Limiter
// and a nil *Limiter admit everything. It is safe for concurrent use.
type Limiter struct {
	rate  float64
	burst float64
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter admitting rate requests a second per key, with bursts
// of up to burst. A rate of zero or less disables it: every request is
// admitted and nothing is stored. A burst below one is raised to one, since a
// bucket that can never hold a whole token would refuse everything.
func New(rate float64, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return &Limiter{now: now}
	}
	return &Limiter{
		rate:    rate,
		burst:   math.Max(1, float64(burst)),
		now:     now,
		buckets: map[string]*bucket{},
	}
}

// Enabled reports whether the limiter refuses anything at all.
func (l *Limiter) Enabled() bool { return l != nil && l.rate > 0 }

// Allow spends one token from key's bucket. When the bucket holds less than a
// whole token it refuses, spends nothing, and says how long until one will
// have refilled. A refusal costs the caller nothing it could bank, so a key
// sending far over the limit is admitted at exactly the refill rate, never
// locked out for longer than the wait it is told.
func (l *Limiter) Allow(key string) (allowed bool, wait time.Duration) {
	if !l.Enabled() {
		return true, 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = l.levelLocked(b, now)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	missing := (1 - b.tokens) / l.rate
	return false, time.Duration(math.Ceil(missing * float64(time.Second)))
}

// Refund gives back one token Allow spent for key, up to the burst: the
// request it paid for turned out to be one the caller does not charge (spec
// section 2.2.1 exempts a poll the hub ended to deliver work an admin queued).
// A key with no bucket has nothing to refund, since a fresh bucket is full.
func (l *Limiter) Refund(key string) {
	if !l.Enabled() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		b.tokens = math.Min(l.burst, b.tokens+1)
	}
}

// levelLocked is what the bucket holds at now, refilled since its last use.
// A clock that went backwards refills nothing rather than draining.
func (l *Limiter) levelLocked(b *bucket, now time.Time) float64 {
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return b.tokens
	}
	return math.Min(l.burst, b.tokens+elapsed*l.rate)
}

// sweepLocked drops the buckets that have refilled to their burst: such a
// bucket admits exactly what a fresh one would, so keeping it buys nothing.
func (l *Limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for key, b := range l.buckets {
		if l.levelLocked(b, now) >= l.burst {
			delete(l.buckets, key)
		}
	}
}

// Len is how many buckets the limiter holds, for tests and for nothing else.
func (l *Limiter) Len() int {
	if !l.Enabled() {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// RetryAfterSeconds renders a wait for the Retry-After header and
// details.retryAfterSeconds: whole seconds, rounded up, and never below 1
// (spec section 2.2.1), so a client that waits what it is told has a token
// waiting for it.
func RetryAfterSeconds(wait time.Duration) int {
	seconds := int(math.Ceil(wait.Seconds()))
	return max(1, seconds)
}

// Reporter collapses a run of refusals per key into a bounded number of
// records, for the records that must not grow with a flood: the audit log
// (spec section 10.5) and the hub's warning log. Like Limiter it holds a key
// only for a credential that authenticated.
//
// A run begins with a refusal for a key that has no run open, and that
// refusal is reported at once. While refusals continue, the first refusal at
// least one interval after the run's previous report is reported too,
// carrying the count of those left unreported in between. A run ends when an
// interval passes with no refusal; Due then hands back its outstanding count
// for a closing report, so the count is complete without waiting for a next
// refusal that may never come.
//
// Counts are exact. A key is forgotten only once everything counted against it
// has been reported, and a report the caller failed to write gives its count
// back (Recorded with ok false), so a refused write is never lost because the
// log it goes to was briefly unavailable.
type Reporter struct {
	every time.Duration
	now   func() time.Time

	mu   sync.Mutex
	runs map[string]*run
}

type run struct {
	last     time.Time // the latest refusal
	reported time.Time // the run's latest report; zero before its first
	// unreported counts refusals since the latest report with no report of
	// their own, and sample is the first of them.
	unreported int
	sample     any
	// writing is set while a report the Reporter handed out is being written;
	// inflight and inflightSample are what that report stands for, given back
	// if it fails, and pending is the sample of the refusal it was for.
	writing        bool
	inflight       int
	inflightSample any
	pending        any
}

// Closing is one run's outstanding count, due for a closing report: Count
// refusals, the first of which was Sample.
type Closing struct {
	Key    string
	Count  int
	Sample any
}

// NewReporter returns a reporter whose runs report once per interval and end
// after an interval without a refusal.
func NewReporter(every time.Duration, now func() time.Time) *Reporter {
	if now == nil {
		now = time.Now
	}
	return &Reporter{every: every, now: now, runs: map[string]*run{}}
}

// Refused notes one refusal for key; sample describes it. When report is true
// the caller writes a report for this refusal carrying skipped, the count of
// the key's earlier refusals it stands for, and then calls Recorded with the
// outcome. When report is false the refusal is counted for a later report.
func (r *Reporter) Refused(key string, sample any) (report bool, skipped int) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	current, ok := r.runs[key]
	if !ok {
		current = &run{}
		r.runs[key] = current
	}
	current.last = now
	if !current.writing && (current.reported.IsZero() || now.Sub(current.reported) >= r.every) {
		current.writing = true
		current.inflight, current.inflightSample = current.unreported, current.sample
		current.unreported, current.sample = 0, nil
		current.pending = sample
		return true, current.inflight
	}
	if current.unreported == 0 {
		current.sample = sample
	}
	current.unreported++
	return false, 0
}

// Recorded settles a report Refused handed out. ok says it was written: only
// then does the run move on. A failed report gives back the count it stood
// for, with the refusal it was for added, so the next refusal tries again.
func (r *Reporter) Recorded(key string, ok bool) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	current, found := r.runs[key]
	if !found || !current.writing {
		return
	}
	current.writing = false
	if ok {
		current.reported = now
		current.inflight, current.inflightSample, current.pending = 0, nil, nil
		return
	}
	r.giveBackLocked(current, current.inflight+1, firstOf(current.inflight, current.inflightSample, current.pending))
}

// Due hands back the runs that have ended, an interval after their last
// refusal, and that still have refusals no report counted; their keys stay
// held until Closed confirms the closing report was written. A run that ended
// with nothing outstanding is forgotten here.
func (r *Reporter) Due() []Closing {
	return r.collect(false)
}

// Drain is Due for a hub shutting down: every run with an outstanding count,
// ended or not.
func (r *Reporter) Drain() []Closing {
	return r.collect(true)
}

func (r *Reporter) collect(all bool) []Closing {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var due []Closing
	for key, current := range r.runs {
		if current.writing || (!all && now.Sub(current.last) < r.every) {
			continue
		}
		if current.unreported == 0 {
			if !all {
				delete(r.runs, key)
			}
			continue
		}
		current.writing = true
		current.inflight, current.inflightSample = current.unreported, current.sample
		current.unreported, current.sample, current.pending = 0, nil, nil
		due = append(due, Closing{Key: key, Count: current.inflight, Sample: current.inflightSample})
	}
	return due
}

// Closed settles a closing report Due or Drain handed out. Written, the run is
// over and the key forgotten, unless a refusal arrived meanwhile: that one
// begins the next run and is counted toward its first report. Not written,
// the count goes back and the next Due tries again.
func (r *Reporter) Closed(key string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, found := r.runs[key]
	if !found || !current.writing {
		return
	}
	current.writing = false
	if !ok {
		r.giveBackLocked(current, current.inflight, current.inflightSample)
		return
	}
	current.inflight, current.inflightSample = 0, nil
	if current.unreported == 0 {
		delete(r.runs, key)
		return
	}
	current.reported = time.Time{}
}

// giveBackLocked returns count refusals, the first of them first, to the
// front of the run's unreported count.
func (r *Reporter) giveBackLocked(current *run, count int, first any) {
	if count > 0 {
		if first != nil {
			current.sample = first
		}
		current.unreported += count
	}
	current.inflight, current.inflightSample, current.pending = 0, nil, nil
}

func firstOf(inflight int, inflightSample, pending any) any {
	if inflight > 0 {
		return inflightSample
	}
	return pending
}

// Len is how many keys the reporter holds, for tests.
func (r *Reporter) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}
