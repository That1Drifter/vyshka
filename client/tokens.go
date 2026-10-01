package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ListTokens reads every Admin API token record, newest first, revoked and
// expired ones included. Requires admin.
func (c *Client) ListTokens(ctx context.Context) (TokenList, error) {
	var list TokenList
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "tokens"}, nil, nil, &list)
	return list, err
}

// CreateToken mints a scoped token. The secret in the answer is never
// available again. Requires admin, and the hub refuses a scope the minting
// token does not hold itself.
func (c *Client) CreateToken(ctx context.Context, request CreateTokenRequest) (CreatedToken, error) {
	var created CreatedToken
	err := c.do(ctx, http.MethodPost, []string{"api", "v1", "tokens"}, nil, request, &created)
	return created, err
}

// RevokeToken revokes a token from its next request on. The record
// survives, and revoking twice is harmless. Requires admin.
func (c *Client) RevokeToken(ctx context.Context, tokenID string) error {
	return c.do(ctx, http.MethodDelete, []string{"api", "v1", "tokens", tokenID}, nil, nil, nil)
}

// ListAudit reads one page of the audit log, newest first. Requires admin.
func (c *Client) ListAudit(ctx context.Context, query AuditQuery) (AuditPage, error) {
	values := url.Values{}
	if query.TokenID != "" {
		values.Set("tokenId", query.TokenID)
	}
	if query.ServerID != "" {
		values.Set("serverId", query.ServerID)
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
	var page AuditPage
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "audit"}, values, nil, &page)
	return page, err
}
