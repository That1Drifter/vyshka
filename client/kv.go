package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// KVGet reads one key. An absent or expired key is the hub's not_found.
func (c *Client) KVGet(ctx context.Context, namespace, key string) (KVEntry, error) {
	var entry KVEntry
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "kv", namespace, key}, nil, nil, &entry)
	return entry, err
}

// KVSet writes one key. With IfRevision set the write is a compare-and-swap,
// and a mismatch is the hub's revision_mismatch with details.revision
// carrying the current revision (0 when the key does not exist). A json.RawMessage
// Value travels byte for byte.
func (c *Client) KVSet(ctx context.Context, namespace, key string, request KVSetRequest) (KVWriteResult, error) {
	var result KVWriteResult
	err := c.do(ctx, http.MethodPut, []string{"api", "v1", "kv", namespace, key}, nil, request, &result)
	return result, err
}

// KVDelete deletes one key. An absent key is the hub's not_found, returned as
// the *Error; a caller retrying a delete treats it as success (spec section
// 12.2).
func (c *Client) KVDelete(ctx context.Context, namespace, key string) error {
	return c.do(ctx, http.MethodDelete, []string{"api", "v1", "kv", namespace, key}, nil, nil, nil)
}

// KVIncr atomically adds delta to an integer key, creating it at delta when
// absent. A nil delta sends no body, which the hub reads as 1.
func (c *Client) KVIncr(ctx context.Context, namespace, key string, delta *int64) (KVEntry, error) {
	var body any
	if delta != nil {
		body = map[string]int64{"delta": *delta}
	}
	var entry KVEntry
	err := c.do(ctx, http.MethodPost, []string{"api", "v1", "kv", namespace, key, "incr"}, nil, body, &entry)
	return entry, err
}

// KVListKeys reads one page of a namespace's live keys, key ascending.
func (c *Client) KVListKeys(ctx context.Context, namespace string, query KVListQuery) (KVKeyPage, error) {
	values := url.Values{}
	if query.Prefix != "" {
		values.Set("prefix", query.Prefix)
	}
	if query.Limit > 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Cursor != "" {
		values.Set("cursor", query.Cursor)
	}
	var page KVKeyPage
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "kv", namespace}, values, nil, &page)
	return page, err
}

// KVListNamespaces reads the namespaces holding at least one live key that
// the token's grants cover, with their key counts. It is not paged.
func (c *Client) KVListNamespaces(ctx context.Context) (KVNamespaceList, error) {
	var list KVNamespaceList
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "kv"}, nil, nil, &list)
	return list, err
}
