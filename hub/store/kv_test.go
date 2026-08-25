package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

func TestKVSetGetDelete(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	entry, err := st.KVSet(ctx, "example-mod", "greeting", []byte(`{"hello":"world"}`), nil, nil)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if entry.Revision != 1 {
		t.Errorf("first set revision = %d, want 1", entry.Revision)
	}
	if entry.ExpiresAt != nil {
		t.Errorf("no-TTL set reported expiry %v", entry.ExpiresAt)
	}

	read, err := st.KVGet(ctx, "example-mod", "greeting")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(read.Value) != `{"hello":"world"}` {
		t.Errorf("value = %s, want the stored object verbatim", read.Value)
	}
	if read.Revision != 1 {
		t.Errorf("get revision = %d, want 1", read.Revision)
	}

	// A second unconditional set advances the revision.
	entry, err = st.KVSet(ctx, "example-mod", "greeting", []byte(`"replaced"`), nil, nil)
	if err != nil {
		t.Fatalf("second set: %v", err)
	}
	if entry.Revision != 2 {
		t.Errorf("second set revision = %d, want 2", entry.Revision)
	}

	if err := st.KVDelete(ctx, "example-mod", "greeting"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.KVGet(ctx, "example-mod", "greeting"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
	if err := st.KVDelete(ctx, "example-mod", "greeting"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}

	// A recreated key starts over at revision 1 (spec section 12.1).
	entry, err = st.KVSet(ctx, "example-mod", "greeting", []byte(`1`), nil, nil)
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if entry.Revision != 1 {
		t.Errorf("recreated revision = %d, want 1", entry.Revision)
	}
}

func TestKVNamespacesAreDisjoint(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	if _, err := st.KVSet(ctx, "mod-a", "shared-name", []byte(`"a"`), nil, nil); err != nil {
		t.Fatalf("set mod-a: %v", err)
	}
	if _, err := st.KVGet(ctx, "mod-b", "shared-name"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("mod-b read mod-a's key: err = %v, want ErrNotFound", err)
	}
}

func TestKVCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	rev := func(n int64) *int64 { return &n }

	// ifRevision 0 creates only when the key does not exist.
	if _, err := st.KVSet(ctx, "example-mod", "cas", []byte(`1`), rev(0), nil); err != nil {
		t.Fatalf("create-only set: %v", err)
	}
	var mismatch *store.KVRevisionMismatchError
	if _, err := st.KVSet(ctx, "example-mod", "cas", []byte(`2`), rev(0), nil); !errors.As(err, &mismatch) {
		t.Fatalf("create-only set over an existing key = %v, want a revision mismatch", err)
	}
	if mismatch.Current != 1 {
		t.Errorf("mismatch reported current %d, want 1", mismatch.Current)
	}

	// A stale revision loses and reports the current one.
	if _, err := st.KVSet(ctx, "example-mod", "cas", []byte(`2`), rev(5), nil); !errors.As(err, &mismatch) {
		t.Fatalf("stale CAS = %v, want a revision mismatch", err)
	}
	if mismatch.Current != 1 {
		t.Errorf("stale CAS reported current %d, want 1", mismatch.Current)
	}

	// The fresh revision wins.
	entry, err := st.KVSet(ctx, "example-mod", "cas", []byte(`2`), rev(1), nil)
	if err != nil {
		t.Fatalf("fresh CAS: %v", err)
	}
	if entry.Revision != 2 {
		t.Errorf("fresh CAS revision = %d, want 2", entry.Revision)
	}

	// A CAS against a missing key reports current 0.
	if _, err := st.KVSet(ctx, "example-mod", "missing", []byte(`1`), rev(3), nil); !errors.As(err, &mismatch) {
		t.Fatalf("CAS on a missing key = %v, want a revision mismatch", err)
	}
	if mismatch.Current != 0 {
		t.Errorf("missing-key CAS reported current %d, want 0", mismatch.Current)
	}
}

func TestKVIncr(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	// Incr on a missing key creates it at delta.
	entry, err := st.KVIncr(ctx, "example-mod", "counter", 5)
	if err != nil {
		t.Fatalf("first incr: %v", err)
	}
	if string(entry.Value) != "5" || entry.Revision != 1 {
		t.Errorf("first incr = (%s, rev %d), want (5, rev 1)", entry.Value, entry.Revision)
	}

	// A negative delta is the decrement.
	entry, err = st.KVIncr(ctx, "example-mod", "counter", -2)
	if err != nil {
		t.Fatalf("decrement: %v", err)
	}
	if string(entry.Value) != "3" || entry.Revision != 2 {
		t.Errorf("decrement = (%s, rev %d), want (3, rev 2)", entry.Value, entry.Revision)
	}

	// Incr on a non-integer value refuses and changes nothing.
	if _, err := st.KVSet(ctx, "example-mod", "not-a-number", []byte(`"three"`), nil, nil); err != nil {
		t.Fatalf("set string: %v", err)
	}
	if _, err := st.KVIncr(ctx, "example-mod", "not-a-number", 1); !errors.Is(err, store.ErrKVNotInteger) {
		t.Errorf("incr on a string = %v, want ErrKVNotInteger", err)
	}
	read, err := st.KVGet(ctx, "example-mod", "not-a-number")
	if err != nil || string(read.Value) != `"three"` || read.Revision != 1 {
		t.Errorf("refused incr changed the key: (%s, rev %d, err %v)", read.Value, read.Revision, err)
	}

	// A sum that would leave the exactness bound refuses and changes nothing.
	if _, err := st.KVSet(ctx, "example-mod", "near-bound", []byte("9007199254740991"), nil, nil); err != nil {
		t.Fatalf("set near bound: %v", err)
	}
	if _, err := st.KVIncr(ctx, "example-mod", "near-bound", 1); !errors.Is(err, store.ErrKVRangeExceeded) {
		t.Errorf("incr past the bound = %v, want ErrKVRangeExceeded", err)
	}
	// A stored value already outside the bound is not exact arithmetic either.
	if _, err := st.KVSet(ctx, "example-mod", "past-bound", []byte("9007199254740992"), nil, nil); err != nil {
		t.Fatalf("set past bound: %v", err)
	}
	if _, err := st.KVIncr(ctx, "example-mod", "past-bound", -1); !errors.Is(err, store.ErrKVNotInteger) {
		t.Errorf("incr on a value past the bound = %v, want ErrKVNotInteger", err)
	}
}

func TestKVConcurrentIncrsAllLand(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	const workers = 8
	const perWorker = 25

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				if _, err := st.KVIncr(ctx, "example-mod", "hits", 1); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent incr: %v", err)
	}

	entry, err := st.KVGet(ctx, "example-mod", "hits")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(entry.Value) != "200" {
		t.Errorf("value = %s, want 200: a delta vanished", entry.Value)
	}
	if entry.Revision != workers*perWorker {
		t.Errorf("revision = %d, want %d", entry.Revision, workers*perWorker)
	}
}

