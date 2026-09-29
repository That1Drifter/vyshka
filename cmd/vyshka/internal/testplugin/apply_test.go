package testplugin

import (
	"context"
	"testing"
)

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
