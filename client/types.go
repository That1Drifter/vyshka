package client

import (
	"encoding/json"
	"time"
)

// Every record below with a Raw field fills it through an UnmarshalJSON
// method of the same three lines: decode through a method-less alias of the
// type, so the decoder does not recurse, then keep the bytes. Raw is the
// hub's own JSON for that object, members this client does not model
// included, which is what lets a caller print the hub's object rather than
// this client's reading of it.

// Health is the body of GET /healthz.
type Health struct {
	// Status is "ok" or "degraded".
	Status        string          `json:"status"`
	Version       string          `json:"version"`
	UptimeSeconds int64           `json:"uptimeSeconds"`
	Database      HealthDatabase  `json:"database"`
	Raw           json.RawMessage `json:"-"`
}

// HealthDatabase reports whether the hub's store is answering.
type HealthDatabase struct {
	Driver        string `json:"driver"`
	OK            bool   `json:"ok"`
	SchemaVersion int    `json:"schemaVersion"`
	Error         string `json:"error,omitempty"`
}

func (h *Health) UnmarshalJSON(data []byte) error {
	type plain Health
	return jsonUnmarshalRaw(data, (*plain)(h), &h.Raw)
}

// Server is a game server record (spec section 5.1). It never carries a
// credential.
type Server struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Game       string     `json:"game"`
	CreatedAt  time.Time  `json:"createdAt"`
	EnrolledAt *time.Time `json:"enrolledAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	// CredentialState is none, active, or revoked in this draft.
	CredentialState string     `json:"credentialState"`
	LastSeenAt      *time.Time `json:"lastSeenAt"`
	// LinkState is unknown, up, or down in this draft (spec section 11.1).
	LinkState            string            `json:"linkState"`
	PendingEnvelopeCount int               `json:"pendingEnvelopeCount"`
	Plugin               *PluginDescriptor `json:"plugin"`
	Session              *SessionSummary   `json:"session"`
	// Bans is nil from a hub predating the installation ban list.
	Bans *ServerBans     `json:"bans"`
	Raw  json.RawMessage `json:"-"`
}

func (s *Server) UnmarshalJSON(data []byte) error {
	type plain Server
	return jsonUnmarshalRaw(data, (*plain)(s), &s.Raw)
}

// ServerList is the answer to a server listing, newest first. Raw is the
// whole answer as the hub sent it.
type ServerList struct {
	Servers []Server        `json:"servers"`
	Raw     json.RawMessage `json:"-"`
}

func (l *ServerList) UnmarshalJSON(data []byte) error {
	type plain ServerList
	return jsonUnmarshalRaw(data, (*plain)(l), &l.Raw)
}

// PluginDescriptor is what a plugin last said about itself. Advisory.
type PluginDescriptor struct {
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Transports []string `json:"transports"`
}

// SessionSummary is a server's live session.
type SessionSummary struct {
	ID                 string    `json:"id"`
	ExpiresAt          time.Time `json:"expiresAt"`
	PollTimeoutSeconds int       `json:"pollTimeoutSeconds"`
}

// ServerBans is a server's side of the installation ban list (spec section
// 13.4).
type ServerBans struct {
	Supported       bool       `json:"supported"`
	AppliedRevision *int64     `json:"appliedRevision"`
	AppliedAt       *time.Time `json:"appliedAt"`
}

// CreateServerRequest is the body of a server creation. Game and the TTL are
// optional; zero values are left out and the hub's defaults apply.
type CreateServerRequest struct {
	Name                      string `json:"name"`
	Game                      string `json:"game,omitempty"`
	EnrollmentTokenTTLSeconds int    `json:"enrollmentTokenTtlSeconds,omitempty"`
}

// EnrollmentToken is a one-time enrollment credential. The hub keeps only a
// digest, so this is the only time its value is ever available.
type EnrollmentToken struct {
	Token     string          `json:"token"`
	ExpiresAt time.Time       `json:"expiresAt"`
	Raw       json.RawMessage `json:"-"`
}

func (e *EnrollmentToken) UnmarshalJSON(data []byte) error {
	type plain EnrollmentToken
	return jsonUnmarshalRaw(data, (*plain)(e), &e.Raw)
}

// CreatedServer is the answer to a server creation.
type CreatedServer struct {
	Server     Server          `json:"server"`
	Enrollment EnrollmentToken `json:"enrollment"`
	Raw        json.RawMessage `json:"-"`
}

func (c *CreatedServer) UnmarshalJSON(data []byte) error {
	type plain CreatedServer
	return jsonUnmarshalRaw(data, (*plain)(c), &c.Raw)
}

// Identity is a platform-qualified player identity (spec section 8.2).
type Identity struct {
	Platform string `json:"platform"`
	ID       string `json:"id"`
}

// ManifestRecord is a server's stored manifest (spec section 6.5). Raw is the
// whole record as the hub sent it; Manifest.Raw is the manifest body alone.
type ManifestRecord struct {
	Revision    int64           `json:"revision"`
	PublishedAt time.Time       `json:"publishedAt"`
	Manifest    Manifest        `json:"manifest"`
	Raw         json.RawMessage `json:"-"`
}

func (m *ManifestRecord) UnmarshalJSON(data []byte) error {
	type plain ManifestRecord
	return jsonUnmarshalRaw(data, (*plain)(m), &m.Raw)
}

// Manifest is an accepted manifest.publish body (spec section 6).
type Manifest struct {
	Game             string            `json:"game"`
	Plugin           *PluginDescriptor `json:"plugin"`
	ManifestRevision int64             `json:"manifestRevision"`
	Actions          []ManifestAction  `json:"actions"`
	Contexts         []ManifestContext `json:"contexts"`
	Events           []ManifestEvent   `json:"events"`
	KVNamespaces     []string          `json:"kvNamespaces"`
	Capabilities     []string          `json:"capabilities"`
	Raw              json.RawMessage   `json:"-"`
}

func (m *Manifest) UnmarshalJSON(data []byte) error {
	type plain Manifest
	return jsonUnmarshalRaw(data, (*plain)(m), &m.Raw)
}

// ManifestAction is one declared action (spec section 6.1).
type ManifestAction struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	// Danger is none, warning, or destructive in this draft; advisory, for
	// confirmation prompts. Empty means none.
	Danger string          `json:"danger"`
	Params *ParamsSchema   `json:"params"`
	Raw    json.RawMessage `json:"-"`
}

func (a *ManifestAction) UnmarshalJSON(data []byte) error {
	type plain ManifestAction
	return jsonUnmarshalRaw(data, (*plain)(a), &a.Raw)
}

// ManifestContext is one declared custom context (spec section 6.2).
type ManifestContext struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// ManifestEvent is one declared custom event type (spec section 6.3).
type ManifestEvent struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Namespace string        `json:"namespace"`
	Payload   *ParamsSchema `json:"payload"`
}

// ParamsSchema is the JSON Schema subset of spec section 6.1. Numbers in Enum,
// Not, and Default decode as float64, the way the hub compares them.
type ParamsSchema struct {
	Type             string                   `json:"type,omitempty"`
	Enum             []any                    `json:"enum,omitempty"`
	Not              *NotSchema               `json:"not,omitempty"`
	Required         []string                 `json:"required,omitempty"`
	Properties       map[string]*ParamsSchema `json:"properties,omitempty"`
	Items            *ParamsSchema            `json:"items,omitempty"`
	Minimum          *float64                 `json:"minimum,omitempty"`
	Maximum          *float64                 `json:"maximum,omitempty"`
	ExclusiveMinimum *float64                 `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum *float64                 `json:"exclusiveMaximum,omitempty"`
	// Default is an annotation: surfaced to UIs, never enforced.
	Default any `json:"default,omitempty"`
	// Context is a string or a list of strings naming the custom contexts
	// whose entries a UI offers as values. An annotation, never enforced.
	Context any `json:"context,omitempty"`
	// KVNamespace names the namespace whose keys a UI offers as values. An
	// annotation, never enforced.
	KVNamespace string `json:"kvNamespace,omitempty"`
	// Widget is the free-form UI hint (itemlist, vector, player, ...).
	Widget string `json:"x-vyshka-widget,omitempty"`
}

