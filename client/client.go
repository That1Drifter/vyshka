// Package client is a Go client for the Admin API of a Vyshka hub, the
// operator-facing half of the protocol (spec/protocol.md, with
// spec/openapi-admin.yaml as its machine-readable companion).
//
// It is hand-written and covers what the vyshka command needs: health, server
// records and enrollment tokens, manifests, context enumeration, action
// dispatch and observation, the event feed, state snapshots, the key/value
// store, and player profiles. Tokens, the audit log, bans, and webhooks are
// out of scope for now; issue 96 tracks a full generated client.
//
// Every method takes a context first. A refusal from the hub comes back as
// *Error, carrying the protocol error code to branch on; a failure to reach
// the hub, or an answer that cannot be read, comes back as *TransportError.
// Use errors.As to tell them apart.
//
// Records tolerate what a newer hub adds: unknown JSON members are ignored and
// enum-like strings (action states, danger levels, link states) are passed
// through verbatim rather than checked against the values this draft names.
// Each record also keeps the JSON the hub sent in its Raw field, for callers
// that want to pass the hub's own object on unchanged.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Version is the build version of this client and of the vyshka command
// built on it. Release builds set it with
// -ldflags "-X github.com/That1Drifter/vyshka/client.Version=v0.3.0".
var Version = "dev"

// ProtocolDraft is the draft of spec/protocol.md this client was written
// against. A newer hub stays usable, because every record here tolerates
// fields and values it does not know, but anything a later draft added is
// invisible to this client.
const ProtocolDraft = "0.33"

// defaultTimeout bounds one request when the caller supplies no HTTP client.
// The longest Admin API answer is a context enumeration, which the hub holds
// for about 10 s, so 30 s leaves room without letting a dead hub hang a
// script.
const defaultTimeout = 30 * time.Second

// maxResponseBytes caps how much of one answer is read, as a guard against
// something that is not a hub answering without end. It sits far above any
// legal page: the largest is a page of full action records, up to 500 of
// them each near the hub's 1 MiB request cap, and a caller asking for one
// gets it.
const maxResponseBytes = 1 << 30

// Client talks to one hub with one token. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	token     string
	http      *http.Client
	userAgent string
}

// Option adjusts a Client at construction.
type Option func(*Client)

// WithHTTPClient replaces the default HTTP client (30 s timeout, redirects
// not followed). The caller's client is used as given, redirect policy
// included.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		if httpClient != nil {
			c.http = httpClient
		}
	}
}

// WithUserAgent sets the User-Agent header every request carries.
func WithUserAgent(userAgent string) Option {
	return func(c *Client) { c.userAgent = userAgent }
}

