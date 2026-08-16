package hub

import "sync"

// waiters wakes held long-polls. Without it a poll would only ever learn about
// new work when its backstop tick came round, and "queued now, delivered within
// a second" is the whole point of holding the request open (spec section 3.1.2).
//
// It is in-process on purpose. The backstop tick in the poll loop is what makes
// a missed wake-up cost latency rather than correctness, which is also what
// would let a multi-process hub work without a message bus.
type waiters struct {
	mu      sync.Mutex
	nextID  uint64
	waiting map[string]map[uint64]chan struct{}
}

func newWaiters() *waiters {
	return &waiters{waiting: map[string]map[uint64]chan struct{}{}}
}

// wait registers interest in a key and returns the channel to select on plus
// the function that unregisters it. Callers must register before they check for
// work, or a notification landing between the check and the select is lost.
func (w *waiters) wait(key string) (<-chan struct{}, func()) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.nextID++
	id := w.nextID
	// Buffered, so notify never blocks on a waiter that has not reached its
	// select yet and never has to care whether one is listening.
	signal := make(chan struct{}, 1)
	if w.waiting[key] == nil {
		w.waiting[key] = map[uint64]chan struct{}{}
	}
	w.waiting[key][id] = signal

	return signal, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		waiting := w.waiting[key]
		delete(waiting, id)
		if len(waiting) == 0 {
			delete(w.waiting, key)
		}
	}
}

// notify wakes everything waiting on a key.
func (w *waiters) notify(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, signal := range w.waiting[key] {
		select {
		case signal <- struct{}{}:
		default: // already signalled and not yet drained; one wake-up is enough
		}
	}
}

// holds counts the polls currently being held per session, so that one
// credential cannot park an unbounded number of them.
//
// A conforming plugin keeps exactly one poll in flight (spec section 3.1), and
// a retry around a flaky link can briefly make that two. Past a small ceiling
// the extra requests are not a plugin doing its job, and each one costs a
// goroutine, a timer, and a database read every backstop tick. Answering them
// immediately instead of holding is always protocol-legal: the hub owes a
// response no later than the poll timeout, never no sooner.
type holds struct {
	mu   sync.Mutex
	held map[string]int
}

func newHolds() *holds { return &holds{held: map[string]int{}} }

// enter claims a hold slot for a session. It always returns a release function,
// including when the claim was refused, so callers can defer it unconditionally.
func (h *holds) enter(key string, limit int) (granted bool, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.held[key] >= limit {
		return false, func() {}
	}
	h.held[key]++

	var once sync.Once
	return true, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.held[key]--
			if h.held[key] <= 0 {
				delete(h.held, key)
			}
		})
	}
}
