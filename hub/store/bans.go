package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The installation ban list (spec section 13): one list for every server of
// the installation, held here and pulled by the plugins that enforce it.

// BanChangedType is the hub -> plugin notice that the active list changed
// (spec section 13.3). The store queues it itself, in the transaction that
// changed the list, so a committed change is never missing its notices.
const BanChangedType = "bans.changed"

// CapabilityBans is the manifest capability that marks a plugin as one that
// enforces the list (spec section 6.7).
const CapabilityBans = "bans"

// ErrBanRevisionGone is returned for a walk of a revision the store cannot
// serve: one above the current revision, or one this hub never minted, which
// only a cursor minted before a restore from an older backup can name (spec
// section 13.3).
var ErrBanRevisionGone = errors.New("the ban list cannot be served at that revision")

// BanActiveError is returned when an identity already carries an active ban;
// BanID names it (spec section 13.2).
type BanActiveError struct{ BanID string }

func (e *BanActiveError) Error() string { return "the identity already carries active ban " + e.BanID }

// Ban is one ban record, active or not.
type Ban struct {
	ID        string
	Platform  string
	PlayerID  string
	Reason    string
	Name      string
	ServerID  string // provenance; "" when none was given
	CreatedAt time.Time
	// TokenID is "" for the bootstrap credential; TokenName is the writer's
	// name as it was when the ban was placed. The Lifted* pair is the same for
	// the lift, empty while the ban stands.
	TokenID         string
	TokenName       string
	ExpiresAt       *time.Time
	LiftedAt        *time.Time
	LiftedTokenID   string
	LiftedTokenName string
	// ListedRevision is the revision at which the ban joined the active list;
	// DelistedRevision the one at which it left, nil while it is on it.
	ListedRevision   int64
	DelistedRevision *int64
}

// OnList reports whether the ban is on the active list the plugins are served,
// which is not quite "not lifted and not expired": an expired ban stays on it
// until the sweep takes it off (spec section 13.1).
func (b Ban) OnList() bool { return b.DelistedRevision == nil }

// NewBan is the request side of CreateBan.
type NewBan struct {
	ID        string
	Platform  string
	PlayerID  string
	Reason    string
	Name      string
	ServerID  string
	TokenID   string
	TokenName string
	// Duration is how long the ban lasts from the store's clock at creation;
	// zero for a ban that does not end on its own.
	Duration time.Duration
}

// BanChange is what a committed change to the list did. Changed is false for
// a lift that found nothing to lift and a sweep that found nothing expired;
// Revision is the active list's revision after the call either way. Notified
// lists the servers a bans.changed was queued or refreshed for, and Dropped
// those whose queue was at its bound, so the caller can wake the first and
// log the second.
type BanChange struct {
	Ban      Ban
	Revision int64
	Changed  bool
	Notified []string
	Dropped  []string
}

const banColumns = `id, platform, player_id, reason, name, server_id, created_at, token_id, token_name,
	expires_at, lifted_at, lifted_token_id, lifted_token_name, listed_revision, delisted_revision`

func scanBan(row rowScanner) (Ban, error) {
	var (
		ban                 Ban
		createdAt           string
		expiresAt, liftedAt sql.NullString
		delisted            sql.NullInt64
	)
	if err := row.Scan(&ban.ID, &ban.Platform, &ban.PlayerID, &ban.Reason, &ban.Name, &ban.ServerID,
		&createdAt, &ban.TokenID, &ban.TokenName, &expiresAt, &liftedAt,
		&ban.LiftedTokenID, &ban.LiftedTokenName, &ban.ListedRevision, &delisted); err != nil {
		return Ban{}, err
	}
	var err error
	if ban.CreatedAt, err = parseTime(createdAt); err != nil {
		return Ban{}, err
	}
	if ban.ExpiresAt, err = scanTime(expiresAt); err != nil {
		return Ban{}, err
	}
	if ban.LiftedAt, err = scanTime(liftedAt); err != nil {
		return Ban{}, err
	}
	if delisted.Valid {
		revision := delisted.Int64
		ban.DelistedRevision = &revision
	}
	return ban, nil
}

