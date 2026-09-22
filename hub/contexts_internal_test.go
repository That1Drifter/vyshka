package hub

import (
	"errors"
	"testing"
	"time"
)

// The registry's bookkeeping, at the package level: what happens to a
// question nobody answered when time moves on. The handler tests cover the
// wire; these cover the paths a reader could be stranded on.

func TestEnumerationsReplacingAStaleQuestionCompletesIt(t *testing.T) {
	registry := newEnumerations(10*time.Second, time.Second)
	key := enumerationKey{serverID: "s", contextID: "c", revision: 1}
	now := time.Now()

	first, started := registry.join(key, now)
	if !started {
		t.Fatal("the first join did not start a question")
	}
	// Past the first question's deadline, a new read replaces it. The
	// reader still waiting on the first must be answered, not left on a
	// channel nothing will close.
	second, started := registry.join(key, now.Add(2*time.Second))
	if !started || second == first {
		t.Fatal("the second join did not replace the stale question")
	}
	select {
	case <-first.done:
	default:
		t.Fatal("the replaced question was not completed; its reader would block forever")
	}
	if !errors.Is(first.err, errEnumerationTimeout) {
		t.Fatalf("the replaced question's err = %v, want the timeout", first.err)
	}
	// The stale reader's own expiry, arriving late, is a no-op rather than
	// a second close.
	registry.expire(first)
	if registry.pending[first.requestID] != nil || registry.inflight[key] != second {
		t.Fatal("the registry does not hold exactly the replacement")
	}
}

func TestEnumerationsSweepAbandonedQuestions(t *testing.T) {
	registry := newEnumerations(10*time.Second, time.Second)
	now := time.Now()
	abandoned, _ := registry.join(enumerationKey{serverID: "s", contextID: "old", revision: 1}, now)

	// A read of another key, past the abandoned question's deadline,
	// completes and forgets it: a reader that left before its deadline
	// must not pin the question for the life of the hub.
	registry.join(enumerationKey{serverID: "s", contextID: "new", revision: 2}, now.Add(2*time.Second))
	select {
	case <-abandoned.done:
	default:
		t.Fatal("the abandoned question was not swept on a later join")
	}
	if registry.pending[abandoned.requestID] != nil {
		t.Fatal("the abandoned question is still registered")
	}
	if len(registry.inflight) != 1 {
		t.Fatalf("inflight holds %d questions, want the new one alone", len(registry.inflight))
	}

	// A reply landing also sweeps.
	stale, _ := registry.join(enumerationKey{serverID: "s", contextID: "stale", revision: 1}, now.Add(3*time.Second))
	registry.deliver("s", preparedContextEntries{requestID: "nobody"}, now.Add(6*time.Second))
	select {
	case <-stale.done:
	default:
		t.Fatal("the stale question was not swept on a delivery")
	}
}