func TestKVTTL(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	ttl := 50 * time.Millisecond
	entry, err := st.KVSet(ctx, "example-mod", "ephemeral", []byte(`1`), nil, &ttl)
	if err != nil {
		t.Fatalf("set with TTL: %v", err)
	}
	if entry.ExpiresAt == nil {
		t.Fatal("set with TTL reported no expiry")
	}

	if _, err := st.KVGet(ctx, "example-mod", "ephemeral"); err != nil {
		t.Fatalf("get before expiry: %v", err)
	}

	time.Sleep(ttl + 30*time.Millisecond)

	// Expired reads as absent everywhere before any prune runs.
	if _, err := st.KVGet(ctx, "example-mod", "ephemeral"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after expiry = %v, want ErrNotFound", err)
	}
	if err := st.KVDelete(ctx, "example-mod", "ephemeral"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete after expiry = %v, want ErrNotFound", err)
	}

	// A CAS sees the expired key as revision 0, and recreating starts over.
	zero := int64(0)
	recreated, err := st.KVSet(ctx, "example-mod", "ephemeral", []byte(`2`), &zero, nil)
	if err != nil {
		t.Fatalf("recreate over an expired key: %v", err)
	}
	if recreated.Revision != 1 {
		t.Errorf("recreated revision = %d, want 1", recreated.Revision)
	}
	if recreated.ExpiresAt != nil {
		t.Errorf("a set with no TTL kept the old expiry %v", recreated.ExpiresAt)
	}

	// An incr on an expired key also starts fresh, with no TTL.
	expiring := 50 * time.Millisecond
	if _, err := st.KVSet(ctx, "example-mod", "expiring-counter", []byte(`10`), nil, &expiring); err != nil {
		t.Fatalf("set expiring counter: %v", err)
	}
	time.Sleep(expiring + 30*time.Millisecond)
	fresh, err := st.KVIncr(ctx, "example-mod", "expiring-counter", 1)
	if err != nil {
		t.Fatalf("incr over an expired key: %v", err)
	}
	if string(fresh.Value) != "1" || fresh.Revision != 1 || fresh.ExpiresAt != nil {
		t.Errorf("incr over an expired key = (%s, rev %d, expiry %v), want (1, rev 1, none)",
			fresh.Value, fresh.Revision, fresh.ExpiresAt)
	}
}

func TestKVIncrPreservesTTLAndSetReplacesIt(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	ttl := time.Hour
	if _, err := st.KVSet(ctx, "example-mod", "counter", []byte(`1`), nil, &ttl); err != nil {
		t.Fatalf("set with TTL: %v", err)
	}

	bumped, err := st.KVIncr(ctx, "example-mod", "counter", 1)
	if err != nil {
		t.Fatalf("incr: %v", err)
	}
	if bumped.ExpiresAt == nil {
		t.Error("incr dropped the key's TTL")
	}

	// A set with no TTL clears it: a set defines the key entirely.
	cleared, err := st.KVSet(ctx, "example-mod", "counter", []byte(`5`), nil, nil)
	if err != nil {
		t.Fatalf("set without TTL: %v", err)
	}
	if cleared.ExpiresAt != nil {
		t.Errorf("a set with no TTL kept the expiry %v", cleared.ExpiresAt)
	}
	read, err := st.KVGet(ctx, "example-mod", "counter")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.ExpiresAt != nil {
		t.Errorf("stored key still carries expiry %v after a no-TTL set", read.ExpiresAt)
	}
}

func TestKVPrune(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	short := 10 * time.Millisecond
	if _, err := st.KVSet(ctx, "example-mod", "doomed", []byte(`1`), nil, &short); err != nil {
		t.Fatalf("set doomed: %v", err)
	}
	if _, err := st.KVSet(ctx, "example-mod", "kept", []byte(`1`), nil, nil); err != nil {
		t.Fatalf("set kept: %v", err)
	}

	time.Sleep(short + 30*time.Millisecond)

	pruned, err := st.PruneKV(ctx, 100)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d rows, want 1", pruned)
	}
	if _, err := st.KVGet(ctx, "example-mod", "kept"); err != nil {
		t.Errorf("prune removed a key with no TTL: %v", err)
	}
}
