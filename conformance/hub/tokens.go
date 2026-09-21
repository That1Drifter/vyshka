package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Fixtures for scoped tokens and the audit log (spec section 10). Like every
// other shape in this suite they are hand-written rather than shared with the
// hub, so a change in the reference implementation cannot quietly change the
// contract being graded.

type tokenRecord struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	Servers   []string `json:"servers"`
	CreatedAt string   `json:"createdAt"`
	CreatedBy string   `json:"createdBy"`
	ExpiresAt *string  `json:"expiresAt"`
	RevokedAt *string  `json:"revokedAt"`
}

type mintedToken struct {
	Token tokenRecord `json:"token"`
	// Secret is returned by the mint response and never again.
	Secret string `json:"secret"`
}

// mintToken creates a scoped Admin API token with the suite's own credential.
func (e Env) mintToken(ctx context.Context, name string, scopes ...string) (mintedToken, error) {
	var minted mintedToken
	err := e.expect(ctx, http.MethodPost, "/api/v1/tokens", e.AdminToken,
		map[string]any{"name": name, "scopes": scopes}, http.StatusCreated, &minted)
	if err != nil {
		return mintedToken{}, err
	}
	if minted.Secret == "" {
		return mintedToken{}, fmt.Errorf("mint %q: response carried no secret", name)
	}
	if minted.Token.ID == "" {
		return mintedToken{}, fmt.Errorf("mint %q: response carried no token id", name)
	}
	return minted, nil
}

// mintBoundToken creates a token confined to the given servers (spec section
// 10.1) with the suite's own credential.
func (e Env) mintBoundToken(ctx context.Context, name string, servers []string, scopes ...string) (mintedToken, error) {
	var minted mintedToken
	err := e.expect(ctx, http.MethodPost, "/api/v1/tokens", e.AdminToken,
		map[string]any{"name": name, "scopes": scopes, "servers": servers}, http.StatusCreated, &minted)
	if err != nil {
		return mintedToken{}, err
	}
	if minted.Secret == "" {
		return mintedToken{}, fmt.Errorf("mint %q: response carried no secret", name)
	}
	return minted, nil
}

// headersOnlyDeadline is how long the stalled-body probe waits for the final
// refusal. A hub that judges the binding at the headers answers in
// milliseconds; one that first waits for the body waits for bytes that never
// come until its own body-read timeout.
const headersOnlyDeadline = 3 * time.Second

// refusedBeforeBody sends a mutation's request line and headers, declaring a
// body it never sends, and requires the hub's final 403 with the protocol's
// forbidden envelope within headersOnlyDeadline. It is a bounded stalled-body
// check: it proves the refusal does not wait on the body for longer than the
// deadline, which is what section 10.2 asks for from the caller's side (a
// refused token must not occupy the connection delivering a payload nothing
// will use). It cannot, from outside, tell a hub that never reads the body
// from one that gives up reading it within the deadline and then refuses;
// the audit assertion beside it (an empty payload digest on the refusal)
// covers what was digested, not what was read. The connection is raw so that
// nothing in an HTTP client library supplies or expects the body, and the
// request target is built from the same full URL ordinary requests use, so a
// hub mounted under a path prefix is reached.
func (e Env) refusedBeforeBody(ctx context.Context, method, path, bearer string) error {
	target, err := url.Parse(e.BaseURL + path)
	if err != nil {
		return fmt.Errorf("parse the hub URL: %w", err)
	}
	host := target.Host
	if target.Port() == "" {
		if target.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	deadline := time.Now().Add(headersOnlyDeadline)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := net.Dialer{Deadline: deadline}
	var conn net.Conn
	if target.Scheme == "https" {
		conn, err = tls.DialWithDialer(&dialer, "tcp", host, &tls.Config{ServerName: target.Hostname()})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", host, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	request := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n",
		method, target.RequestURI(), target.Host, bearer)
	if _, err := io.WriteString(conn, request); err != nil {
		return fmt.Errorf("send the headers of %s %s: %w", method, path, err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	// An informational response is not the final one; read past it.
	for err == nil && resp.StatusCode >= 100 && resp.StatusCode < 200 {
		resp, err = http.ReadResponse(reader, nil)
	}
	if err != nil {
		return fmt.Errorf("%s %s with the body withheld: no final response within %s; a token the binding refuses must not be left occupying the connection waiting for a body (section 10.2): %w", method, path, headersOnlyDeadline, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		return fmt.Errorf("%s %s with the body withheld: status = %d, want 403 at the headers", method, path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("%s %s with the body withheld: read the refusal: %w", method, path, err)
	}
	return assertErrorCode(method, path, body, "forbidden")
}

// keySuffix turns any identifier into 16 hexadecimal characters that fit the
// key/value key alphabet of spec section 12.1, whatever alphabet the hub's
// own ids use, so a probe key built from a token id is a valid key on every
// hub.
func keySuffix(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

// containsString walks a decoded JSON value and reports the path of the first
// string that contains needle, or "" when none does. A refusal must not name
// what it withholds anywhere in its body, `details` included.
func containsString(value any, needle, path string) string {
	switch v := value.(type) {
	case string:
		if strings.Contains(v, needle) {
			return path
		}
	case map[string]any:
		for k, child := range v {
			// A member name is a string the client decodes like any other.
			if strings.Contains(k, needle) {
				return path + "." + k
			}
			if where := containsString(child, needle, path+"."+k); where != "" {
				return where
			}
		}
	case []any:
		for i, child := range v {
			if where := containsString(child, needle, fmt.Sprintf("%s[%d]", path, i)); where != "" {
				return where
			}
		}
	}
	return ""
}

// syntheticServerIDs makes n distinct well-formed ids that name no server,
// for the oversized-binding refusal: the size check must come before any
// lookup could answer not_found.
func syntheticServerIDs(n int) []string {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, fmt.Sprintf("01J000000000000000000%05d", i))
	}
	return ids
}

func (e Env) listTokens(ctx context.Context) ([]tokenRecord, error) {
	var listed struct {
		Tokens []tokenRecord `json:"tokens"`
	}
	err := e.expect(ctx, http.MethodGet, "/api/v1/tokens", e.AdminToken,
		nil, http.StatusOK, &listed)
	return listed.Tokens, err
}

type auditRecord struct {
	ID            string          `json:"id"`
	At            string          `json:"at"`
	TokenID       string          `json:"tokenId"`
	TokenName     string          `json:"tokenName"`
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Status        int             `json:"status"`
	SourceIP      string          `json:"sourceIp"`
	PayloadDigest string          `json:"payloadDigest"`
	ServerID      string          `json:"serverId"`
	Detail        json.RawMessage `json:"detail"`
}

type auditPage struct {
	Records    []auditRecord `json:"records"`
	NextCursor string        `json:"nextCursor"`
}

// auditRecords reads one page of the audit log.
func (e Env) auditRecords(ctx context.Context, parameters url.Values) (auditPage, error) {
	path := "/api/v1/audit"
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page auditPage
	err := e.expect(ctx, http.MethodGet, path, e.AdminToken, nil, http.StatusOK, &page)
	return page, err
}

// refused asserts that a request made with a scoped token is answered 403 with
// the protocol's forbidden code, which is what separates "your token may not"
// from "no such thing" (spec section 2.2).
func (e Env) refused(ctx context.Context, method, path, bearer string, body any, why string) error {
	if err := e.expectError(ctx, method, path, bearer, body, http.StatusForbidden, "forbidden"); err != nil {
		return fmt.Errorf("%s: %w", why, err)
	}
	return nil
}
