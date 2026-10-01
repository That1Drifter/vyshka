package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// ListBans reads one page of the installation ban list, newest first: the
// active bans by default, everything with State "all", and one identity's
// history with Player set. Requires bans:read.
func (c *Client) ListBans(ctx context.Context, query BanQuery) (BanPage, error) {
	values := url.Values{}
	if query.State != "" {
		values.Set("state", query.State)
	}
	if query.Player != nil {
		values.Set("platform", query.Player.Platform)
		values.Set("playerId", query.Player.ID)
	}
	if query.Limit > 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Cursor != "" {
		values.Set("cursor", query.Cursor)
	}
	var page BanPage
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "bans"}, values, nil, &page)
	return page, err
}

// CreateBan bans an identity on every server of the installation. An
// identity that already carries an active ban is the hub's conflict, with
// details.banId naming that ban. Requires bans:manage.
func (c *Client) CreateBan(ctx context.Context, request CreateBanRequest) (BanChange, error) {
	var change BanChange
	err := c.do(ctx, http.MethodPost, []string{"api", "v1", "bans"}, nil, request, &change)
	return change, err
}

// GetBan reads one ban record. Requires bans:read.
func (c *Client) GetBan(ctx context.Context, banID string) (Ban, error) {
	var answer struct {
		Ban Ban `json:"ban"`
	}
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "bans", banID}, nil, nil, &answer)
	return answer.Ban, err
}

// LiftBan lifts one ban. A ban already lifted or expired is answered as it
// stands, so a retried lift is harmless. Requires bans:manage.
func (c *Client) LiftBan(ctx context.Context, banID string) (BanChange, error) {
	var change BanChange
	err := c.do(ctx, http.MethodPost, []string{"api", "v1", "bans", banID, "lift"}, nil, nil, &change)
	return change, err
}
