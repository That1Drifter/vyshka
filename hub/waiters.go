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