// NotSchema is the one form of not the subset admits: values the field must
// not take.
type NotSchema struct {
	Enum []any `json:"enum"`
}

// ContextEntries is one enumeration of a custom context (spec section 6.2).
type ContextEntries struct {
	Context string         `json:"context"`
	Entries []ContextEntry `json:"entries"`
	// Reason is the plugin's explanation when it sent one, empty otherwise.
	Reason       string          `json:"reason"`
	EnumeratedAt time.Time       `json:"enumeratedAt"`
	Raw          json.RawMessage `json:"-"`
}

func (c *ContextEntries) UnmarshalJSON(data []byte) error {
	type plain ContextEntries
	return jsonUnmarshalRaw(data, (*plain)(c), &c.Raw)
}

// ContextEntry is one member of a custom context.
type ContextEntry struct {
	ReferenceKey string         `json:"referenceKey"`
	Label        string         `json:"label"`
	Position     []float64      `json:"position"`
	Data         map[string]any `json:"data"`
}

// DispatchRequest is the body of an action dispatch (spec section 7). Empty
// optional fields are left out, so the hub's defaults apply; nil Params is
// sent as {}.
type DispatchRequest struct {
	Code           string         `json:"code"`
	Context        string         `json:"context,omitempty"`
	ReferenceKey   string         `json:"referenceKey,omitempty"`
	Params         map[string]any `json:"params"`
	TTLSeconds     int            `json:"ttlSeconds,omitempty"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty"`
}