// lockBanList takes the list's lock for the rest of the transaction and
// returns the current revision. Every change to the list starts here, so two
// changes are two revisions and a change's notices are queued against the
// revision it made. On Postgres it is a row lock on the one ban_list row; on
// SQLite the single connection is the lock.
//
// It is taken before any server row. Nothing that holds a server row reaches
// for this one (the pull reads the revision without a lock, and the applied
// report writes only the server row), so the order cannot invert.
func lockBanList(ctx context.Context, tx *Tx) (int64, error) {
	var revision int64
	if err := tx.QueryRowContext(ctx,
		`SELECT revision FROM ban_list WHERE id = 1`+tx.forUpdate()).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read ban list revision: %w", err)
	}
	return revision, nil
}

// nextBanRevision mints the revision a change takes: one more than the last,
// or the clock in milliseconds since the epoch when that is larger. The clock
// is what keeps a hub restored from a backup from handing out a revision it
// already handed out before the restore for a different list (spec section
// 13.1): the restored hub's last revision is behind the ones it minted since,
// but the clock is not, so its next change lands above all of them and a
// plugin still holding a pre-restore revision sees it differ. Milliseconds
// stay far inside the protocol's 2^53 bound.
func nextBanRevision(current int64, now time.Time) int64 {
	return max(current+1, now.UnixMilli())
}

// setBanRevision records a change's revision as the current one and in the
// register of revisions this hub has minted, which is what a walk's cursor
// is checked against (BanListAt).
func setBanRevision(ctx context.Context, tx *Tx, revision int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE ban_list SET revision = ? WHERE id = 1`, revision); err != nil {
		return fmt.Errorf("record ban list revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ban_revisions (revision, minted_at) VALUES (?, ?)`, revision, formatTime(now)); err != nil {
		return fmt.Errorf("register ban list revision: %w", err)
	}
	return nil
}