// New returns a client for the hub at baseURL (such as
// http://127.0.0.1:8080, or a path prefix behind a reverse proxy) that
// authenticates with token. Neither may be empty, and the URL must be http or
// https.
func New(baseURL, token string, opts ...Option) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, errors.New("client: the hub URL is empty")
	}
	if token == "" {
		return nil, errors.New("client: the token is empty")
	}
	// A token with whitespace or a control character in it cannot travel in a
	// header. Saying so here, without echoing it, beats the transport's
	// refusal, and keeps a pasted newline from reaching the wire at all.
	if strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return nil, errors.New("client: the token contains whitespace or a control character")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("client: the hub URL does not parse: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("client: the hub URL must start with http:// or https://, not %q", parsed.Scheme+":")
	}
	if parsed.Host == "" {
		return nil, errors.New("client: the hub URL names no host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("client: the hub URL must not carry a query or a fragment")
	}

	c := &Client{
		base:  parsed,
		token: token,
		http: &http.Client{
			Timeout: defaultTimeout,
			// An API client that follows redirects turns a POST into a GET and
			// a misconfigured URL into a confusing answer from somewhere else;
			// the redirect is reported as the hub's answer instead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		userAgent: "vyshka-client/" + Version,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Error is a refusal from the hub: any non-2xx answer. Code is the protocol
// error code (spec section 2.2), stable and meant to be branched on; Message
// is for people. An answer that did not carry the protocol error shape (a
// reverse proxy's error page, say) has an empty Code and the HTTP status text
// as its Message.
type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("%s (%d)", e.Message, e.Status)
	}
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// TransportError is a failure below the protocol: the hub could not be
// reached, the request timed out, or a successful answer could not be read.
// Op names the request.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string { return e.Op + ": " + e.Err.Error() }

// Unwrap exposes the cause, so errors.Is can find context.DeadlineExceeded
// and the like.
func (e *TransportError) Unwrap() error { return e.Err }

// IsNotFound reports whether err is the hub's not_found refusal.
func IsNotFound(err error) bool {
	var refusal *Error
	return errors.As(err, &refusal) && refusal.Code == "not_found"
}

// escapeSegment renders one path segment. Dot segments are the special case:
// url.PathEscape leaves "." and ".." alone, and a URL library or proxy would
// then remove them before the request left, so they travel percent-encoded,
// which spec section 8.6 requires a hub to read as the member they encode.
func escapeSegment(segment string) string {
	switch segment {
	case ".":
		return "%2e"
	case "..":
		return "%2e%2e"
	}
	return url.PathEscape(segment)
}

// endpoint builds the URL of one request. Path carries the segments as they
// are and RawPath as they travel, so net/http sends exactly the escaped form
// built here rather than re-deriving one of its own.
func (c *Client) endpoint(segments []string, query url.Values) (*url.URL, error) {
	target := *c.base
	var plain, escaped strings.Builder
	basePlain, baseEscaped := c.base.Path, c.base.EscapedPath()
	// A trailing slash is dropped from both forms together, and only when it
	// is a slash in the escaped form too: a prefix ending in %2F decodes to a
	// slash that belongs to its last segment, not to a separator, and
	// trimming one form alone would leave the two disagreeing, at which point
	// net/http falls back to the decoded path and the prefix is lost.
	if strings.HasSuffix(baseEscaped, "/") {
		basePlain, baseEscaped = strings.TrimSuffix(basePlain, "/"), strings.TrimSuffix(baseEscaped, "/")
	}
	plain.WriteString(basePlain)
	escaped.WriteString(baseEscaped)
	for _, segment := range segments {
		if segment == "" {
			return nil, errors.New("client: a path parameter is empty")
		}
		plain.WriteString("/")
		plain.WriteString(segment)
		escaped.WriteString("/")
		escaped.WriteString(escapeSegment(segment))
	}
	target.Path = plain.String()
	target.RawPath = escaped.String()
	target.RawQuery = ""
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}
	return &target, nil
}

// answer is one response, read whole.
type answer struct {
	status int
	header http.Header
	body   []byte
	op     string
}

// send performs one request and reads the answer. It fails only below the
// protocol; a non-2xx status is the caller's to interpret.
func (c *Client) send(ctx context.Context, method string, segments []string, query url.Values, body any) (answer, error) {
	target, err := c.endpoint(segments, query)
	if err != nil {
		return answer{}, err
	}
	op := method + " " + target.Redacted()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return answer{}, fmt.Errorf("client: encoding the request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return answer{}, &TransportError{Op: op, Err: err}
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		request.Header.Set("User-Agent", c.userAgent)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		// url.Error repeats the method and URL that Op already carries.
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return answer{}, &TransportError{Op: op, Err: err}
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return answer{}, &TransportError{Op: op, Err: fmt.Errorf("reading the answer: %w", err)}
	}
	if len(data) > maxResponseBytes {
		return answer{}, &TransportError{Op: op, Err: fmt.Errorf("the answer exceeds %d bytes", maxResponseBytes)}
	}
	return answer{status: response.StatusCode, header: response.Header, body: data, op: op}, nil
}

// do performs one request and decodes a 2xx answer into out, when out is not
// nil. Anything else becomes an *Error.
func (c *Client) do(ctx context.Context, method string, segments []string, query url.Values, body, out any) error {
	got, err := c.send(ctx, method, segments, query, body)
	if err != nil {
		return err
	}
	if got.status < 200 || got.status > 299 {
		return refusal(got)
	}
	return decode(got, out)
}

func decode(got answer, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(got.body, out); err != nil {
		return &TransportError{Op: got.op, Err: fmt.Errorf("the answer is not the JSON expected: %w", err)}
	}
	return nil
}

// refusal turns a non-2xx answer into an *Error, from the protocol error shape
// when the body carries one and from the status line when it does not.
func refusal(got answer) *Error {
	var shaped struct {
		Error *struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(got.body, &shaped) == nil && shaped.Error != nil && shaped.Error.Code != "" {
		return &Error{
			Status:  got.status,
			Code:    shaped.Error.Code,
			Message: shaped.Error.Message,
			Details: shaped.Error.Details,
		}
	}

	message := http.StatusText(got.status)
	if message == "" {
		message = "unexpected HTTP status"
	}
	if got.status >= 300 && got.status < 400 {
		if location := got.header.Get("Location"); location != "" {
			message += "; the hub redirected to " + location + ", which this client does not follow"
		}
	}
	return &Error{Status: got.status, Message: message}
}

// Health reads GET /healthz. A hub answering 503 still sends the health body,
// so a degraded hub returns that body together with an *Error whose Code is
// "degraded", letting a caller show both.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var health Health
	got, err := c.send(ctx, http.MethodGet, []string{"healthz"}, nil, nil)
	if err != nil {
		return health, err
	}
	switch got.status {
	case http.StatusOK:
		return health, decode(got, &health)
	case http.StatusServiceUnavailable:
		if json.Unmarshal(got.body, &health) == nil && health.Status != "" {
			message := "the hub reports status " + health.Status
			if health.Database.Error != "" {
				message += "; database: " + health.Database.Error
			}
			return health, &Error{Status: got.status, Code: "degraded", Message: message}
		}
	}
	return Health{}, refusal(got)
}

// jsonUnmarshalRaw decodes data into value and keeps a copy of data in raw.
// It backs the UnmarshalJSON methods of every record with a Raw field.
func jsonUnmarshalRaw(data []byte, value any, raw *json.RawMessage) error {
	if err := json.Unmarshal(data, value); err != nil {
		return err
	}
	*raw = append(json.RawMessage(nil), data...)
	return nil
}