// Dispatched is the answer to a dispatch: the action's id and its state,
// queued for a fresh dispatch or the original's current state for an
// idempotent retry.
type Dispatched struct {
	ActionID string          `json:"actionId"`
	State    string          `json:"state"`
	Raw      json.RawMessage `json:"-"`
}

func (d *Dispatched) UnmarshalJSON(data []byte) error {
	type plain Dispatched
	return jsonUnmarshalRaw(data, (*plain)(d), &d.Raw)
}

// Action is one tracked job and everything that happened to it (spec
// section 7).
type Action struct {
	ID             string         `json:"id"`
	ServerID       string         `json:"serverId"`
	Code           string         `json:"code"`
	Context        string         `json:"context"`
	ReferenceKey   string         `json:"referenceKey"`
	Params         map[string]any `json:"params"`
	IdempotencyKey string         `json:"idempotencyKey"`
	// State is queued, delivered, running, completed, failed, or expired in
	// this draft. A state this client does not know is passed through.
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"createdAt"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	DeliveredAt *time.Time `json:"deliveredAt"`
	RunningAt   *time.Time `json:"runningAt"`
	FinishedAt  *time.Time `json:"finishedAt"`
	// OK is nil until an action.result landed.
	OK *bool `json:"ok"`
	// Result is the plugin's result payload; empty or JSON null when there is
	// none.
	Result     json.RawMessage `json:"result"`
	Error      *string         `json:"error"`
	DurationMs *int64          `json:"durationMs"`
	Raw        json.RawMessage `json:"-"`
}

func (a *Action) UnmarshalJSON(data []byte) error {
	type plain Action
	return jsonUnmarshalRaw(data, (*plain)(a), &a.Raw)
}

// Terminal reports whether the action has reached a final state: completed,
// failed, or expired. Expiry is final and wins every race (spec section 7).
func (a Action) Terminal() bool {
	switch a.State {
	case "completed", "failed", "expired":
		return true
	}
	return false
}

// HasResult reports whether the action carries a result payload.
func (a Action) HasResult() bool {
	return len(a.Result) > 0 && string(a.Result) != "null"
}

// WaitOptions tunes WaitAction.
type WaitOptions struct {
	// Interval between reads; zero means 500 ms.
	Interval time.Duration
	// OnChange, when set, is called with the record on the first read and on
	// every read whose state differs from the one before.
	OnChange func(Action)
}

// EventQuery filters one read of an event feed (spec section 8.5). Zero
// values are left out.
type EventQuery struct {
	// Types are exact types, {namespace}.* patterns, or *, ORed.
	Types []string
	// Since is inclusive and Until exclusive, both on occurredAt.
	Since, Until *time.Time
	Limit        int
	Cursor       string
}

