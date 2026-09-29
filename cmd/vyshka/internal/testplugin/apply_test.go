package testplugin

import (
	"context"
	"encoding/json"
	"strings"
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

// A poll's batch also stays under the hub's request body cap: three bodies
// of 400 KiB go out one per poll, and an Emit whose events are large closes
// its batches early so that every envelope fits.
func TestBatchStaysUnderThePollBodyCap(t *testing.T) {
	p := &Plugin{}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	big := json.RawMessage(`{"events":[{"t":"x","data":{"s":"` + strings.Repeat("a", 400<<10) + `"}}]}`)
	for range 3 {
		p.enqueueLocked("event.batch", big, 1)
	}
	request, _, cancel := p.beginPoll()
	cancel()
	if len(request.Envelopes) != 1 || !request.More {
		t.Errorf("three 400 KiB bodies queued: the poll carries %d with more=%v, want 1 with more=true",
			len(request.Envelopes), request.More)
	}

	q := &Plugin{opts: Options{Logf: func(string, ...any) {}}}
	q.ctx, q.cancel = context.WithCancel(context.Background())
	events := make([]Event, 600)
	for i := range events {
		events[i] = Event{Type: "cli-test.beat", Data: map[string]any{"s": strings.Repeat("b", 2048)}}
	}
	q.Emit(events...)
	if len(q.outbound) < 5 {
		t.Errorf("600 events of 2 KiB went into %d batches; want them closed by size, several", len(q.outbound))
	}
	total := 0
	for _, pending := range q.outbound {
		if len(pending.Body) > maxBatchBytes+4096 {
			t.Errorf("a batch body of %d bytes passed the %d cap", len(pending.Body), maxBatchBytes)
		}
		total += pending.events
	}
	if total != 600 {
		t.Errorf("the batches carry %d events, want 600", total)
	}
	request, _, cancel = q.beginPoll()
	cancel()
	if len(request.Envelopes) == len(q.outbound) {
		t.Errorf("all %d batches (over 1 MiB together) were framed into one poll", len(q.outbound))
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