// delistExpired takes every expired ban off the active list at revision next,
// inside a transaction that holds the list's lock, and reports how many it
// took. The caller bumps the revision to next when this found any.
func delistExpired(ctx context.Context, tx *Tx, next int64, now time.Time) (int, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE bans SET delisted_revision = ?
		  WHERE delisted_revision IS NULL AND expires_at IS NOT NULL AND expires_at <= ?`,
		next, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("delist expired bans: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delist expired bans: %w", err)
	}
	return int(affected), nil
}

// CreateBan places a ban and queues the notices of the change (spec sections
// 13.2 and 13.3). An identity that already carries an active ban is a
// *BanActiveError naming it. Expired bans are swept first, in the same
// change, so an identity whose last ban has expired but not yet been swept
// can be banned again at once; the sweep and the new ban are one revision.
func (s *Store) CreateBan(ctx context.Context, request NewBan, queueLimit int) (BanChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BanChange{}, fmt.Errorf("begin create ban: %w", err)
	}
	defer tx.Rollback()

	current, err := lockBanList(ctx, tx)
	if err != nil {
		return BanChange{}, err
	}
	now := time.Now().UTC()
	next := nextBanRevision(current, now)
	if _, err := delistExpired(ctx, tx, next, now); err != nil {
		return BanChange{}, err
	}

	var existing string
	switch err := tx.QueryRowContext(ctx,
		`SELECT id FROM bans WHERE platform = ? AND player_id = ? AND delisted_revision IS NULL`,
		request.Platform, request.PlayerID).Scan(&existing); {
	case err == nil:
		return BanChange{}, &BanActiveError{BanID: existing}
	case !errors.Is(err, sql.ErrNoRows):
		return BanChange{}, fmt.Errorf("read active ban: %w", err)
	}

	ban := Ban{
		ID:             request.ID,
		Platform:       request.Platform,
		PlayerID:       request.PlayerID,
		Reason:         request.Reason,
		Name:           request.Name,
		ServerID:       request.ServerID,
		CreatedAt:      now.Truncate(time.Millisecond),
		TokenID:        request.TokenID,
		TokenName:      request.TokenName,
		ListedRevision: next,
	}
	var expiresAt any
	if request.Duration > 0 {
		expiry := now.Add(request.Duration).Truncate(time.Millisecond)
		ban.ExpiresAt = &expiry
		expiresAt = formatTime(expiry)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO bans (id, platform, player_id, reason, name, server_id, created_at,
		                   token_id, token_name, expires_at, listed_revision)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ban.ID, ban.Platform, ban.PlayerID, ban.Reason, ban.Name, ban.ServerID,
		formatTime(now), ban.TokenID, ban.TokenName, expiresAt, next,
	); err != nil {
		return BanChange{}, fmt.Errorf("insert ban: %w", err)
	}
	if err := setBanRevision(ctx, tx, next, now); err != nil {
		return BanChange{}, err
	}
	change := BanChange{Ban: ban, Revision: next, Changed: true}
	if change.Notified, change.Dropped, err = queueBanNotices(ctx, tx, next, queueLimit); err != nil {
		return BanChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return BanChange{}, fmt.Errorf("commit create ban: %w", err)
	}
	return change, nil
}

// LiftBan lifts one ban (spec section 13.2). A ban on the active list is
// lifted and taken off it, and the change queues its notices; a ban already
// lifted, or expired, is answered as it stands with Changed false, so a
// repeated lift never moves the recorded lift. An unknown id is ErrNotFound.
//
// An expired ban the sweep has not reached yet is not lifted: it is already
// expired on every read (spec section 13.1), and recording a lift over it
// would make its history say something that did not happen. The sweep runs
// here instead, which takes it off the list.
func (s *Store) LiftBan(ctx context.Context, banID, tokenID, tokenName string, queueLimit int) (BanChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BanChange{}, fmt.Errorf("begin lift ban: %w", err)
	}
	defer tx.Rollback()

	current, err := lockBanList(ctx, tx)
	if err != nil {
		return BanChange{}, err
	}
	now := time.Now().UTC()
	next := nextBanRevision(current, now)
	swept, err := delistExpired(ctx, tx, next, now)
	if err != nil {
		return BanChange{}, err
	}

	ban, err := scanBan(tx.QueryRowContext(ctx, `SELECT `+banColumns+` FROM bans WHERE id = ?`, banID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return BanChange{}, ErrNotFound
	case err != nil:
		return BanChange{}, fmt.Errorf("read ban: %w", err)
	}

	change := BanChange{Ban: ban, Revision: current}
	if ban.OnList() {
		stamp := now.Truncate(time.Millisecond)
		if _, err := tx.ExecContext(ctx,
			`UPDATE bans SET lifted_at = ?, lifted_token_id = ?, lifted_token_name = ?, delisted_revision = ?
			  WHERE id = ? AND delisted_revision IS NULL`,
			formatTime(now), tokenID, tokenName, next, banID,
		); err != nil {
			return BanChange{}, fmt.Errorf("lift ban: %w", err)
		}
		change.Ban.LiftedAt = &stamp
		change.Ban.LiftedTokenID = tokenID
		change.Ban.LiftedTokenName = tokenName
		change.Ban.DelistedRevision = &next
		change.Changed = true
	}
	if change.Changed || swept > 0 {
		if err := setBanRevision(ctx, tx, next, now); err != nil {
			return BanChange{}, err
		}
		change.Revision = next
		if change.Notified, change.Dropped, err = queueBanNotices(ctx, tx, next, queueLimit); err != nil {
			return BanChange{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return BanChange{}, fmt.Errorf("commit lift ban: %w", err)
	}
	return change, nil
}

// SweepExpiredBans takes every expired ban off the active list in one change
// (spec section 13.1), queueing its notices, and reports how many it took. A
// sweep that finds nothing changes nothing and bumps nothing.
func (s *Store) SweepExpiredBans(ctx context.Context, queueLimit int) (BanChange, int, error) {
	// A cheap unlocked look first: this runs every few seconds, and taking the
	// list's lock for a sweep that finds nothing would put every idle pass in
	// the way of an operator's ban.
	now := time.Now().UTC()
	var due int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bans
		  WHERE delisted_revision IS NULL AND expires_at IS NOT NULL AND expires_at <= ?`,
		formatTime(now)).Scan(&due); err != nil {
		return BanChange{}, 0, fmt.Errorf("count expired bans: %w", err)
	}
	if due == 0 {
		return BanChange{}, 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BanChange{}, 0, fmt.Errorf("begin sweep bans: %w", err)
	}
	defer tx.Rollback()
	current, err := lockBanList(ctx, tx)
	if err != nil {
		return BanChange{}, 0, err
	}
	next := nextBanRevision(current, now)
	swept, err := delistExpired(ctx, tx, next, now)
	if err != nil {
		return BanChange{}, 0, err
	}
	change := BanChange{Revision: current}
	if swept > 0 {
		if err := setBanRevision(ctx, tx, next, now); err != nil {
			return BanChange{}, 0, err
		}
		change.Revision = next
		change.Changed = true
		if change.Notified, change.Dropped, err = queueBanNotices(ctx, tx, next, queueLimit); err != nil {
			return BanChange{}, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return BanChange{}, 0, fmt.Errorf("commit sweep bans: %w", err)
	}
	return change, swept, nil
}

