package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/id"
	"github.com/That1Drifter/vyshka/hub/internal/token"
)

// Errors the enrollment and session queries return. Handlers map these onto
// protocol error codes (spec/protocol.md section 5); nothing else about the
// wire format leaks into this package.
var (
	ErrNotFound               = errors.New("not found")
	ErrEnrollmentTokenInvalid = errors.New("enrollment token is unknown or expired")
	ErrEnrollmentTokenUsed    = errors.New("enrollment token has already been used")
	ErrGameMismatch           = errors.New("server record declares a different game")
	ErrCredentialsInvalid     = errors.New("server credentials do not match")
	ErrCredentialsRevoked     = errors.New("server credentials have been revoked")
)

// Credential states of a server record, mirrored on the Admin API.
const (
	CredentialStateNone    = "none"
	CredentialStateActive  = "active"
	CredentialStateRevoked = "revoked"
)

// Reasons recorded on a session that the hub ended rather than let expire.
const (
	SessionEndSuperseded  = "superseded"
	SessionEndCredentials = "credentials-changed"
	SessionEndRevoked     = "credentials-revoked"
)

// Server is one game server record: its identity, its credential state, and
// what the plugin last told us about itself.
type Server struct {
	ID             string
	Name           string
	Game           string
	CreatedAt      time.Time
	EnrolledAt     *time.Time
	RevokedAt      *time.Time
	LastSeenAt     *time.Time
	HasCredentials bool
	PluginName     string
	PluginVersion  string
	Transports     []string
	// LinkState is the link monitor's classification (spec section 11.1):
	// unknown until a session has been observed, then up or down.
	LinkState string
}

// CredentialState reports whether the server can authenticate right now.
func (s Server) CredentialState() string {
	switch {
	case s.RevokedAt != nil:
		return CredentialStateRevoked
	case s.HasCredentials:
		return CredentialStateActive
	default:
		return CredentialStateNone
	}
}

// PluginInfo is what a plugin says about itself at enrollment or session start.
// All of it is advisory: the hub displays it and never routes on it.
type PluginInfo struct {
	Name       string
	Version    string
	Transports []string
}

// Session is one live (or ended) plugin session, including the per-direction
// sequence state that dies with it (spec section 9.1).
type Session struct {
	ID                 string
	ServerID           string
	CreatedAt          time.Time
	ExpiresAt          time.Time
	EndedAt            *time.Time
	EndReason          string
	ProtocolVersion    int
	PollTimeoutSeconds int
	// OutboundSeq is the highest seq the hub has assigned to this session, and
	// OutboundAck the highest the plugin has acked.
	OutboundSeq int64
	OutboundAck int64
	// InboundAck is the highest contiguous seq the hub has durably processed
	// from the plugin; InboundCount is how many envelopes that took.
	InboundAck   int64
	InboundCount int64
}

// Live reports whether a session may still authenticate calls at now.
func (s Session) Live(now time.Time) bool {
	return s.EndedAt == nil && s.ExpiresAt.After(now)
}

// NewSession is the request side of StartSession.
type NewSession struct {
	ServerID           string
	TokenHash          string
	TTL                time.Duration
	ProtocolVersion    int
	PollTimeoutSeconds int
	Plugin             PluginInfo
}

// timeLayout is fixed width and always UTC, so that string comparison in SQL
// orders timestamps correctly. RFC 3339 with variable fractional digits does
// not: "…:00.5Z" sorts after "…:00.5001Z".
const timeLayout = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(timeLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	return parsed, nil
}

