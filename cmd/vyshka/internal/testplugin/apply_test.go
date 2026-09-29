package testplugin

import (
	"context"
	"encoding/json"
	"testing"
)

// A poll's batch is framed within the per-poll event budget: six
// event.batch envelopes of 200 events go out five at a time with more set,
// five exactly fill it, and an envelope alone always goes.
func TestBatchStaysWithinTheEventBudget(t *testing.T) {
	queued := func(batches int) *Plugin {
		p := &Plugin{}
		p.ctx, p.cancel = context.WithCancel(context.Background())
		for range batches {
			p.enqueueLocked("event.batch", json.RawMessage(`{}`), maxEventsPerBatch)
		}
		return p
	}
	for _, tc := range []struct {
		batches, want int
		more          bool
	}{
		{6, 5, true},
		{5, 5, false},
		{1, 1, false},
		{12, 5, true},
	} {
		p := queued(tc.batches)
		request, _, cancel := p.beginPoll()
		cancel()
		if len(request.Envelopes) != tc.want || request.More != tc.more {
			t.Errorf("%d batches queued: the poll carries %d with more=%v, want %d with more=%v",
				tc.batches, len(request.Envelopes), request.More, tc.want, tc.more)
		}
		if p.outSent != int64(tc.want) {
			t.Errorf("%d batches queued: outSent = %d, want %d", tc.batches, p.outSent, tc.want)
		}
	}
}

// An ack above the highest seq ever sent is a hub fault, not a reason to
// drop envelopes that never travelled: it is recorded as fatal and the
// outbox is left as it was.
func TestAckAboveWhatWasSentIsAFault(t *testing.T) {
	queued := func() *Plugin {
		p := &Plugin{outSeq: 201, outSent: 200}
		p.ctx, p.cancel = context.WithCancel(context.Background())
		for seq := int64(1); seq <= 201; seq++ {
			p.outbound = append(p.outbound, envelope{Seq: seq})
		}
		return p
	}

	faulty := queued()
	faulty.apply(pollResponse{Ack: 201})
	if faulty.err == nil {
		t.Fatal("an ack above what was sent was accepted")
	}
	if len(faulty.outbound) != 201 {
		t.Errorf("%d envelopes were dropped on a faulty ack", 201-len(faulty.outbound))
	}
	if faulty.ctx.Err() == nil {
		t.Error("the loop was not stopped on a faulty ack")
	}

	// An ack within what was sent frees exactly the envelopes it covers.
	sound := queued()
	sound.apply(pollResponse{Ack: 200})
	if sound.err != nil {
		t.Fatalf("a sound ack was refused: %v", sound.err)
	}
	if len(sound.outbound) != 1 || sound.outbound[0].Seq != 201 {
		t.Errorf("after ack 200 the outbox holds %d envelopes, want the one unsent seq 201", len(sound.outbound))
	}
}