// queueBanNotices queues a bans.changed carrying revision for every server
// whose stored manifest declares the bans capability and whose credentials
// stand (spec section 13.3), inside the transaction that made the change and
// holds the list's lock. A notice queued for a server and not yet sent is
// refreshed to the new revision in place rather than joined by another, so a
// burst of changes costs a server one notice; a notice already sent keeps its
// body, because a sent envelope is retransmitted unchanged (section 9.1). A
// server whose queue is at its bound is skipped and reported: the notice is a
// nudge, and the next session response carries the revision regardless.
//
// Servers are locked in id order, each through lockServer as every queue
// write is, after the list's lock; see lockBanList for why that order holds.
func queueBanNotices(ctx context.Context, tx *Tx, revision int64, queueLimit int) ([]string, []string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT s.id, m.capabilities, CASE WHEN m.capabilities IS NULL THEN m.body ELSE '' END
		   FROM servers s JOIN manifests m ON m.server_id = s.id
		  WHERE s.secret_hash <> '' AND s.revoked_at IS NULL`)
	if err != nil {
		return nil, nil, fmt.Errorf("read servers to notify of a ban change: %w", err)
	}
	var targets []string
	for rows.Next() {
		var (
			serverID     string
			capabilities sql.NullString
			body         string
		)
		if err := rows.Scan(&serverID, &capabilities, &body); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan server to notify of a ban change: %w", err)
		}
		if hasCapability(decodeCapabilities(capabilities, body), CapabilityBans) {
			targets = append(targets, serverID)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("read servers to notify of a ban change: %w", err)
	}
	sort.Strings(targets)

	runHook(testHooks.afterBanTargets)
	var notified, dropped []string
	for _, serverID := range targets {
		switch err := lockServer(ctx, tx, serverID); {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			return nil, nil, fmt.Errorf("lock server to notify of a ban change: %w", err)
		}
		queued, err := noticeBanRevision(ctx, tx, serverID, revision, queueLimit)
		if err != nil {
			return nil, nil, err
		}
		if !queued {
			dropped = append(dropped, serverID)
			continue
		}
		notified = append(notified, serverID)
	}
	return notified, dropped, nil
}

// noticeBanRevision queues one bans.changed carrying revision for a server
// whose row the transaction holds, refreshing a notice already queued and not
// yet sent rather than adding another; false when the server's queue is at
// its bound, which drops the notice (spec section 13.3).
func noticeBanRevision(ctx context.Context, tx *Tx, serverID string, revision int64, queueLimit int) (bool, error) {
	body, err := json.Marshal(map[string]int64{"revision": revision})
	if err != nil {
		return false, fmt.Errorf("encode bans.changed: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE outbound_envelopes SET body = ?
		  WHERE server_id = ? AND type = ? AND session_id IS NULL AND acked_at IS NULL`,
		string(body), serverID, BanChangedType)
	if err != nil {
		return false, fmt.Errorf("refresh bans.changed: %w", err)
	}
	refreshed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("refresh bans.changed: %w", err)
	}
	if refreshed > 0 {
		return true, nil
	}
	switch _, err := queueOutbound(ctx, tx, serverID, BanChangedType, body, queueLimit); {
	case errors.Is(err, ErrOutboundQueueFull):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// decodeCapabilities reads a stored manifest's capabilities: the column when
// it was written, and otherwise the body's own member, for a manifest stored
// before the column existed. Anything unreadable is no capability at all,
// which fails closed: the server is sent nothing and shown as not supported.
func decodeCapabilities(column sql.NullString, body string) []string {
	var capabilities []string
	if column.Valid {
		_ = json.Unmarshal([]byte(column.String), &capabilities)
		return capabilities
	}
	var manifest struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal([]byte(body), &manifest) == nil {
		capabilities = manifest.Capabilities
	}
	return capabilities
}

func hasCapability(capabilities []string, want string) bool {
	for _, one := range capabilities {
		if one == want {
			return true
		}
	}
	return false
}

// ManifestCapabilities returns the capabilities a server's stored manifest
// declares (spec section 6.7), empty when it has none or no manifest at all.
func (s *Store) ManifestCapabilities(ctx context.Context, serverID string) ([]string, error) {
	var (
		capabilities sql.NullString
		body         string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT capabilities, CASE WHEN capabilities IS NULL THEN body ELSE '' END
		   FROM manifests WHERE server_id = ?`, serverID).Scan(&capabilities, &body)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read manifest capabilities: %w", err)
	}
	return decodeCapabilities(capabilities, body), nil
}

// BanRevision returns the active list's current revision.
func (s *Store) BanRevision(ctx context.Context) (int64, error) {
	var revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT revision FROM ban_list WHERE id = 1`).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read ban list revision: %w", err)
	}
	return revision, nil
}