// Event is one stored telemetry event (spec section 8.1).
type Event struct {
	ID         string         `json:"id"`
	ServerID   string         `json:"serverId"`
	Type       string         `json:"type"`
	OccurredAt time.Time      `json:"occurredAt"`
	ReceivedAt time.Time      `json:"receivedAt"`
	Data       map[string]any `json:"data"`
	// Roles is present only on a player profile's feed (spec section 8.6):
	// the members of Data that hold the identity.
	Roles []string        `json:"roles,omitempty"`
	Raw   json.RawMessage `json:"-"`
}

func (e *Event) UnmarshalJSON(data []byte) error {
	type plain Event
	return jsonUnmarshalRaw(data, (*plain)(e), &e.Raw)
}

// EventPage is one page of a feed, newest first. NextCursor is empty on the
// last page.
type EventPage struct {
	Events     []Event         `json:"events"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

func (p *EventPage) UnmarshalJSON(data []byte) error {
	type plain EventPage
	return jsonUnmarshalRaw(data, (*plain)(p), &p.Raw)
}

// StateSnapshot is one accepted state snapshot (spec section 8.3). Snapshot is
// the plugin's body verbatim; CapturedAt beside it is the hub's reading of
// when the game sampled it, and is the one to trust.
type StateSnapshot struct {
	Type       string          `json:"type"`
	CapturedAt time.Time       `json:"capturedAt"`
	ReceivedAt time.Time       `json:"receivedAt"`
	Snapshot   json.RawMessage `json:"snapshot"`
	Raw        json.RawMessage `json:"-"`
}

func (s *StateSnapshot) UnmarshalJSON(data []byte) error {
	type plain StateSnapshot
	return jsonUnmarshalRaw(data, (*plain)(s), &s.Raw)
}

// StateHistoryPage is the answer to a history read: recent snapshots of one
// type, newest first, the latest included. Raw is the whole answer as the
// hub sent it.
type StateHistoryPage struct {
	Snapshots []StateSnapshot `json:"snapshots"`
	Raw       json.RawMessage `json:"-"`
}

func (p *StateHistoryPage) UnmarshalJSON(data []byte) error {
	type plain StateHistoryPage
	return jsonUnmarshalRaw(data, (*plain)(p), &p.Raw)
}

// PlayerEntry is one entry of a players snapshot.
type PlayerEntry struct {
	Player   Identity       `json:"player"`
	Name     string         `json:"name"`
	Position []float64      `json:"position"`
	Data     map[string]any `json:"data"`
}

// ThingEntry is one entry of a vehicles or entities snapshot.
type ThingEntry struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Position []float64      `json:"position"`
	Data     map[string]any `json:"data"`
}

// WorldState is the body of a world snapshot. Time is the game's own
// calendar, not a moment on any real clock.
type WorldState struct {
	Time string         `json:"time"`
	Data map[string]any `json:"data"`
}

// Players decodes a players snapshot's list.
func (s StateSnapshot) Players() ([]PlayerEntry, error) {
	var body struct {
		Players []PlayerEntry `json:"players"`
	}
	err := json.Unmarshal(s.Snapshot, &body)
	return body.Players, err
}

// Vehicles decodes a vehicles snapshot's list.
func (s StateSnapshot) Vehicles() ([]ThingEntry, error) {
	var body struct {
		Vehicles []ThingEntry `json:"vehicles"`
	}
	err := json.Unmarshal(s.Snapshot, &body)
	return body.Vehicles, err
}

// Entities decodes an entities snapshot's list.
func (s StateSnapshot) Entities() ([]ThingEntry, error) {
	var body struct {
		Entities []ThingEntry `json:"entities"`
	}
	err := json.Unmarshal(s.Snapshot, &body)
	return body.Entities, err
}

// World decodes a world snapshot.
func (s StateSnapshot) World() (WorldState, error) {
	var body struct {
		World WorldState `json:"world"`
	}
	err := json.Unmarshal(s.Snapshot, &body)
	return body.World, err
}

// KVEntry is one key with its value (spec section 12.2).
type KVEntry struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Revision  int64           `json:"revision"`
	// ExpiresAt is nil for a key without a TTL.
	ExpiresAt *time.Time      `json:"expiresAt,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func (k *KVEntry) UnmarshalJSON(data []byte) error {
	type plain KVEntry
	return jsonUnmarshalRaw(data, (*plain)(k), &k.Raw)
}

