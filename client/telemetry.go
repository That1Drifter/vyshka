package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// eventQueryValues renders an EventQuery. Types repeat the type parameter,
// one term each, and times travel as RFC 3339 in UTC.
func eventQueryValues(query EventQuery) url.Values {
	values := url.Values{}
	for _, term := range query.Types {
		values.Add("type", term)
	}
	if query.Since != nil {
		values.Set("since", query.Since.UTC().Format(time.RFC3339Nano))
	}
	if query.Until != nil {
		values.Set("until", query.Until.UTC().Format(time.RFC3339Nano))
	}
	if query.Limit > 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Cursor != "" {
		values.Set("cursor", query.Cursor)
	}
	return values
}

func pageQueryValues(query PageQuery) url.Values {
	values := url.Values{}
	if query.Limit > 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Cursor != "" {
		values.Set("cursor", query.Cursor)
	}
	return values
}

// ListEvents reads one page of a server's event feed, newest first.
func (c *Client) ListEvents(ctx context.Context, serverID string, query EventQuery) (EventPage, error) {
	var page EventPage
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "servers", serverID, "events"},
		eventQueryValues(query), nil, &page)
	return page, err
}

// GetState reads the latest snapshot of one type: players, vehicles,
// entities, or world. A snapshot can be stale; CapturedAt says how stale.
func (c *Client) GetState(ctx context.Context, serverID, stateType string) (StateSnapshot, error) {
	var snapshot StateSnapshot
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "servers", serverID, "state", stateType},
		nil, nil, &snapshot)
	return snapshot, err
}

// StateHistory reads recent snapshots of one type, newest first, the latest
// included. limit zero means the hub's default; the hub clamps a large one.
func (c *Client) StateHistory(ctx context.Context, serverID, stateType string, limit int) (StateHistoryPage, error) {
	var query url.Values
	if limit > 0 {
		query = url.Values{"limit": {strconv.Itoa(limit)}}
	}
	var page StateHistoryPage
	err := c.do(ctx, http.MethodGet,
		[]string{"api", "v1", "servers", serverID, "state", stateType, "history"}, query, nil, &page)
	return page, err
}

// PlayerEvents reads the events that refer to one identity across every
// server the token may read, newest first. The profile is as deep as event
// retention and no deeper.
func (c *Client) PlayerEvents(ctx context.Context, platform, playerID string, query EventQuery) (EventPage, error) {
	var page EventPage
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "players", platform, playerID, "events"},
		eventQueryValues(query), nil, &page)
	return page, err
}

// PlayerActions reads the actions dispatched against one identity (context
// player, referenceKey equal to the id) across every server, newest first.
func (c *Client) PlayerActions(ctx context.Context, platform, playerID string, query PageQuery) (ActionPage, error) {
	var page ActionPage
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "players", platform, playerID, "actions"},
		pageQueryValues(query), nil, &page)
	return page, err
}

// PlayerNotes reads the operator notes on one identity, newest first.
func (c *Client) PlayerNotes(ctx context.Context, platform, playerID string, query PageQuery) (NotePage, error) {
	var page NotePage
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "players", platform, playerID, "notes"},
		pageQueryValues(query), nil, &page)
	return page, err
}