// Ban returns one ban record, or ErrNotFound.
func (s *Store) Ban(ctx context.Context, banID string) (Ban, error) {
	ban, err := scanBan(s.db.QueryRowContext(ctx, `SELECT `+banColumns+` FROM bans WHERE id = ?`, banID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Ban{}, ErrNotFound
	case err != nil:
		return Ban{}, fmt.Errorf("read ban: %w", err)
	}
	return ban, nil
}

// BanCursor is a position in the Admin API list, newest first by creation.
type BanCursor struct {
	CreatedAt time.Time
	ID        string
}

// Set reports whether the cursor names a position at all.
func (c BanCursor) Set() bool { return c.ID != "" }

// BanQuery is one page of the Admin API list (spec section 13.2).
type BanQuery struct {
	// All includes lifted and expired bans; otherwise only the active ones,
	// judged against Now so that a ban past its expiry reads as expired
	// before the sweep has reached it.
	All      bool
	Now      time.Time
	Platform string
	PlayerID string
	Limit    int
	After    BanCursor
}

// BansWithRevision answers one page of ban records and the active list's
// revision as one read: on Postgres both come from one repeatable-read
// snapshot, on SQLite from one transaction on the single connection, so the
// revision is the one the records belong to (spec section 13.2).
func (s *Store) BansWithRevision(ctx context.Context, query BanQuery) (int64, []Ban, error) {
	var options *sql.TxOptions
	if s.driver == dialectPostgres {
		options = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	tx, err := s.db.BeginTx(ctx, options)
	if err != nil {
		return 0, nil, fmt.Errorf("begin read bans: %w", err)
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM ban_list WHERE id = 1`).Scan(&revision); err != nil {
		return 0, nil, fmt.Errorf("read ban list revision: %w", err)
	}
	bans, err := readBans(ctx, tx, query)
	if err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit read bans: %w", err)
	}
	return revision, bans, nil
}

// Bans answers one page of ban records, newest first by (created_at, id).
func (s *Store) Bans(ctx context.Context, query BanQuery) ([]Ban, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin read bans: %w", err)
	}
	defer tx.Rollback()
	bans, err := readBans(ctx, tx, query)
	if err != nil {
		return nil, err
	}
	return bans, tx.Commit()
}

func readBans(ctx context.Context, tx *Tx, query BanQuery) ([]Ban, error) {
	var conditions []string
	var args []any
	if !query.All {
		conditions = append(conditions, "delisted_revision IS NULL AND (expires_at IS NULL OR expires_at > ?)")
		args = append(args, formatTime(query.Now))
	}
	if query.Platform != "" {
		conditions = append(conditions, "platform = ? AND player_id = ?")
		args = append(args, query.Platform, query.PlayerID)
	}
	if query.After.Set() {
		conditions = append(conditions, "(created_at < ? OR (created_at = ? AND id < ?))")
		at := formatTime(query.After.CreatedAt)
		args = append(args, at, at, query.After.ID)
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	args = append(args, query.Limit)
	rows, err := tx.QueryContext(ctx,
		`SELECT `+banColumns+` FROM bans `+where+`
		  ORDER BY created_at DESC, id DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read bans: %w", err)
	}
	defer rows.Close()
	bans := make([]Ban, 0, min(query.Limit, 128))
	for rows.Next() {
		ban, err := scanBan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan ban: %w", err)
		}
		bans = append(bans, ban)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read bans: %w", err)
	}
	return bans, nil
}

// BanListPosition is where a walk of the active list stands: the revision it
// reads and the last identity it was served. A zero Platform starts at the
// first entry.
type BanListPosition struct {
	Platform string
	PlayerID string
}

// BanListAt answers one page of the active list as it stood at revision, in
// identity order after the position (spec section 13.3). A negative revision
// means the current one. It returns the revision it served, which is the one
// asked for, or ErrBanRevisionGone when that is above the current revision.
//
// The page is the bans whose place in the list's history covers the revision:
// joined at or before it and not yet left by it. Reading the current revision
// and then the page takes no lock and needs none: a change committed between
// the two joins at a higher revision, which the page excludes, and a ban it
// takes off leaves at a higher revision, which the page still counts, so the
// page is the list at the revision read whatever lands in between.
func (s *Store) BanListAt(ctx context.Context, revision int64, after BanListPosition, limit int) (int64, []Ban, error) {
	current, err := s.BanRevision(ctx)
	if err != nil {
		return 0, nil, err
	}
	if revision < 0 {
		revision = current
	}
	if revision > current {
		return 0, nil, ErrBanRevisionGone
	}
	if revision != current && revision != 0 {
		// A cursor names a revision this hub minted, or it comes from another
		// history (a hub restored from a backup older than the cursor) and
		// the list it would describe was never this hub's.
		var known int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM ban_revisions WHERE revision = ?`, revision).Scan(&known); err != nil {
			return 0, nil, fmt.Errorf("read ban list revision register: %w", err)
		}
		if known == 0 {
			return 0, nil, ErrBanRevisionGone
		}
	}
	conditions := []string{"listed_revision <= ?", "(delisted_revision IS NULL OR delisted_revision > ?)"}
	args := []any{revision, revision}
	if after.Platform != "" {
		conditions = append(conditions, "(platform > ? OR (platform = ? AND player_id > ?))")
		args = append(args, after.Platform, after.Platform, after.PlayerID)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+banColumns+` FROM bans
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY platform, player_id
		  LIMIT ?`, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("read ban list: %w", err)
	}
	defer rows.Close()
	bans := make([]Ban, 0, min(limit, 128))
	for rows.Next() {
		ban, err := scanBan(rows)
		if err != nil {
			return 0, nil, fmt.Errorf("scan ban list entry: %w", err)
		}
		bans = append(bans, ban)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("read ban list: %w", err)
	}
	return revision, bans, nil
}