// KVSetRequest is the body of a set. IfRevision is a pointer because 0 is a
// meaningful guard ("only if the key does not exist") and nil means
// unconditional. TTLSeconds zero means the key does not expire.
type KVSetRequest struct {
	Value      any    `json:"value"`
	IfRevision *int64 `json:"ifRevision,omitempty"`
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
}

// KVWriteResult is a set's answer; the value is not echoed back.
type KVWriteResult struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Revision  int64           `json:"revision"`
	ExpiresAt *time.Time      `json:"expiresAt,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func (k *KVWriteResult) UnmarshalJSON(data []byte) error {
	type plain KVWriteResult
	return jsonUnmarshalRaw(data, (*plain)(k), &k.Raw)
}

// KVListQuery filters one page of a key listing. Zero values are left out.
type KVListQuery struct {
	// Prefix is a literal prefix of the key, not a pattern.
	Prefix string
	Limit  int
	Cursor string
}

// KVKeyPage is one page of a namespace's live keys, key ascending.
type KVKeyPage struct {
	Namespace  string          `json:"namespace"`
	Keys       []KVKey         `json:"keys"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

func (p *KVKeyPage) UnmarshalJSON(data []byte) error {
	type plain KVKeyPage
	return jsonUnmarshalRaw(data, (*plain)(p), &p.Raw)
}

// KVKey is one row of a key listing; values are not included.
type KVKey struct {
	Key       string     `json:"key"`
	Revision  int64      `json:"revision"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// KVNamespace is one namespace holding live keys the token may read.
type KVNamespace struct {
	Namespace string          `json:"namespace"`
	Keys      int             `json:"keys"`
	Raw       json.RawMessage `json:"-"`
}

func (n *KVNamespace) UnmarshalJSON(data []byte) error {
	type plain KVNamespace
	return jsonUnmarshalRaw(data, (*plain)(n), &n.Raw)
}

// KVNamespaceList is the answer to a namespace listing, name ascending. It
// is not paged. Raw is the whole answer as the hub sent it.
type KVNamespaceList struct {
	Namespaces []KVNamespace   `json:"namespaces"`
	Raw        json.RawMessage `json:"-"`
}

func (l *KVNamespaceList) UnmarshalJSON(data []byte) error {
	type plain KVNamespaceList
	return jsonUnmarshalRaw(data, (*plain)(l), &l.Raw)
}

// PageQuery is the paging of the player profile's action and note reads.
type PageQuery struct {
	Limit  int
	Cursor string
}

// ActionPage is one page of a player's actions, newest first.
type ActionPage struct {
	Actions    []Action        `json:"actions"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

func (p *ActionPage) UnmarshalJSON(data []byte) error {
	type plain ActionPage
	return jsonUnmarshalRaw(data, (*plain)(p), &p.Raw)
}

// Note is one operator note on a player (spec section 8.6).
type Note struct {
	ID        string     `json:"id"`
	Player    Identity   `json:"player"`
	Text      string     `json:"text"`
	CreatedAt time.Time  `json:"createdAt"`
	CreatedBy NoteAuthor `json:"createdBy"`
}

// NoteAuthor is the credential that wrote a note, under the name it had then.
// TokenID is empty for a bootstrap credential.
type NoteAuthor struct {
	TokenID   string `json:"tokenId"`
	TokenName string `json:"tokenName"`
}

// NotePage is one page of a player's notes, newest first.
type NotePage struct {
	Notes      []Note          `json:"notes"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

func (p *NotePage) UnmarshalJSON(data []byte) error {
	type plain NotePage
	return jsonUnmarshalRaw(data, (*plain)(p), &p.Raw)
}
