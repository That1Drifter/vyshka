package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// ListServers reads every server record the token may see, newest first.
func (c *Client) ListServers(ctx context.Context) (ServerList, error) {
	var list ServerList
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "servers"}, nil, nil, &list)
	return list, err
}

// GetServer reads one server record.
func (c *Client) GetServer(ctx context.Context, serverID string) (Server, error) {
	var server Server
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "servers", serverID}, nil, nil, &server)
	return server, err
}

// CreateServer creates a server record and mints its first enrollment token,
// which is returned here and never again.
func (c *Client) CreateServer(ctx context.Context, request CreateServerRequest) (CreatedServer, error) {
	var created CreatedServer
	err := c.do(ctx, http.MethodPost, []string{"api", "v1", "servers"}, nil, request, &created)
	return created, err
}

// IssueEnrollmentToken mints a replacement enrollment token, invalidating any
// unused one the server still had. ttlSeconds zero means the hub's default.
func (c *Client) IssueEnrollmentToken(ctx context.Context, serverID string, ttlSeconds int) (EnrollmentToken, error) {
	var body any
	if ttlSeconds > 0 {
		body = map[string]int{"ttlSeconds": ttlSeconds}
	}
	var token EnrollmentToken
	err := c.do(ctx, http.MethodPost, []string{"api", "v1", "servers", serverID, "enrollment-token"}, nil, body, &token)
	return token, err
}

// RevokeServerCredentials revokes a server's secret and ends its sessions at
// once. Idempotent.
func (c *Client) RevokeServerCredentials(ctx context.Context, serverID string) error {
	return c.do(ctx, http.MethodDelete, []string{"api", "v1", "servers", serverID, "credentials"}, nil, nil, nil)
}

// GetManifest reads a server's stored manifest. A server that never had a
// manifest accepted answers not_found, like an unknown server.
func (c *Client) GetManifest(ctx context.Context, serverID string) (ManifestRecord, error) {
	var record ManifestRecord
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "servers", serverID, "manifest"}, nil, nil, &record)
	return record, err
}

// EnumerateContext asks for the members of one custom context the server's
// manifest declares. The hub answers from a short cache unless refresh is
// set, and otherwise holds the request while it asks the plugin.
func (c *Client) EnumerateContext(ctx context.Context, serverID, contextID string, refresh bool) (ContextEntries, error) {
	var query url.Values
	if refresh {
		query = url.Values{"refresh": {strconv.FormatBool(true)}}
	}
	var entries ContextEntries
	err := c.do(ctx, http.MethodGet,
		[]string{"api", "v1", "servers", serverID, "contexts", contextID, "entries"}, query, nil, &entries)
	return entries, err
}
