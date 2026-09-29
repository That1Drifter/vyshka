package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// testToken is the bootstrap admin credential every test hub boots with.
const testToken = "vya_CLITESTTOKENCLITESTTOKEN0"

func TestMain(m *testing.M) {
	// --wait reads the action every half second in real use; the tests
	// shorten that so a lifecycle costs milliseconds per step. Set once,
	// before any test runs, so parallel tests never race on it.
	waitInterval = 20 * time.Millisecond
	os.Exit(m.Run())
}

// testHub is a real hub, booted in-process and served over HTTP, which the
// command under test reaches exactly as it would reach a deployed one.
type testHub struct {
	t          *testing.T
	url        string
	configDir  string
	dispatches atomic.Int64
	// actionReadDelay, in milliseconds, is how long every read of an action
	// record is held before the hub answers it: a slow hub, for proving
	// that a wait's deadline covers its first read too. actionReads counts
	// those reads, for proving a record is not read twice.
	actionReadDelay atomic.Int64
	actionReads     atomic.Int64
}

func newTestHub(t *testing.T) *testHub {
	t.Helper()
	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: filepath.Join(t.TempDir(), "hub.db"),
		AdminToken:  testToken,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })

	h := &testHub{t: t, configDir: t.TempDir()}
	handler := server.Handler()
	// Counting dispatches at the door is how a test proves a locally refused
	// run sent nothing at all.
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions") {
			h.dispatches.Add(1)
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v1/actions/") {
			h.actionReads.Add(1)
			if delay := h.actionReadDelay.Load(); delay > 0 {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	h.url = httpServer.URL
	return h
}

// syncBuffer is a bytes.Buffer safe to read while a command still writes to
// it from another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type result struct {
	code           int
	stdout, stderr string
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\n--- stdout\n%s--- stderr\n%s", r.code, r.stdout, r.stderr)
}

// runOptions varies one run of the command.
type runOptions struct {
	// env replaces variables of the default environment (the hub's URL and
	// the admin token); an empty value unsets one.
	env      map[string]string
	stdin    string
	terminal bool
	ctx      context.Context
	stdout   *syncBuffer
	stderr   *syncBuffer
}

func (h *testHub) run(args ...string) result {
	return h.runWith(runOptions{}, args...)
}

func (h *testHub) runWith(options runOptions, args ...string) result {
	env := map[string]string{"VYSHKA_URL": h.url, "VYSHKA_TOKEN": testToken}
	for key, value := range options.env {
		env[key] = value
	}
	stdout, stderr := options.stdout, options.stderr
	if stdout == nil {
		stdout = &syncBuffer{}
	}
	if stderr == nil {
		stderr = &syncBuffer{}
	}
	code := Run(args, IO{
		Stdin:           strings.NewReader(options.stdin),
		Stdout:          stdout,
		Stderr:          stderr,
		Getenv:          func(key string) string { return env[key] },
		StdinIsTerminal: options.terminal,
		ConfigDir:       func() (string, error) { return h.configDir, nil },
		Context:         options.ctx,
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// background runs the command on another goroutine and hands back a channel
// for its result.
func (h *testHub) background(options runOptions, args ...string) <-chan result {
	done := make(chan result, 1)
	go func() { done <- h.runWith(options, args...) }()
	return done
}

func await(t *testing.T, done <-chan result, within time.Duration) result {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(within):
		t.Fatalf("the command did not finish within %s", within)
		return result{}
	}
}

// eventually polls a condition, which is how these tests wait: on what they
// are waiting for, never on a fixed sleep.
func eventually(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// call sends one JSON request to the hub over HTTP and decodes the answer.
func (h *testHub) call(method, path, bearer string, body, out any) int {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, h.url+path, reader)
	if err != nil {
		h.t.Fatalf("request %s %s: %v", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		h.t.Fatalf("read %s %s: %v", method, path, err)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			h.t.Fatalf("decode %s %s answer %q: %v", method, path, data, err)
		}
	}
	return response.StatusCode
}

func (h *testHub) mustCall(method, path, bearer string, body, out any, want int) {
	h.t.Helper()
	if status := h.call(method, path, bearer, body, out); status != want {
		h.t.Fatalf("%s %s: status %d, want %d", method, path, status, want)
	}
}

// createServer makes a server record through the Admin API directly.
func (h *testHub) createServer(name string) (id, enrollmentToken string) {
	h.t.Helper()
	var created struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
		Enrollment struct {
			Token string `json:"token"`
		} `json:"enrollment"`
	}
	h.mustCall(http.MethodPost, "/api/v1/servers", testToken,
		map[string]any{"name": name, "game": "test-game"}, &created, http.StatusCreated)
	return created.Server.ID, created.Enrollment.Token
}

// fakePlugin plays the game server's side over plain HTTP: enroll, a
// session, and polls. It never acks: one envelope queued for it at the start
// and never acked keeps every poll answered at once (spec section 3.1.2), so
// no test waits out a hold, while the envelopes each poll carries are still
// ingested.
type fakePlugin struct {
	h        *testHub
	serverID string
	session  string

	mu      sync.Mutex
	seq     int64
	handled map[string]bool
}

type wireEnvelope struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Body json.RawMessage `json:"body"`
}

type dispatchBody struct {
	ActionID     string         `json:"actionId"`
	Code         string         `json:"code"`
	Context      string         `json:"context"`
	ReferenceKey string         `json:"referenceKey"`
	Params       map[string]any `json:"params"`
}

func (h *testHub) plugin(name string) *fakePlugin {
	h.t.Helper()
	id, enrollmentToken := h.createServer(name)
	return h.enroll(id, enrollmentToken)
}

