package panel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/That1Drifter/vyshka/hub"
	"github.com/That1Drifter/vyshka/panel"
)

// The scaffolding both browser tests stand on: a real hub with the embedded
// panel, a headless Chromium-family browser pointed at it, a console capture
// for the failure report, and the step helpers every assertion is written in
// terms of. TestPanelEndToEnd (issue #13, #47, #46) and
// TestPanelManagementEndToEnd (issue #64) differ in what they drive, not in
// how they drive it, so this lives here and neither test owns it.
//
// Both need a Chromium-family browser. CI has one and sets VYSHKA_E2E=required
// so a missing browser fails the job instead of skipping the test; anywhere
// else the tests skip themselves when they find none. VYSHKA_E2E_BROWSER names
// the executable explicitly.

const e2eAdminToken = "vya_E2ETOKENE2ETOKENE2ETOKENE2"

// e2eHarness is one browser driving one hub. Every helper on it reports a
// failure the same way: the message, the page's text, and whatever the page
// logged or threw, because a bare "selector never appeared" says nothing about
// which of the two sides went wrong.
type e2eHarness struct {
	t *testing.T
	// Hub is the reference hub under the browser, and web the HTTP server in
	// front of it; the tests reach both through web.URL.
	Hub *hub.Server
	web *httptest.Server
	// Ctx bounds the whole test, Page is the browser tab. A step's own
	// deadline is taken from Page, well inside Ctx, so a step that hangs
	// still leaves time to read the page for the report.
	Ctx  context.Context
	Page context.Context

	consoleMu sync.Mutex
	console   []string
}

