package hub

import (
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

// Protocol constants this hub implements (spec/protocol.md sections 3.1.1, 4
// and 13).
const (
	// ProtocolVersion is the current negotiated version.
	ProtocolVersion = 1
	// EnvelopeVersion is the `v` this hub stamps on envelopes.
	EnvelopeVersion = 1

	// DefaultPollTimeout applies when a plugin requests nothing.
	DefaultPollTimeout = 25 * time.Second
	// MinPollTimeout and MaxPollTimeout bound what a plugin may request; values
	// outside the range are clamped, never silently replaced.
	MinPollTimeout = 5 * time.Second
	MaxPollTimeout = 60 * time.Second
)

// supportedProtocolVersions is what a session may negotiate. A hub must serve
// the current and previous major version; there is no previous one yet.
var supportedProtocolVersions = []int{ProtocolVersion}

// supportedTransports is advertised in the session response. WebSocket is an
// optional upgrade and arrives with its own slice.
var supportedTransports = []string{"poll"}

func protocolVersionSupported(version int) bool {
	for _, supported := range supportedProtocolVersions {
		if version == supported {
			return true
		}
	}
	return false
}

// clampPollTimeout applies the section 3.1.1 bounds to a requested value.
func clampPollTimeout(requested time.Duration) time.Duration {
	switch {
	case requested <= 0:
		return DefaultPollTimeout
	case requested < MinPollTimeout:
		return MinPollTimeout
	case requested > MaxPollTimeout:
		return MaxPollTimeout
	default:
		return requested
	}
}

// normalizeGame folds a game identifier to its comparable form. Game ids are
// lowercase ASCII by spec, and comparison is case-insensitive, so "DayZ" in a
// config file still matches the record the operator created.
func normalizeGame(game string) string {
	return strings.ToLower(strings.TrimSpace(game))
}

// serverView is the Admin API representation of a server record. It never
// carries a credential: the hub stores only digests and cannot reveal one.
type serverView struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Game            string       `json:"game"`
	CreatedAt       time.Time    `json:"createdAt"`
	EnrolledAt      *time.Time   `json:"enrolledAt"`
	RevokedAt       *time.Time   `json:"revokedAt"`
	CredentialState string       `json:"credentialState"`
	LastSeenAt      *time.Time   `json:"lastSeenAt"`
	Plugin          *pluginView  `json:"plugin"`
	Session         *sessionView `json:"session"`
}

type pluginView struct {
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Transports []string `json:"transports"`
}

type sessionView struct {
	ID                 string    `json:"id"`
	ExpiresAt          time.Time `json:"expiresAt"`
	PollTimeoutSeconds int       `json:"pollTimeoutSeconds"`
}

func newServerView(server store.Server, session *store.Session) serverView {
	view := serverView{
		ID:              server.ID,
		Name:            server.Name,
		Game:            server.Game,
		CreatedAt:       server.CreatedAt,
		EnrolledAt:      server.EnrolledAt,
		RevokedAt:       server.RevokedAt,
		CredentialState: server.CredentialState(),
		LastSeenAt:      server.LastSeenAt,
	}
	if server.PluginName != "" || server.PluginVersion != "" || len(server.Transports) > 0 {
		view.Plugin = &pluginView{
			Name:       server.PluginName,
			Version:    server.PluginVersion,
			Transports: server.Transports,
		}
	}
	if session != nil {
		view.Session = &sessionView{
			ID:                 session.ID,
			ExpiresAt:          session.ExpiresAt,
			PollTimeoutSeconds: session.PollTimeoutSeconds,
		}
	}
	return view
}

// serverIdentity is the subset of a server record the Plugin API echoes back,
// so a plugin can log which record it is attached to.
type serverIdentity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Game string `json:"game"`
}

func newServerIdentity(server store.Server) serverIdentity {
	return serverIdentity{ID: server.ID, Name: server.Name, Game: server.Game}
}
