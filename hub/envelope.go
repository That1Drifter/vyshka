package hub

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// Envelope limits. The inbound cap is above the 200 a hub must accept (spec
// section 3.1.2) so that a plugin flushing a full event batch alongside other
// traffic never trips it; the outbound cap keeps one poll response a size a
// constrained engine can parse in one go.
const (
	maxInboundEnvelopes = 500
	maxOutboundBatch    = 200
	maxEnvelopeType     = 128
	maxEnvelopeID       = 128
)

// envelope is the wire form of spec section 4, used in both directions.
//
// `ts` is carried as a string rather than a time.Time so that a clock a game
// engine got wrong costs one ignored field instead of a rejected poll: the hub
// stamps its own receipt time when the field is missing or unparseable.
type envelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

// envelopeTimestamp renders an envelope `ts`: RFC 3339, UTC, fixed width.
func envelopeTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// newOutboundEnvelope renders a queued envelope for the wire. The id, body and
// timestamp are whatever was stored, unchanged on every retransmission, because
// that is what lets the plugin deduplicate (spec section 9.1).
func newOutboundEnvelope(queued store.OutboundEnvelope) envelope {
	body := queued.Body
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	return envelope{
		V:    EnvelopeVersion,
		ID:   queued.ID,
		Type: queued.Type,
		Seq:  queued.Seq,
		TS:   envelopeTimestamp(queued.CreatedAt),
		Body: body,
	}
}

// envelopeFault names why one inbound envelope was refused, so the error body
// can point at it rather than at the batch (spec section 3.1.2).
type envelopeFault struct {
	Index   int
	Seq     int64
	Message string
}

func (f envelopeFault) details() map[string]any {
	details := map[string]any{"index": f.Index}
	if f.Seq > 0 {
		details["seq"] = f.Seq
	}
	return details
}

// validateInbound checks the envelope framing a hub must be able to rely on.
// Anything beyond framing is the business of whatever handles the type, and
// unknown types are not an error at all: they are acked and ignored.
func validateInbound(index int, e envelope) *envelopeFault {
	fault := func(format string, args ...any) *envelopeFault {
		return &envelopeFault{Index: index, Seq: e.Seq, Message: fmt.Sprintf(format, args...)}
	}

	switch {
	case e.V != 0 && e.V != EnvelopeVersion:
		return fault("envelope version %d is not supported; this hub speaks version %d",
			e.V, EnvelopeVersion)
	case e.ID == "":
		return fault("id is required")
	case len(e.ID) > maxEnvelopeID:
		return fault("id is longer than %d characters", maxEnvelopeID)
	case e.Type == "":
		return fault("type is required")
	case len(e.Type) > maxEnvelopeType:
		return fault("type is longer than %d characters", maxEnvelopeType)
	case e.Seq < 1:
		return fault("seq must be 1 or above, got %d", e.Seq)
	}
	return nil
}

// inboundBatch is what one poll's envelopes did to the session's inbound ack.
type inboundBatch struct {
	// Ack is the highest contiguous seq after applying the batch.
	Ack      int64
	Accepted []envelope
	// Duplicate counts envelopes at or below the ack: already processed, acked
	// again, never an error. Gapped counts envelopes above a gap, which are
	// discarded and will come back on retransmission.
	Duplicate int
	Gapped    int
}

// classifyInbound applies a batch to a contiguous ack (spec section 9.1). It is
// deliberately a pure function of the ack and the batch: every ordering rule the
// protocol has in the plugin -> hub direction lives here and nowhere else.
func classifyInbound(ack int64, envelopes []envelope) inboundBatch {
	batch := inboundBatch{Ack: ack}
	for _, e := range envelopes {
		switch {
		case e.Seq <= batch.Ack:
			batch.Duplicate++
		case e.Seq == batch.Ack+1:
			batch.Ack++
			batch.Accepted = append(batch.Accepted, e)
		default:
			// A gap. The ack must not move past it, and holding the envelope
			// would only duplicate a buffer the sender already keeps.
			batch.Gapped++
		}
	}
	return batch
}