// scanTime converts a nullable timestamp column into a pointer.
func scanTime(column sql.NullString) (*time.Time, error) {
	if !column.Valid || column.String == "" {
		return nil, nil
	}
	parsed, err := parseTime(column.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

const serverColumns = `id, name, game, created_at, secret_hash, enrolled_at, revoked_at,
	plugin_name, plugin_version, transports, last_seen_at, link_state`

const sessionColumns = `id, server_id, created_at, expires_at, ended_at, end_reason,
	protocol_version, poll_timeout_seconds, outbound_seq, outbound_ack,
	inbound_ack, inbound_count`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanServer(row rowScanner) (Server, error) {
	var (
		server                            Server
		createdAt, secretHash, transports string
		enrolledAt, revokedAt, lastSeenAt sql.NullString
	)

	if err := row.Scan(&server.ID, &server.Name, &server.Game, &createdAt, &secretHash,
		&enrolledAt, &revokedAt, &server.PluginName, &server.PluginVersion,
		&transports, &lastSeenAt, &server.LinkState,
	); err != nil {
		return Server{}, err
	}

	var err error
	if server.CreatedAt, err = parseTime(createdAt); err != nil {
		return Server{}, err
	}
	if server.EnrolledAt, err = scanTime(enrolledAt); err != nil {
		return Server{}, err
	}
	if server.RevokedAt, err = scanTime(revokedAt); err != nil {
		return Server{}, err
	}
	if server.LastSeenAt, err = scanTime(lastSeenAt); err != nil {
		return Server{}, err
	}
	server.HasCredentials = secretHash != ""
	server.Transports = splitTransports(transports)

	return server, nil
}

func splitTransports(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func joinTransports(values []string) string { return strings.Join(values, ",") }

// CreateServer records a game server the operator intends to enroll. game is
// optional: empty means "whatever enrolls here".
func (s *Store) CreateServer(ctx context.Context, name, game string) (Server, error) {
	now := time.Now().UTC()
	server := Server{
		ID:        id.NewAt(now),
		Name:      name,
		Game:      game,
		CreatedAt: now.Truncate(time.Millisecond),
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO servers (id, name, game, created_at) VALUES (?, ?, ?, ?)`,
		server.ID, server.Name, server.Game, formatTime(now),
	); err != nil {
		return Server{}, fmt.Errorf("insert server: %w", err)
	}
	return server, nil
}

// Server returns one server record, or ErrNotFound.
func (s *Store) Server(ctx context.Context, serverID string) (Server, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+serverColumns+` FROM servers WHERE id = ?`, serverID)

	server, err := scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, fmt.Errorf("read server: %w", err)
	}
	return server, nil
}

// ListServers returns every server record, newest first.
func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+serverColumns+` FROM servers ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	defer rows.Close()

	servers := []Server{}
	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan server: %w", err)
		}
		servers = append(servers, server)
	}
	return servers, rows.Err()
}

// IssueEnrollmentToken stores the digest of a fresh one-time token and
// invalidates any unused token the server still had, so that only the most
// recently issued token can enroll. It returns the expiry it recorded.
func (s *Store) IssueEnrollmentToken(ctx context.Context, serverID, tokenHash string, ttl time.Duration) (time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(ttl).Truncate(time.Millisecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("begin issue enrollment token: %w", err)
	}
	defer tx.Rollback()

	var exists string
	switch err := tx.QueryRowContext(ctx, `SELECT id FROM servers WHERE id = ?`, serverID).Scan(&exists); {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, ErrNotFound
	case err != nil:
		return time.Time{}, fmt.Errorf("read server: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM enrollment_tokens WHERE server_id = ? AND used_at IS NULL`, serverID,
	); err != nil {
		return time.Time{}, fmt.Errorf("clear unused enrollment tokens: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO enrollment_tokens (token_hash, server_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`,
		tokenHash, serverID, formatTime(now), formatTime(expiresAt),
	); err != nil {
		return time.Time{}, fmt.Errorf("insert enrollment token: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return time.Time{}, fmt.Errorf("commit issue enrollment token: %w", err)
	}
	return expiresAt, nil
}

// Enroll burns an enrollment token and installs the server's credentials in one
// transaction, so two plugins racing on the same token produce exactly one
// success. Enrolling over an existing credential replaces it and ends every
// session derived from the old secret.
func (s *Store) Enroll(ctx context.Context, tokenHash, game, secretHash string, plugin PluginInfo) (Server, error) {
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Server{}, fmt.Errorf("begin enroll: %w", err)
	}
	defer tx.Rollback()

	var (
		serverID  string
		expiresAt string
		usedAt    sql.NullString
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT server_id, expires_at, used_at FROM enrollment_tokens WHERE token_hash = ?`,
		tokenHash,
	).Scan(&serverID, &expiresAt, &usedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return Server{}, ErrEnrollmentTokenInvalid
	case err != nil:
		return Server{}, fmt.Errorf("read enrollment token: %w", err)
	}
	if usedAt.Valid && usedAt.String != "" {
		return Server{}, ErrEnrollmentTokenUsed
	}
	expiry, err := parseTime(expiresAt)
	if err != nil {
		return Server{}, err
	}
	if !expiry.After(now) {
		return Server{}, ErrEnrollmentTokenInvalid
	}

	var declaredGame string
	if err := tx.QueryRowContext(ctx, `SELECT game FROM servers WHERE id = ?`, serverID).Scan(&declaredGame); err != nil {
		return Server{}, fmt.Errorf("read server: %w", err)
	}
	if declaredGame != "" && declaredGame != game {
		return Server{}, ErrGameMismatch
	}

	// The guard on used_at is what makes the burn atomic under concurrency.
	result, err := tx.ExecContext(ctx,
		`UPDATE enrollment_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`,
		formatTime(now), tokenHash)
	if err != nil {
		return Server{}, fmt.Errorf("burn enrollment token: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Server{}, fmt.Errorf("burn enrollment token: %w", err)
	} else if affected != 1 {
		return Server{}, ErrEnrollmentTokenUsed
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE servers
		    SET secret_hash = ?, game = ?, enrolled_at = COALESCE(enrolled_at, ?),
		        revoked_at = NULL, plugin_name = ?, plugin_version = ?, transports = ?
		  WHERE id = ?`,
		secretHash, game, formatTime(now),
		plugin.Name, plugin.Version, joinTransports(plugin.Transports), serverID,
	); err != nil {
		return Server{}, fmt.Errorf("install server credentials: %w", err)
	}

	if err := endSessions(ctx, tx, serverID, SessionEndCredentials, now); err != nil {
		return Server{}, err
	}

	row := tx.QueryRowContext(ctx, `SELECT `+serverColumns+` FROM servers WHERE id = ?`, serverID)
	server, err := scanServer(row)
	if err != nil {
		return Server{}, fmt.Errorf("read enrolled server: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Server{}, fmt.Errorf("commit enroll: %w", err)
	}
	return server, nil
}

// AuthenticateServer checks a server id and secret digest pair. A wrong secret
// and an unknown server are the same answer on purpose; a correct secret on a
// revoked server is not, because the plugin needs to know to stop retrying.
func (s *Store) AuthenticateServer(ctx context.Context, serverID, secretHash string) (Server, error) {
	var storedHash string
	switch err := s.db.QueryRowContext(ctx,
		`SELECT secret_hash FROM servers WHERE id = ?`, serverID).Scan(&storedHash); {
	case errors.Is(err, sql.ErrNoRows):
		return Server{}, ErrCredentialsInvalid
	case err != nil:
		return Server{}, fmt.Errorf("read server credentials: %w", err)
	}
	if storedHash == "" || !token.Equal(storedHash, secretHash) {
		return Server{}, ErrCredentialsInvalid
	}

	server, err := s.Server(ctx, serverID)
	if err != nil {
		return Server{}, err
	}
	if server.RevokedAt != nil {
		return Server{}, ErrCredentialsRevoked
	}
	return server, nil
}

// RevokeCredentials disables a server's secret and ends its sessions at once,
// per spec section 5.4. It is idempotent: revoking a server that never enrolled
// leaves it unable to authenticate, which is already true.
func (s *Store) RevokeCredentials(ctx context.Context, serverID string) error {
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin revoke: %w", err)
	}
	defer tx.Rollback()

	var secretHash string
	switch err := tx.QueryRowContext(ctx,
		`SELECT secret_hash FROM servers WHERE id = ?`, serverID).Scan(&secretHash); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("read server: %w", err)
	}

	if secretHash != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE servers SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`,
			formatTime(now), serverID,
		); err != nil {
			return fmt.Errorf("revoke server credentials: %w", err)
		}
	}

	if err := endSessions(ctx, tx, serverID, SessionEndRevoked, now); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit revoke: %w", err)
	}
	return nil
}

// StartSession issues a session and ends any earlier one for the same server,
// keeping the "at most one live session per server" rule of spec section 5.3.
func (s *Store) StartSession(ctx context.Context, request NewSession) (Session, error) {
	now := time.Now().UTC()
	session := Session{
		ID:                 id.NewAt(now),
		ServerID:           request.ServerID,
		CreatedAt:          now.Truncate(time.Millisecond),
		ExpiresAt:          now.Add(request.TTL).Truncate(time.Millisecond),
		ProtocolVersion:    request.ProtocolVersion,
		PollTimeoutSeconds: request.PollTimeoutSeconds,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin start session: %w", err)
	}
	defer tx.Rollback()

	if err := endSessions(ctx, tx, request.ServerID, SessionEndSuperseded, now); err != nil {
		return Session{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (id, token_hash, server_id, created_at, expires_at,
		                       protocol_version, poll_timeout_seconds)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		session.ID, request.TokenHash, session.ServerID,
		formatTime(session.CreatedAt), formatTime(session.ExpiresAt),
		session.ProtocolVersion, session.PollTimeoutSeconds,
	); err != nil {
		return Session{}, fmt.Errorf("insert session: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE servers
		    SET last_seen_at = ?, plugin_name = ?, plugin_version = ?, transports = ?
		  WHERE id = ?`,
		formatTime(now), request.Plugin.Name, request.Plugin.Version,
		joinTransports(request.Plugin.Transports), request.ServerID,
	); err != nil {
		return Session{}, fmt.Errorf("update server on session start: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit start session: %w", err)
	}
	return session, nil
}

// SessionByToken resolves a session token digest to its session and server. It
// returns ErrNotFound for a token that is unknown, ended, or expired, so that
// callers cannot tell those apart from outside.
func (s *Store) SessionByToken(ctx context.Context, tokenHash string) (Session, Server, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE token_hash = ?`, tokenHash)

	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, Server{}, ErrNotFound
	}
	if err != nil {
		return Session{}, Server{}, fmt.Errorf("read session: %w", err)
	}
	if !session.Live(time.Now().UTC()) {
		return Session{}, Server{}, ErrNotFound
	}

	server, err := s.Server(ctx, session.ServerID)
	if err != nil {
		return Session{}, Server{}, err
	}
	return session, server, nil
}

// LiveSession returns the server's current session, or nil when it has none.
func (s *Store) LiveSession(ctx context.Context, serverID string) (*Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+`
		   FROM sessions
		  WHERE server_id = ? AND ended_at IS NULL AND expires_at > ?
		  ORDER BY created_at DESC LIMIT 1`,
		serverID, formatTime(time.Now().UTC()))

	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read live session: %w", err)
	}
	return &session, nil
}

func scanSession(row rowScanner) (Session, error) {
	var (
		session              Session
		createdAt, expiresAt string
		endedAt              sql.NullString
	)
	if err := row.Scan(&session.ID, &session.ServerID, &createdAt, &expiresAt,
		&endedAt, &session.EndReason, &session.ProtocolVersion, &session.PollTimeoutSeconds,
		&session.OutboundSeq, &session.OutboundAck, &session.InboundAck, &session.InboundCount,
	); err != nil {
		return Session{}, err
	}

	var err error
	if session.CreatedAt, err = parseTime(createdAt); err != nil {
		return Session{}, err
	}
	if session.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return Session{}, err
	}
	if session.EndedAt, err = scanTime(endedAt); err != nil {
		return Session{}, err
	}
	return session, nil
}

// endSessions closes every live session of a server inside an open transaction.
func endSessions(ctx context.Context, tx *sql.Tx, serverID, reason string, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET ended_at = ?, end_reason = ?
		  WHERE server_id = ? AND ended_at IS NULL`,
		formatTime(now), reason, serverID,
	); err != nil {
		return fmt.Errorf("end sessions: %w", err)
	}
	return nil
}
