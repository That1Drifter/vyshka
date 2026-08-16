package hub

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// Audit query limits (spec section 10).
const (
	defaultAuditPageSize = 100
	maxAuditPageSize     = 500
)

// DefaultAuditRetention is how long the reference hub keeps audit records. It
// is far longer than event retention because the questions asked of an audit
// log arrive late: a dispute about who changed what surfaces months after the
// change, not minutes after. Retention is hub configuration, not protocol.
const DefaultAuditRetention = 365 * 24 * time.Hour

// auditView is the Admin API representation of one audit record.
type auditView struct {
	ID   string    `json:"id"`
	At   time.Time `json:"at"`
	Name string    `json:"tokenName"`
	// TokenID is empty for the bootstrap credential, which has no token record.
	TokenID       string          `json:"tokenId"`
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Status        int             `json:"status"`
	SourceIP      string          `json:"sourceIp"`
	PayloadDigest string          `json:"payloadDigest"`
	ServerID      string          `json:"serverId,omitempty"`
	Detail        json.RawMessage `json:"detail"`
}

type auditResponse struct {
	Records []auditView `json:"records"`
	// NextCursor is absent on the last page.
	NextCursor string `json:"nextCursor,omitempty"`
}

// handleListAudit answers one page of the audit log (spec section 10). It is
// gated on `admin`: the log names every credential and everything each one
// changed, so a token that could read it could map the whole installation.
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	parameters := r.URL.Query()

	since, ok := parseTimeParam(w, parameters.Get("since"), "since")
	if !ok {
		return
	}
	until, ok := parseTimeParam(w, parameters.Get("until"), "until")
	if !ok {
		return
	}
	limit, ok := parseLimitParam(w, parameters.Get("limit"), defaultAuditPageSize, maxAuditPageSize)
	if !ok {
		return
	}
	after, ok := parseAuditCursor(w, parameters.Get("cursor"))
	if !ok {
		return
	}

	// One row past the page, so the presence of a next page is known without a
	// second query and without ever handing out a cursor to nothing.
	found, err := s.store.AuditRecords(r.Context(), store.AuditQuery{
		TokenID:  parameters.Get("tokenId"),
		ServerID: parameters.Get("serverId"),
		Since:    since,
		Until:    until,
		Limit:    limit + 1,
		After:    after,
	})
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	response := auditResponse{Records: make([]auditView, 0, min(len(found), limit))}
	if len(found) > limit {
		response.NextCursor = encodeAuditCursor(found[limit-1])
		found = found[:limit]
	}
	for _, record := range found {
		detail := record.Detail
		if len(detail) == 0 {
			detail = json.RawMessage(`{}`)
		}
		response.Records = append(response.Records, auditView{
			ID:            record.ID,
			At:            record.At,
			Name:          record.Name,
			TokenID:       record.TokenID,
			Method:        record.Method,
			Path:          record.Path,
			Status:        record.Status,
			SourceIP:      record.SourceIP,
			PayloadDigest: record.PayloadDigest,
			ServerID:      record.ServerID,
			Detail:        detail,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func encodeAuditCursor(record store.AuditRecord) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(envelopeTimestamp(record.At) + "|" + record.ID))
}

func parseAuditCursor(w http.ResponseWriter, value string) (store.AuditCursor, bool) {
	if value == "" {
		return store.AuditCursor{}, true
	}
	refuse := func() (store.AuditCursor, bool) {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"cursor is not one this hub issued")
		return store.AuditCursor{}, false
	}

	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return refuse()
	}
	at, recordID, found := strings.Cut(string(decoded), "|")
	if !found || recordID == "" {
		return refuse()
	}
	parsed, err := time.Parse(cursorLayout, at)
	if err != nil {
		return refuse()
	}
	return store.AuditCursor{At: parsed, ID: recordID}, true
}

// parseTimeParam reads an RFC 3339 query parameter, refusing rather than
// ignoring one it cannot parse: an ignored bound answers with a page that looks
// real and is not.
func parseTimeParam(w http.ResponseWriter, value, name string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, true
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, name+" must be an RFC 3339 timestamp")
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// parseLimitParam reads a page size. An over-large limit is clamped rather than
// refused, because a client asking for more than a page holds wants as much as
// it can get and the cursor gives it the rest.
func parseLimitParam(w http.ResponseWriter, value string, fallback, ceiling int) (int, bool) {
	if value == "" {
		return fallback, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 {
		writeError(w, http.StatusBadRequest, codeBadRequest, "limit must be a positive integer")
		return 0, false
	}
	return min(limit, ceiling), true
}