func (h *testHub) enroll(serverID, enrollmentToken string) *fakePlugin {
	h.t.Helper()
	identity := map[string]string{"name": "cli-test-plugin", "version": "0.0.1"}
	var enrolled struct {
		ServerSecret string `json:"serverSecret"`
	}
	h.mustCall(http.MethodPost, "/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": enrollmentToken, "game": "test-game",
		"plugin": identity, "transports": []string{"poll"},
	}, &enrolled, http.StatusCreated)

	var session struct {
		SessionToken string `json:"sessionToken"`
	}
	h.mustCall(http.MethodPost, "/plugin/v1/session", "", map[string]any{
		"serverId": serverID, "serverSecret": enrolled.ServerSecret,
		"pollTimeoutSeconds": 5, "plugin": identity, "transports": []string{"poll"},
	}, &session, http.StatusOK)

	h.mustCall(http.MethodPost, "/api/v1/servers/"+serverID+"/envelopes", testToken,
		map[string]any{"type": "cli-test.nudge"}, nil, http.StatusAccepted)

	return &fakePlugin{h: h, serverID: serverID, session: session.SessionToken, handled: map[string]bool{}}
}

func (p *fakePlugin) envelope(envelopeType string, body any) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return map[string]any{
		"v": 1, "id": fmt.Sprintf("cli-test-%s-%d", p.serverID, p.seq),
		"type": envelopeType, "seq": p.seq,
		"ts": time.Now().UTC().Format(time.RFC3339Nano), "body": body,
	}
}

func (p *fakePlugin) poll(envelopes ...map[string]any) []wireEnvelope {
	p.h.t.Helper()
	if envelopes == nil {
		envelopes = []map[string]any{}
	}
	var answer struct {
		Envelopes []wireEnvelope `json:"envelopes"`
		Ack       int64          `json:"ack"`
	}
	start := time.Now()
	p.h.mustCall(http.MethodPost, "/plugin/v1/poll", p.session,
		map[string]any{"envelopes": envelopes}, &answer, http.StatusOK)
	if held := time.Since(start); held > 2*time.Second {
		p.h.t.Errorf("a poll was held for %s; the nudge should keep it immediate", held)
	}
	return answer.Envelopes
}

func (p *fakePlugin) publishManifest(manifest map[string]any) {
	p.h.t.Helper()
	p.poll(p.envelope("manifest.publish", manifest))
	var stored struct {
		Revision int64 `json:"revision"`
	}
	p.h.mustCall(http.MethodGet, "/api/v1/servers/"+p.serverID+"/manifest", testToken, nil, &stored, http.StatusOK)
}

func (p *fakePlugin) pushPlayers(capturedAt time.Time, players ...map[string]any) {
	p.h.t.Helper()
	if players == nil {
		players = []map[string]any{}
	}
	p.poll(p.envelope("state.players", map[string]any{
		"capturedAt": capturedAt.UTC().Format(time.RFC3339Nano), "players": players,
	}))
}

func (p *fakePlugin) pushEvents(events ...map[string]any) {
	p.h.t.Helper()
	p.poll(p.envelope("event.batch", map[string]any{"events": events}))
}

// nextDispatch polls until an action.dispatch this plugin has not handled
// yet arrives.
func (p *fakePlugin) nextDispatch() dispatchBody {
	p.h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, envelope := range p.poll() {
			if envelope.Type != "action.dispatch" {
				continue
			}
			var body dispatchBody
			if err := json.Unmarshal(envelope.Body, &body); err != nil {
				p.h.t.Fatalf("decode action.dispatch: %v", err)
			}
			p.mu.Lock()
			fresh := !p.handled[body.ActionID]
			p.handled[body.ActionID] = true
			p.mu.Unlock()
			if fresh {
				return body
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.h.t.Fatal("no action.dispatch arrived within 5s")
	return dispatchBody{}
}

// finish reports an action as the plugin would: action.ack, then
// action.result.
func (p *fakePlugin) finish(actionID string, ok bool, payload any, failure string) {
	p.h.t.Helper()
	body := map[string]any{"actionId": actionID, "ok": ok, "durationMs": 7}
	if payload != nil {
		body["result"] = payload
	}
	if failure != "" {
		body["error"] = failure
	}
	p.poll(p.envelope("action.ack", map[string]any{"actionId": actionID}), p.envelope("action.result", body))
}

// testManifest declares one action of each shape the command treats
// differently.
func testManifest(revision int64) map[string]any {
	return map[string]any{
		"manifestRevision": revision,
		"contexts":         []map[string]any{{"id": "territory", "name": "Territory"}},
		"kvNamespaces":     []string{"example-mod"},
		"actions": []map[string]any{
			{
				"code": "example-mod.heal", "name": "Heal player", "context": "player",
				"namespace": "example-mod", "danger": "warning",
				"params": map[string]any{
					"type": "object", "required": []string{"amount"},
					"properties": map[string]any{
						"amount": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
						"reason": map[string]any{"type": "string"},
					},
				},
			},
			{
				"code": "example-mod.teleport", "name": "Teleport", "context": "player",
				"namespace": "example-mod",
				"params": map[string]any{
					"type": "object", "required": []string{"position"},
					"properties": map[string]any{
						"position": map[string]any{"type": "array", "items": map[string]any{"type": "number"},
							"x-vyshka-widget": "vector"},
					},
				},
			},
			{
				"code": "example-mod.wipe", "name": "Wipe the map", "context": "world",
				"namespace": "example-mod", "danger": "destructive",
			},
			{
				"code": "example-mod.ping", "name": "Ping", "context": "world", "namespace": "example-mod",
			},
		},
	}
}

// actionIDFrom reads the id out of run's human output.
func actionIDFrom(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(line, "actionId:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("no actionId line in %q", stdout)
	return ""
}