// newE2EHarness boots the hub with the panel and starts the browser. mapsDir
// is the tileset directory the map view reads, empty for a hub with no
// imagery installed.
func newE2EHarness(t *testing.T, mapsDir string) *e2eHarness {
	t.Helper()
	browser := findBrowser(t)

	server, err := hub.New(context.Background(), hub.Config{
		DatabaseURL: filepath.Join(t.TempDir(), "e2e.db"),
		AdminToken:  e2eAdminToken,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Panel:       panel.NewHandler(panel.Config{MapsDir: mapsDir}),
		// Long enough that every form the test opens after the first is
		// served from the cache, so the count of questions the fake plugin
		// answered is a fact about the panel and not about the clock.
		ContextCacheTTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("boot hub: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	web := httptest.NewServer(server.Handler())
	t.Cleanup(web.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	// Two bounds apply to the browser's start alone, and chromedp's defaults
	// for both are too short on a CI runner where every package's tests run
	// at once: 20 s for the browser to print its DevTools address and 10 s to
	// dial it. A whole start that takes 0.25 s idle took 12.4 to 20.5 s beside
	// a CPU burner, one CI start lost the first bound (issue #125), and a
	// heavier burner lost the second. Neither bounds anything the panel does.
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, append(
		chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(browser),
		chromedp.WSURLReadTimeout(90*time.Second))...)
	t.Cleanup(cancelAlloc)
	page, cancelPage := chromedp.NewContext(allocCtx,
		chromedp.WithBrowserOption(chromedp.WithDialTimeout(90*time.Second)))
	t.Cleanup(cancelPage)
	// The browser's lifetime is tied to the context of the first Run, so it
	// is started here on the tab context rather than under a step's deadline.
	started := time.Now()
	if err := chromedp.Run(page); err != nil {
		t.Fatalf("start %s after %v: %v", browser, time.Since(started).Round(time.Millisecond), err)
	}
	t.Logf("started %s in %v", browser, time.Since(started).Round(time.Millisecond))

	h := &e2eHarness{t: t, Hub: server, web: web, Ctx: ctx, Page: page}
	// Anything the page logs or throws is kept for the failure report, which
	// otherwise says only that a selector never appeared.
	chromedp.ListenTarget(page, func(event any) {
		switch e := event.(type) {
		case *cdpruntime.EventExceptionThrown:
			h.consoleMu.Lock()
			h.console = append(h.console, "exception: "+e.ExceptionDetails.Error())
			h.consoleMu.Unlock()
		case *cdpruntime.EventConsoleAPICalled:
			var parts []string
			for _, arg := range e.Args {
				parts = append(parts, string(arg.Value))
			}
			h.consoleMu.Lock()
			h.console = append(h.console, string(e.Type)+": "+strings.Join(parts, " "))
			h.consoleMu.Unlock()
		}
	})
	return h
}

// URL is where the hub, and so the panel, is reachable.
func (h *e2eHarness) URL() string { return h.web.URL }

func (h *e2eHarness) fail(format string, args ...any) {
	h.t.Helper()
	var bodyText string
	snapshot, cancelSnapshot := context.WithTimeout(h.Page, 5*time.Second)
	_ = chromedp.Run(snapshot, chromedp.Evaluate(`document.body.innerText`, &bodyText))
	cancelSnapshot()
	h.consoleMu.Lock()
	logged := strings.Join(h.console, "\n")
	h.consoleMu.Unlock()
	h.t.Fatalf(format+"\n--- page text ---\n%s\n--- browser console ---\n%s", append(args, bodyText, logged)...)
}

// run performs one step under its own deadline.
func (h *e2eHarness) run(what string, actions ...chromedp.Action) {
	h.t.Helper()
	step, cancelStep := context.WithTimeout(h.Page, 30*time.Second)
	defer cancelStep()
	if err := chromedp.Run(step, actions...); err != nil {
		h.fail("%s: %v", what, err)
	}
}

// setValue replaces an input's live value through the DOM property.
// chromedp.Clear writes the value attribute, which a field the test has
// already typed into ignores, and SetValue refuses an empty string.
func (h *e2eHarness) setValue(selector, value string) chromedp.Action {
	return chromedp.Evaluate(`(function(){const i=document.querySelector(`+strconv.Quote(selector)+`);i.value=`+
		strconv.Quote(value)+`;i.dispatchEvent(new Event("input",{bubbles:true}));return true})()`, nil)
}

// selectOption picks a <select>'s option and fires the change event the view
// listens for; typing into a select is not a thing a browser does.
func (h *e2eHarness) selectOption(selector, value string) chromedp.Action {
	return chromedp.Evaluate(`(function(){const s=document.querySelector(`+strconv.Quote(selector)+`);s.value=`+
		strconv.Quote(value)+`;s.dispatchEvent(new Event("change",{bubbles:true}));return true})()`, nil)
}

// waitJS polls a predicate in the page until it is truthy.
func (h *e2eHarness) waitJS(what, predicate string) {
	h.t.Helper()
	var ok bool
	if err := chromedp.Run(h.Page, chromedp.Poll(predicate, &ok,
		chromedp.WithPollingTimeout(30*time.Second), chromedp.WithPollingInterval(100*time.Millisecond))); err != nil {
		h.fail("%s: %v", what, err)
	}
}

func (h *e2eHarness) text(selector string) string {
	h.t.Helper()
	var value string
	h.run("read "+selector, chromedp.Text(selector, &value, chromedp.ByQuery))
	return value
}

func (h *e2eHarness) attribute(selector, name string) string {
	h.t.Helper()
	var value string
	var found bool
	h.run("attribute "+selector, chromedp.AttributeValue(selector, name, &value, &found, chromedp.ByQuery))
	if !found {
		h.t.Fatalf("%s has no %s attribute", selector, name)
	}
	return value
}

func (h *e2eHarness) evalString(expression string) string {
	h.t.Helper()
	var value string
	h.run("evaluate "+expression, chromedp.Evaluate(expression, &value))
	return value
}

// waitUntil polls a condition on the Go side of the test: the hub's own
// answers and the webhook receiver's counters, neither of which the browser
// can be asked about.
func (h *e2eHarness) waitUntil(what string, timeout time.Duration, condition func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			h.fail("%s: still false after %s", what, timeout)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// findBrowser locates a Chromium-family executable or decides between skip
// and fail, per the VYSHKA_E2E contract in the file comment.
func findBrowser(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("VYSHKA_E2E_BROWSER"); path != "" {
		return path
	}
	var candidates []string
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
			if root == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(root, "Chromium", "Application", "chrome.exe"),
				filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"))
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	} else {
		candidates = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"}
		for _, candidate := range candidates {
			if path, err := exec.LookPath(candidate); err == nil {
				return path
			}
			if strings.HasPrefix(candidate, "/") {
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
	}
	if os.Getenv("VYSHKA_E2E") == "required" {
		t.Fatalf("VYSHKA_E2E=required but no browser was found (tried %s); set VYSHKA_E2E_BROWSER", strings.Join(candidates, ", "))
	}
	t.Skipf("no Chromium-family browser found (tried %s); set VYSHKA_E2E_BROWSER to run the panel end-to-end test", strings.Join(candidates, ", "))
	return ""
}

// ---------------------------------------------------------------------------
// The Admin API and Plugin API halves, over HTTP like any other client.

type createdServer struct {
	Server struct {
		ID string `json:"id"`
	} `json:"server"`
	Enrollment struct {
		Token string `json:"token"`
	} `json:"enrollment"`
}

func postJSON(client *http.Client, url, bearer string, body any) (int, []byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, answer, err
}

func createServer(t *testing.T, hubURL, name string) createdServer {
	t.Helper()
	status, body, err := postJSON(http.DefaultClient, hubURL+"/api/v1/servers", e2eAdminToken,
		map[string]any{"name": name, "game": "e2e"})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("create server: status %d err %v body %s", status, err, body)
	}
	var created createdServer
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created server: %v", err)
	}
	return created
}

type wireEnvelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

type dispatchBody struct {
	ActionID     string          `json:"actionId"`
	Code         string          `json:"code"`
	Context      string          `json:"context"`
	ReferenceKey string          `json:"referenceKey"`
	Params       json.RawMessage `json:"params"`
}

// fakePlugin is the smallest plugin that can serve these tests: it enrolls,
// keeps one session, long-polls, and answers every action.dispatch with an
// action.ack and an action.result computed from the params. Its outbound
// buffer is acked cumulatively like the protocol says; it never needs to
// survive a session change, so it does no renumbering.
type fakePlugin struct {
	t      *testing.T
	hubURL string
	client *http.Client

	serverID, serverSecret, sessionToken string

	mu           sync.Mutex
	inAck        int64
	outSeq       int64
	buffer       []wireEnvelope
	ids          int64
	received     []dispatchBody
	unauthorized int
	// enumerations counts the context.enumerate questions answered, which
	// is how the browser test tells a cached read from a fresh one.
	enumerations int
	allAcked     chan struct{}
	ackedOnce    sync.Once
}

// enumerated is how many context.enumerate questions this plugin answered.
func (p *fakePlugin) enumerated() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enumerations
}

// e2eContextEntries is what the fake plugin answers a context.enumerate with,
// by context id (spec section 6.2). The browser test's manifest declares the
// contexts; a question about any other id gets an empty list and a reason.
var e2eContextEntries = map[string][]map[string]any{
	"example-mod.item": {
		{"referenceKey": "AKM", "label": "AKM", "data": map[string]any{"type": "firearm"}},
		{"referenceKey": "Apple", "label": "Apple", "data": map[string]any{"type": "edible"}},
	},
	"example-mod.landmark": {
		{"referenceKey": "green-mountain", "label": "Green Mountain", "position": []float64{3700, 402, 5980}},
	},
}

func newFakePlugin(t *testing.T, hubURL, enrollmentToken string) *fakePlugin {
	t.Helper()
	p := &fakePlugin{t: t, hubURL: hubURL, client: &http.Client{Timeout: 15 * time.Second}, allAcked: make(chan struct{})}
	status, body, err := postJSON(p.client, hubURL+"/plugin/v1/enroll", "", map[string]any{
		"enrollmentToken": enrollmentToken, "game": "e2e",
		"plugin": map[string]any{"name": "e2e-plugin", "version": "0.1.0"}, "transports": []string{"poll"},
	})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("enroll: status %d err %v body %s", status, err, body)
	}
	var enrolled struct {
		ServerID     string `json:"serverId"`
		ServerSecret string `json:"serverSecret"`
	}
	if err := json.Unmarshal(body, &enrolled); err != nil {
		t.Fatalf("decode enrollment: %v", err)
	}
	p.serverID, p.serverSecret = enrolled.ServerID, enrolled.ServerSecret

	status, body, err = postJSON(p.client, hubURL+"/plugin/v1/session", "", map[string]any{
		"serverId": p.serverID, "serverSecret": p.serverSecret, "protocolVersion": 1,
		"pollTimeoutSeconds": 5,
		"plugin":             map[string]any{"name": "e2e-plugin", "version": "0.1.0"}, "transports": []string{"poll"},
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("session: status %d err %v body %s", status, err, body)
	}
	var session struct {
		SessionToken string `json:"sessionToken"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	p.sessionToken = session.SessionToken
	return p
}

func (p *fakePlugin) queue(envelopeType string, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		p.t.Fatalf("encode %s: %v", envelopeType, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outSeq++
	p.ids++
	p.buffer = append(p.buffer, wireEnvelope{
		V: 1, ID: "e2e-" + strconv.FormatInt(p.ids, 10), Type: envelopeType, Seq: p.outSeq,
		TS: time.Now().UTC().Format(time.RFC3339), Body: encoded,
	})
}

func (p *fakePlugin) run(ctx context.Context) {
	for ctx.Err() == nil {
		p.mu.Lock()
		request := map[string]any{"ack": p.inAck, "envelopes": append([]wireEnvelope(nil), p.buffer...)}
		p.mu.Unlock()
		status, body, err := postJSON(p.client, p.hubURL+"/plugin/v1/poll", p.sessionToken, request)
		if status == http.StatusUnauthorized {
			// Revoked credentials, or a session the hub no longer knows: the
			// loop stops rather than hammering a door that is now locked,
			// which is also what a real plugin does before re-enrolling. The
			// count is what the management test asserts on.
			p.mu.Lock()
			p.unauthorized++
			p.mu.Unlock()
			return
		}
		if err != nil || status != http.StatusOK {
			if ctx.Err() != nil {
				return
			}
			p.t.Logf("fake plugin poll: status %d err %v body %s", status, err, body)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		var answer struct {
			Envelopes []wireEnvelope `json:"envelopes"`
			Ack       int64          `json:"ack"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			p.t.Logf("fake plugin poll: decode: %v", err)
			continue
		}
		p.mu.Lock()
		kept := p.buffer[:0]
		for _, e := range p.buffer {
			if e.Seq > answer.Ack {
				kept = append(kept, e)
			}
		}
		p.buffer = kept
		if len(p.buffer) == 0 && p.outSeq > 0 {
			p.ackedOnce.Do(func() { close(p.allAcked) })
		}
		for _, e := range answer.Envelopes {
			if e.Seq > p.inAck {
				p.inAck = e.Seq
			}
			if e.Type == "context.enumerate" {
				// The hub asking what a custom context holds (section 6.2):
				// answered from the fixture, echoing the question, with an
				// empty list and a reason for a context this plugin does not
				// declare.
				var question struct {
					RequestID string `json:"requestId"`
					Context   string `json:"context"`
				}
				if err := json.Unmarshal(e.Body, &question); err != nil {
					continue
				}
				p.enumerations++
				reply := map[string]any{"requestId": question.RequestID, "context": question.Context, "entries": []any{}}
				if entries, known := e2eContextEntries[question.Context]; known {
					reply["entries"] = entries
				} else {
					reply["reason"] = "this plugin declares no context " + question.Context
				}
				p.mu.Unlock()
				p.queue("context.entries", reply)
				p.mu.Lock()
				continue
			}
			if e.Type != "action.dispatch" {
				continue
			}
			var dispatch dispatchBody
			if err := json.Unmarshal(e.Body, &dispatch); err != nil {
				continue
			}
			p.received = append(p.received, dispatch)
			p.mu.Unlock()
			p.queue("action.ack", map[string]any{"actionId": dispatch.ActionID})
			var params struct {
				Amount float64 `json:"amount"`
			}
			_ = json.Unmarshal(dispatch.Params, &params)
			p.queue("action.result", map[string]any{
				"actionId": dispatch.ActionID, "ok": true,
				"result": map[string]any{"healedTo": params.Amount, "player": dispatch.ReferenceKey}, "durationMs": 7,
			})
			p.mu.Lock()
		}
		p.mu.Unlock()
	}
}

func (p *fakePlugin) awaitAcked(ctx context.Context) error {
	select {
	case <-p.allAcked:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitDrained waits until the hub has acked everything queued so far, which
// is when everything queued so far is stored.
func (p *fakePlugin) awaitDrained(ctx context.Context) error {
	for ctx.Err() == nil {
		p.mu.Lock()
		pending := len(p.buffer)
		p.mu.Unlock()
		if pending == 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ctx.Err()
}

func (p *fakePlugin) dispatches() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.received)
}

func (p *fakePlugin) lastDispatch() dispatchBody {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.received) == 0 {
		p.t.Fatal("the plugin received no dispatch")
	}
	return p.received[len(p.received)-1]
}

// unauthorizedPolls is how many times the hub refused this plugin's poll with
// a 401, which is what revoked credentials look like from the plugin's side.
func (p *fakePlugin) unauthorizedPolls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unauthorized
}
