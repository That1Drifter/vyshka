// Command loadgen drives a running Vyshka hub with synthetic game-server
// plugins and synthetic admin clients, over HTTP only, and reports what the
// hub did under that load (issue #87, spikes/hub-capacity).
//
// It never imports the hub: the numbers it reports hold for the wire, whatever
// implementation answers it.
//
// Each synthetic server enrolls, opens a session at pollTimeout 25, publishes a
// manifest of 28 actions, and then behaves the way the reference DayZ plugin's
// README describes: one poll in flight, events buffered and flushed into an
// event.batch every 2 s, state.players, state.vehicles and state.world
// captured as the poll is built (at most every 10 s, never while the previous
// one is unacked), and an action.ack plus action.result sent on the poll after
// a dispatch arrives. Admin clients behave like the panel with three views
// open: the server list, an event feed, and the live map, each re-read every
// 5 s, plus action dispatches whose record they then read every second until it
// is final. One webhook subscribed to core.player.death delivers to a sink this
// process serves.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	url            string
	adminToken     string
	servers        int
	players        int
	profile        string
	warmup         time.Duration
	duration       time.Duration
	pollTimeout    int
	actionsPerMin  float64
	panels         int
	webhookAddr    string
	cgroup         string
	dbDir          string
	out            string
	label          string
	verify         bool
	setupParallel  int
	memLimitBytes  int64
	snapshotFloor  time.Duration
	flushInterval  time.Duration
	panelInterval  time.Duration
	actionReadTick time.Duration
}

func main() {
	var cfg config
	flag.StringVar(&cfg.url, "url", "http://127.0.0.1:18787", "hub base URL")
	flag.StringVar(&cfg.adminToken, "admin-token", os.Getenv("VYSHKA_ADMIN_TOKEN"), "bootstrap admin token (env VYSHKA_ADMIN_TOKEN)")
	flag.IntVar(&cfg.servers, "servers", 10, "synthetic game servers")
	flag.IntVar(&cfg.players, "players", 100, "players online per server")
	flag.StringVar(&cfg.profile, "profile", "busy", "event profile: realistic or busy")
	flag.DurationVar(&cfg.warmup, "warmup", 60*time.Second, "load before the measurement window opens")
	flag.DurationVar(&cfg.duration, "duration", 180*time.Second, "measurement window")
	flag.IntVar(&cfg.pollTimeout, "poll-timeout", 25, "pollTimeoutSeconds each plugin requests")
	flag.Float64Var(&cfg.actionsPerMin, "actions-per-min", -1, "action dispatches per server per minute (-1: the profile's rate)")
	flag.IntVar(&cfg.panels, "panels", -1, "admin panel clients, one token each (-1: max(2, servers/10))")
	flag.StringVar(&cfg.webhookAddr, "webhook-addr", "127.0.0.1:18799", "address the webhook sink listens on")
	flag.StringVar(&cfg.cgroup, "cgroup", "", "the hub's cgroup v2 directory, sampled for memory and CPU")
	flag.StringVar(&cfg.dbDir, "db-dir", "", "directory holding the hub's vyshka.db, sampled for size")
	flag.StringVar(&cfg.out, "out", "", "write the JSON report here")
	flag.StringVar(&cfg.label, "label", "", "free-form label copied into the report")
	flag.BoolVar(&cfg.verify, "verify", false, "after the window, drain and count what the hub stored against what was sent")
	flag.IntVar(&cfg.setupParallel, "setup-parallel", 8, "concurrent server setups")
	flag.Int64Var(&cfg.memLimitBytes, "mem-limit", 2<<30, "the hub's memory limit in bytes, for the memory criterion")
	flag.Parse()
	cfg.snapshotFloor = 10 * time.Second
	cfg.flushInterval = 2 * time.Second
	cfg.panelInterval = 5 * time.Second
	cfg.actionReadTick = time.Second
	if cfg.adminToken == "" {
		fatalf("set -admin-token or VYSHKA_ADMIN_TOKEN")
	}
	prof, ok := profiles[cfg.profile]
	if !ok {
		fatalf("unknown profile %q", cfg.profile)
	}
	if cfg.actionsPerMin < 0 {
		cfg.actionsPerMin = prof.actionsPerServerPerMin
	}
	if cfg.panels < 0 {
		cfg.panels = max(2, cfg.servers/10)
	}
	if err := run(cfg, prof); err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "loadgen: "+format+"\n", args...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// Profiles. Rates are events per player per hour; see README.md for where
// each figure comes from.

type eventKind struct {
	t      string
	perHr  float64
	mkData func(r *rand.Rand, p player, now time.Time) map[string]any
}

type profile struct {
	kinds                  []eventKind
	actionsPerServerPerMin float64
}

var profiles = map[string]profile{
	"realistic": {kinds: mix(30, 1.5, 6, 1, 1, 2, 2), actionsPerServerPerMin: 1},
	"busy":      {kinds: mix(400, 6, 20, 2, 2, 6, 6), actionsPerServerPerMin: 6},
}

func mix(damage, death, chat, connect, disconnect, enter, exit float64) []eventKind {
	return []eventKind{
		{"core.player.damage", damage, damageData},
		{"core.player.death", death, deathData},
		{"core.player.chat", chat, chatData},
		{"core.player.connect", connect, nameData},
		{"core.player.disconnect", disconnect, nameData},
		{"vyshka.vehicle.enter", enter, vehicleData},
		{"vyshka.vehicle.exit", exit, vehicleData},
	}
}

func (p profile) perPlayerPerSecond() float64 {
	total := 0.0
	for _, k := range p.kinds {
		total += k.perHr
	}
	return total / 3600
}

type player struct {
	id   string
	name string
	pos  [3]float64
}

func ident(p player) map[string]any { return map[string]any{"platform": "steam", "id": p.id} }

func pos(r *rand.Rand, p player) []float64 {
	return []float64{round1(p.pos[0] + r.Float64()*40 - 20), round1(p.pos[1]), round1(p.pos[2] + r.Float64()*40 - 20)}
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

var weapons = []string{"M4-A1", "AKM", "Mosin 9130", "SKS", "FX-45", "Sporter 22", "KA-101", "Blaze"}
var ammos = []string{"Bullet_556x45", "Bullet_762x39", "Bullet_762x54", "MeleeInfected", "FallDamageHealth"}
var zones = []string{"Torso", "Head", "LeftLeg", "RightLeg", "LeftArm", "RightArm"}

func damageData(r *rand.Rand, p player, _ time.Time) map[string]any {
	return map[string]any{
		"player": ident(p), "name": p.name, "position": pos(r, p),
		"cause": "infected", "damageType": "melee", "bodyPart": zones[r.IntN(len(zones))],
		"ammo": ammos[r.IntN(len(ammos))], "damage": round1(r.Float64() * 30),
		"blood": round1(r.Float64() * 200), "shock": round1(r.Float64() * 20), "health": round1(r.Float64() * 100),
	}
}

func deathData(r *rand.Rand, p player, now time.Time) map[string]any {
	return map[string]any{
		"player": ident(p), "name": p.name, "position": pos(r, p),
		"cause": "player", "killer": map[string]any{"platform": "steam", "id": "7656119800" + strconv.Itoa(1000000+r.IntN(8999999))},
		"killerName": "Survivor", "weapon": weapons[r.IntN(len(weapons))], "distance": round1(r.Float64() * 400),
		"bodyPart": zones[r.IntN(len(zones))], "ammo": ammos[r.IntN(3)], "damageType": "firearm",
		// emittedAtUs and deathId let the webhook sink measure delivery
		// latency from the game and from the poll that carried the event.
		"emittedAtUs": now.UnixMicro(), "deathId": newID("death"),
	}
}

func chatData(r *rand.Rand, p player, _ time.Time) map[string]any {
	lines := []string{"anyone near elektro?", "friendly", "gg", "heli crash north of the airfield", "need a medic at NWAF tents, bleeding out"}
	return map[string]any{"player": ident(p), "name": p.name, "channel": "direct", "channelId": 0, "text": lines[r.IntN(len(lines))]}
}

func nameData(_ *rand.Rand, p player, _ time.Time) map[string]any {
	return map[string]any{"player": ident(p), "name": p.name}
}

func vehicleData(r *rand.Rand, p player, _ time.Time) map[string]any {
	return map[string]any{
		"player": ident(p), "name": p.name, "vehicle": "0-" + strconv.Itoa(2000+r.IntN(50)),
		"type": "OffroadHatchback", "kind": "car", "position": pos(r, p), "seat": r.IntN(4), "driver": r.IntN(4) == 0,
	}
}

// ---------------------------------------------------------------------------
// Metrics.

type samples struct {
	mu sync.Mutex
	v  []float64 // milliseconds
}

func (s *samples) add(d time.Duration) {
	s.mu.Lock()
	s.v = append(s.v, float64(d.Microseconds())/1000)
	s.mu.Unlock()
}

type dist struct {
	N    int     `json:"n"`
	P50  float64 `json:"p50Ms"`
	P90  float64 `json:"p90Ms"`
	P99  float64 `json:"p99Ms"`
	Max  float64 `json:"maxMs"`
	Mean float64 `json:"meanMs"`
}

func (s *samples) summary() dist {
	s.mu.Lock()
	v := append([]float64(nil), s.v...)
	s.mu.Unlock()
	if len(v) == 0 {
		return dist{}
	}
	sort.Float64s(v)
	q := func(p float64) float64 { return v[min(len(v)-1, int(math.Ceil(p*float64(len(v))))-1)] }
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return dist{N: len(v), P50: q(0.5), P90: q(0.9), P99: q(0.99), Max: v[len(v)-1], Mean: sum / float64(len(v))}
}

// rates counts requests per credential per second inside the window.
type rates struct {
	mu      sync.Mutex
	buckets map[string]map[int64]int
}

func (r *rates) hit(cred string, at time.Time) {
	r.mu.Lock()
	if r.buckets == nil {
		r.buckets = map[string]map[int64]int{}
	}
	b := r.buckets[cred]
	if b == nil {
		b = map[int64]int{}
		r.buckets[cred] = b
	}
	b[at.Unix()]++
	r.mu.Unlock()
}

type rateSummary struct {
	Credentials  int     `json:"credentials"`
	MeanRPS      float64 `json:"meanRps"`    // mean over credentials of requests/window
	MaxMeanRPS   float64 `json:"maxMeanRps"` // busiest credential's mean
	Peak1sMax    int     `json:"peak1sMax"`  // busiest single second of any credential
	Peak10sMax   float64 `json:"peak10sMax"` // busiest 10 s window of any credential, per second
	Peak60sMax   float64 `json:"peak60sMax"` // busiest 60 s window of any credential, per second
	Peak1sP99    int     `json:"peak1sP99"`  // 99th percentile over credentials of their busiest second
	TotalPerSecs float64 `json:"totalRps"`   // all credentials together
	WindowSecs   int     `json:"windowSeconds"`
}

func (r *rates) summary(from, to time.Time) rateSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	start, end := from.Unix(), to.Unix()
	window := int(end - start)
	out := rateSummary{Credentials: len(r.buckets), WindowSecs: window}
	if window <= 0 || len(r.buckets) == 0 {
		return out
	}
	var peaks []int
	total := 0
	for _, b := range r.buckets {
		n, peak := 0, 0
		series := make([]int, window)
		for sec, c := range b {
			if sec < start || sec >= end {
				continue
			}
			series[sec-start] = c
			n += c
			peak = max(peak, c)
		}
		total += n
		mean := float64(n) / float64(window)
		out.MeanRPS += mean
		out.MaxMeanRPS = math.Max(out.MaxMeanRPS, mean)
		out.Peak1sMax = max(out.Peak1sMax, peak)
		out.Peak10sMax = math.Max(out.Peak10sMax, slidingPeak(series, 10))
		out.Peak60sMax = math.Max(out.Peak60sMax, slidingPeak(series, 60))
		peaks = append(peaks, peak)
	}
	out.MeanRPS /= float64(len(r.buckets))
	out.TotalPerSecs = float64(total) / float64(window)
	sort.Ints(peaks)
	out.Peak1sP99 = peaks[min(len(peaks)-1, int(math.Ceil(0.99*float64(len(peaks))))-1)]
	return out
}

func slidingPeak(series []int, width int) float64 {
	if len(series) < width {
		width = len(series)
	}
	sum, best := 0, 0
	for i, c := range series {
		sum += c
		if i >= width {
			sum -= series[i-width]
		}
		if i >= width-1 {
			best = max(best, sum)
		}
	}
	return float64(best) / float64(width)
}

type metrics struct {
	measuring atomic.Bool

	pollOverhead  samples // polls held to term: RTT minus pollTimeout (the hub starts the hold after ingest)
	pollEarlyRTT  samples // polls answered before the hold ran out (a dispatch arrived)
	pollIngest    samples // polls that set more and were answered at once
	actionDeliver samples // dispatch POST start -> plugin has the envelope
	actionResult  samples // dispatch POST start -> the admin client's 1 s re-read first shows it completed
	dispatchRTT   samples
	adminReadRTT  samples
	readByRoute   [5]samples // servers list, events page, state players, vehicles, world
	actionReadRTT samples
	webhookLag    samples // event emitted in game -> sink received the delivery
	webhookHubLag samples // poll carrying the event sent -> sink received the delivery
	eventAckLag   samples // event emitted in game -> hub acked its batch

	requests     atomic.Int64
	failures     atomic.Int64
	failuresBy   sync.Map // endpoint -> *atomic.Int64
	eventsSent   atomic.Int64
	eventsAcked  atomic.Int64
	deathsAcked  atomic.Int64
	snapshots    atomic.Int64
	dispatched   atomic.Int64
	delivered    atomic.Int64
	results      atomic.Int64
	webhookHits  atomic.Int64
	bytesSent    atomic.Int64
	pluginRates  rates
	adminRates   rates
	outboxEvents sync.Map // serverIndex -> int (events emitted, not yet acked)

	// kinds counts, per request kind, the requests started and the ones that
	// came back 2xx inside the window, so a request that never returns shows
	// up as a gap between the two instead of as nothing (review C3).
	kinds sync.Map // kind -> *kindCount
	// dispatchArrivals counts the dispatches the Poisson schedule called for
	// inside the window, whether or not the request was ever answered.
	dispatchArrivals atomic.Int64
}

// cancelled counts requests started in the window that the run's own end
// cancelled before they were answered: not missing, just cut off.
type kindCount struct{ started, completed, cancelled atomic.Int64 }

func (m *metrics) kind(name string) *kindCount {
	v, _ := m.kinds.LoadOrStore(name, new(kindCount))
	return v.(*kindCount)
}

// adminTimeout bounds every admin request. The panel re-reads every 5 s, so a
// read that has not come back in 10 s has failed its reader twice over.
const adminTimeout = 10 * time.Second

// routeNames are the five reads an admin client makes every 5 s, graded one
// by one (review C2): pooling them let a slow server list hide behind four
// fast reads.
var routeNames = [5]string{"servers list", "events page", "state players", "state vehicles", "state world"}

func (m *metrics) fail(endpoint string) {
	if !m.measuring.Load() {
		return
	}
	m.failures.Add(1)
	v, _ := m.failuresBy.LoadOrStore(endpoint, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// ---------------------------------------------------------------------------
// HTTP.

type client struct {
	base string
	http *http.Client
	m    *metrics
}

// newClient gives one synthetic client its own connections (review C4): a
// game server or a browser owns its sockets, so the hub's connection cap has
// to be met by separate clients, not by a shared pool. maxConns is the
// client's own bound on connections to the hub: 1 for a plugin (one poll in
// flight on one HTTP context), 6 for a panel (a browser's per-host limit).
func newClient(base string, m *metrics, maxConns int) *client {
	transport := &http.Transport{MaxConnsPerHost: maxConns, MaxIdleConnsPerHost: maxConns, IdleConnTimeout: 2 * time.Minute}
	return &client{base: base, http: &http.Client{Transport: transport}, m: m}
}

type apiError struct {
	status     int
	body       string
	retryAfter time.Duration // from Retry-After on a 429
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.body) }

// do sends one request and decodes a 2xx body into out. cred names the
// credential for the per-credential rate count ("" counts nothing); endpoint
// names the route class for the failure tally.
func (c *client) do(ctx context.Context, method, path, token, cred, endpoint string, rr *rates, in, out any) (time.Duration, error) {
	for {
		elapsed, err := c.doOnce(ctx, method, path, token, cred, endpoint, rr, in, out)
		// Setup and -verify run outside the measured window, and the hub's
		// rate limits (since #86: 10 requests/s with a burst of 60 per admin
		// token, the bootstrap one included) refuse their bursts. They wait
		// out the hub's Retry-After and go on, as a polite client would.
		// Inside the window a 429 is a failure like any other non-2xx.
		var ae *apiError
		if (endpoint == "setup" || endpoint == "verify" || endpoint == "drain") && errors.As(err, &ae) && ae.status == http.StatusTooManyRequests {
			wait := max(ae.retryAfter, 200*time.Millisecond)
			select {
			case <-ctx.Done():
				return elapsed, err
			case <-time.After(wait):
			}
			continue
		}
		return elapsed, err
	}
}

// doOnce sends one request: do without the setup retry.
func (c *client) doOnce(ctx context.Context, method, path, token, cred, endpoint string, rr *rates, in, out any) (time.Duration, error) {
	var body io.Reader
	var n int
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		n = len(raw)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	start := time.Now()
	var kc *kindCount
	if c.m.measuring.Load() {
		c.m.requests.Add(1)
		c.m.bytesSent.Add(int64(n))
		if rr != nil && cred != "" {
			rr.hit(cred, start)
		}
		kc = c.m.kind(endpoint)
		kc.started.Add(1)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A request the generator itself cancelled (the run ending) is not a
		// failure; one that ran out its own deadline is.
		if !errors.Is(ctx.Err(), context.Canceled) {
			c.m.fail(endpoint + " " + transportFault(err))
		} else if kc != nil {
			kc.cancelled.Add(1)
		}
		return time.Since(start), err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if err != nil {
		// A request the generator itself cancelled (the run ending) is not a
		// failure; one that ran out its own deadline is.
		if !errors.Is(ctx.Err(), context.Canceled) {
			c.m.fail(endpoint + " " + transportFault(err))
		} else if kc != nil {
			kc.cancelled.Add(1)
		}
		return elapsed, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		key := endpoint + " " + strconv.Itoa(resp.StatusCode)
		var problem struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &problem) == nil && problem.Error.Code != "" {
			key += " " + problem.Error.Code
		}
		c.m.fail(key)
		ae := &apiError{status: resp.StatusCode, body: truncate(string(raw), 300)}
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 0 {
			ae.retryAfter = time.Duration(seconds) * time.Second
		}
		return elapsed, ae
	}
	if kc != nil {
		kc.completed.Add(1)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return elapsed, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return elapsed, nil
}

// doAdmin is do with the admin deadline.
func (c *client) doAdmin(ctx context.Context, method, path, token, cred, endpoint string, rr *rates, in, out any) (time.Duration, error) {
	actx, cancel := context.WithTimeout(ctx, adminTimeout)
	defer cancel()
	return c.do(actx, method, path, token, cred, endpoint, rr, in, out)
}

// transportFault names a transport error class for the failure tally.
func transportFault(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "transport"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// Synthetic plugin.

type envelope struct {
	V    int            `json:"v,omitempty"`
	ID   string         `json:"id"`
	Type string         `json:"type"`
	Seq  int64          `json:"seq"`
	TS   string         `json:"ts"`
	Body map[string]any `json:"body"`

	events   int       // events this envelope carries, for the per-poll budget
	deaths   int       // core.player.death events among them
	emitted  time.Time // oldest event's emission, for the ack lag
	sentOnce bool
}

type pollResponse struct {
	Envelopes []struct {
		Seq  int64           `json:"seq"`
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	} `json:"envelopes"`
	Ack int64 `json:"ack"`
}

type plugin struct {
	idx      int
	cfg      config
	prof     profile
	c        *client
	m        *metrics
	serverID string
	token    string
	rng      *rand.Rand
	players  []player

	mu         sync.Mutex
	outbox     []*envelope
	nextSeq    int64
	buffer     []map[string]any
	bufferFrom time.Time
	bufDeaths  int
	pending    int // events emitted and not yet acked

	emittedTotal int64 // every event emitted, warm-up included
	deathsTotal  int64

	hubSeq        int64     // highest hub -> plugin seq processed
	lastCapture   time.Time // last snapshot capture
	snapshotUnack int64     // highest snapshot seq not yet acked (0: none)
	deliveredAt   map[string]time.Time
}

// deathSent maps a death event's deathId to when the first poll carrying it
// was sent, for the webhook sink.
var deathSent sync.Map

func markDeaths(batch []*envelope, at time.Time) {
	for _, e := range batch {
		if e.deaths == 0 || e.sentOnce {
			continue
		}
		e.sentOnce = true
		events, _ := e.Body["events"].([]map[string]any)
		for _, ev := range events {
			if ev["t"] != "core.player.death" {
				continue
			}
			if data, ok := ev["data"].(map[string]any); ok {
				if id, ok := data["deathId"].(string); ok {
					deathSent.LoadOrStore(id, at)
				}
			}
		}
	}
}

var idCounter atomic.Int64

func newID(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(idCounter.Add(1), 36)
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (p *plugin) setup(ctx context.Context) error {
	var created struct {
		Server     struct{ ID string } `json:"server"`
		Enrollment struct{ Token string }
	}
	if _, err := p.c.do(ctx, "POST", "/api/v1/servers", p.cfg.adminToken, "", "setup", nil,
		map[string]any{"name": fmt.Sprintf("Capacity #%d", p.idx+1), "game": "dayz"}, &created); err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	p.serverID = created.Server.ID
	var enrolled struct{ ServerSecret string }
	if _, err := p.c.do(ctx, "POST", "/plugin/v1/enroll", "", "", "setup", nil, map[string]any{
		"enrollmentToken": created.Enrollment.Token, "game": "dayz",
		"plugin": map[string]any{"name": "capacity-loadgen", "version": "0.1.0"}, "transports": []string{"poll"},
	}, &enrolled); err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	var session struct {
		SessionToken       string
		PollTimeoutSeconds int
	}
	if _, err := p.c.do(ctx, "POST", "/plugin/v1/session", "", "", "setup", nil, map[string]any{
		"serverId": p.serverID, "serverSecret": enrolled.ServerSecret, "protocolVersion": 1,
		"pollTimeoutSeconds": p.cfg.pollTimeout,
		"plugin":             map[string]any{"name": "capacity-loadgen", "version": "0.1.0"}, "transports": []string{"poll"},
	}, &session); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if session.PollTimeoutSeconds != p.cfg.pollTimeout {
		return fmt.Errorf("hub granted pollTimeout %d, asked %d", session.PollTimeoutSeconds, p.cfg.pollTimeout)
	}
	p.token = session.SessionToken
	p.nextSeq = 1
	p.deliveredAt = map[string]time.Time{}
	for i := range p.cfg.players {
		p.players = append(p.players, player{
			id:   strconv.FormatInt(76561198000000000+int64(p.idx)*1000+int64(i), 10),
			name: fmt.Sprintf("Survivor %d-%d", p.idx, i),
			pos:  [3]float64{1000 + p.rng.Float64()*13000, 200 + p.rng.Float64()*200, 1000 + p.rng.Float64()*13000},
		})
	}
	// The manifest rides the first poll, the way the DayZ plugin publishes it
	// on its first poll after the session opens. pollTimeout is honored, so the
	// answer comes at the end of the hold; a short context is enough because
	// an ack arrives either way once the hold ends.
	p.mu.Lock()
	p.appendLocked("manifest.publish", manifestBody(), 0, 0, time.Time{})
	p.mu.Unlock()
	return nil
}

func manifestBody() map[string]any {
	verbs := []string{"heal", "kill", "kick", "ban", "unban", "teleport", "spawn", "strip", "freeze", "godmode",
		"invisible", "message", "broadcast", "time", "weather", "repair", "refuel", "delete", "lock", "unlock",
		"loadout", "capture", "inventory", "wipe", "restart", "shutdown", "mute", "unmute"}
	actions := make([]map[string]any, 0, len(verbs))
	for _, v := range verbs {
		actions = append(actions, map[string]any{
			"code": "capacity." + v, "name": strings.ToUpper(v[:1]) + v[1:] + " (load test)",
			"context": "player", "namespace": "capacity", "danger": "warning",
			"params": map[string]any{
				"type": "object", "required": []string{"amount"},
				"properties": map[string]any{
					"amount": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
					"sentAt": map[string]any{"type": "integer"},
				},
			},
		})
	}
	return map[string]any{"manifestRevision": 1, "actions": actions}
}

// appendLocked queues one outbound envelope. p.mu must be held.
func (p *plugin) appendLocked(typ string, body map[string]any, events, deaths int, emitted time.Time) *envelope {
	e := &envelope{V: 1, ID: newID(typ), Type: typ, Seq: p.nextSeq, TS: stamp(time.Now()), Body: body,
		events: events, deaths: deaths, emitted: emitted}
	p.nextSeq++
	p.outbox = append(p.outbox, e)
	return e
}

// emitter adds game events to the buffer at the profile's rate and flushes the
// buffer into an event.batch every 2 s or at 200 events, as the DayZ plugin
// does. fps samples go out every 60 s.
func (p *plugin) emitter(ctx context.Context) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	rate := p.prof.perPlayerPerSecond() * float64(len(p.players)) // events/s
	var acc float64
	lastFlush, lastFPS := time.Now(), time.Now()
	weights := make([]float64, len(p.prof.kinds))
	total := 0.0
	for i, k := range p.prof.kinds {
		total += k.perHr
		weights[i] = total
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			acc += rate * 0.2
			n := int(acc)
			acc -= float64(n)
			p.mu.Lock()
			for range n {
				k := p.prof.kinds[sort.SearchFloat64s(weights, p.rng.Float64()*total)]
				pl := p.players[p.rng.IntN(len(p.players))]
				p.addEventLocked(now, k.t, k.mkData(p.rng, pl, now))
			}
			if now.Sub(lastFPS) >= 60*time.Second {
				lastFPS = now
				p.addEventLocked(now, "core.server.fps", map[string]any{"fps": round1(40 + p.rng.Float64()*20), "players": len(p.players)})
			}
			// Reported here as well as after each poll, so a server whose polls
			// never come back still shows its backlog.
			p.m.outboxEvents.Store(p.idx, p.pending)
			if len(p.buffer) >= 200 || (len(p.buffer) > 0 && now.Sub(lastFlush) >= p.cfg.flushInterval) {
				p.flushLocked()
				lastFlush = now
			} else if len(p.buffer) == 0 {
				lastFlush = now
			}
			p.mu.Unlock()
		}
	}
}

func (p *plugin) addEventLocked(now time.Time, typ string, data map[string]any) {
	if len(p.buffer) == 0 {
		p.bufferFrom = now
	}
	p.buffer = append(p.buffer, map[string]any{"t": typ, "ts": stamp(now), "data": data})
	if typ == "core.player.death" {
		p.bufDeaths++
	}
	p.pending++
	p.emittedTotal++
	if typ == "core.player.death" {
		p.deathsTotal++
	}
	if p.m.measuring.Load() {
		p.m.eventsSent.Add(1)
	}
	if len(p.buffer) >= 200 {
		p.flushLocked()
	}
}

func (p *plugin) flushLocked() {
	if len(p.buffer) == 0 {
		return
	}
	p.appendLocked("event.batch", map[string]any{"events": p.buffer}, len(p.buffer), p.bufDeaths, p.bufferFrom)
	p.buffer, p.bufDeaths = nil, 0
}

func (p *plugin) captureLocked(now time.Time) {
	if p.cfg.snapshotFloor <= 0 || now.Sub(p.lastCapture) < p.cfg.snapshotFloor || p.snapshotUnack != 0 {
		return
	}
	p.lastCapture = now
	players := make([]map[string]any, 0, len(p.players))
	for i := range p.players {
		pl := &p.players[i]
		pl.pos[0] += p.rng.Float64()*20 - 10
		pl.pos[2] += p.rng.Float64()*20 - 10
		players = append(players, map[string]any{
			"player": ident(*pl), "name": pl.name, "position": []float64{round1(pl.pos[0]), round1(pl.pos[1]), round1(pl.pos[2])},
			"data": map[string]any{"alive": true, "health": 100, "blood": 5000},
		})
	}
	vehicles := make([]map[string]any, 0, 50)
	for i := range 50 {
		vehicles = append(vehicles, map[string]any{
			"id": "0-" + strconv.Itoa(2000+i), "kind": "car",
			"position": []float64{round1(6500 + float64(i)*37.5), 284.6, round1(7498.1 + float64(i)*11.25)},
			"data": map[string]any{"type": "OffroadHatchback", "displayName": "ADA 4x4", "seats": 4, "crew": []any{},
				"state": "intact", "health": 100, "fuel": 0.62, "engineOn": false},
		})
	}
	ts := stamp(now)
	p.appendLocked("state.players", map[string]any{"capturedAt": ts, "players": players}, 0, 0, time.Time{})
	p.appendLocked("state.vehicles", map[string]any{"capturedAt": ts, "vehicles": vehicles}, 0, 0, time.Time{})
	last := p.appendLocked("state.world", map[string]any{"capturedAt": ts, "world": map[string]any{
		"time": "2026-09-22T12:32", "data": map[string]any{"timeFrozen": false, "night": false, "overcast": 0.44, "fog": 0.02, "rain": 0, "windSpeed": 11.32}}},
		0, 0, time.Time{})
	p.snapshotUnack = last.Seq
	if p.m.measuring.Load() {
		p.m.snapshots.Add(3)
	}
}

// buildLocked frames the next poll: at most 200 envelopes and 1000 events,
// the DayZ plugin's own limits.
func (p *plugin) buildLocked(now time.Time) (map[string]any, []*envelope) {
	p.captureLocked(now)
	var batch []*envelope
	events := 0
	more := false
	for _, e := range p.outbox {
		if len(batch) == 200 || events+e.events > 1000 {
			more = true
			break
		}
		batch = append(batch, e)
		events += e.events
	}
	req := map[string]any{"ack": p.hubSeq}
	if len(batch) > 0 {
		req["envelopes"] = batch
		if more {
			req["more"] = true
		}
	}
	return req, batch
}

func (p *plugin) pollLoop(ctx context.Context) {
	hold := time.Duration(p.cfg.pollTimeout) * time.Second
	for ctx.Err() == nil {
		p.mu.Lock()
		req, batch := p.buildLocked(time.Now())
		markDeaths(batch, time.Now())
		p.mu.Unlock()
		var resp pollResponse
		pctx, cancel := context.WithTimeout(ctx, hold+30*time.Second)
		rtt, err := p.c.do(pctx, "POST", "/plugin/v1/poll", p.token, p.serverID, "poll", &p.m.pluginRates, req, &resp)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Back off as a plugin would, then retransmit.
			time.Sleep(time.Second)
			continue
		}
		received := time.Now()
		measuring := p.m.measuring.Load()
		early := rtt < hold
		if measuring {
			if early {
				p.m.pollEarlyRTT.add(rtt)
				if len(batch) > 0 && req["more"] == true {
					p.m.pollIngest.add(rtt)
				}
			} else {
				p.m.pollOverhead.add(rtt - hold)
			}
		}
		p.mu.Lock()
		// Acked envelopes leave the outbox.
		keep := p.outbox[:0]
		for _, e := range p.outbox {
			if e.Seq <= resp.Ack {
				p.pending -= e.events
				if measuring {
					p.m.eventsAcked.Add(int64(e.events))
					p.m.deathsAcked.Add(int64(e.deaths))
					if e.events > 0 {
						p.m.eventAckLag.add(received.Sub(e.emitted))
					}
					if e.Type == "action.result" {
						p.m.results.Add(1)
					}
				}
				if e.Seq == p.snapshotUnack {
					p.snapshotUnack = 0
				}
				continue
			}
			keep = append(keep, e)
		}
		p.outbox = keep
		for _, in := range resp.Envelopes {
			if in.Seq <= p.hubSeq {
				continue
			}
			p.hubSeq = in.Seq
			if in.Type != "action.dispatch" {
				continue
			}
			var body struct {
				ActionID string `json:"actionId"`
				Params   struct {
					SentAt int64 `json:"sentAt"`
				} `json:"params"`
			}
			if json.Unmarshal(in.Body, &body) != nil || body.ActionID == "" {
				continue
			}
			started := time.UnixMicro(body.Params.SentAt)
			if measuring {
				p.m.delivered.Add(1)
				p.m.actionDeliver.add(received.Sub(started))
			}
			p.appendLocked("action.ack", map[string]any{"actionId": body.ActionID}, 0, 0, time.Time{})
			p.appendLocked("action.result", map[string]any{"actionId": body.ActionID, "ok": true,
				"result": map[string]any{"healedTo": 100.0}, "durationMs": 3}, 0, 0, time.Time{})
		}
		p.m.outboxEvents.Store(p.idx, p.pending)
		p.mu.Unlock()
	}
}

// drain pushes whatever is still buffered and keeps polling until the outbox
// is empty, for the -verify count.
func (p *plugin) drain(ctx context.Context) {
	p.mu.Lock()
	p.flushLocked()
	p.cfg.snapshotFloor = 0
	p.mu.Unlock()
	deadline := time.Now().Add(2 * time.Duration(p.cfg.pollTimeout+5) * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		empty := len(p.outbox) == 0
		req, _ := p.buildLocked(time.Now())
		p.mu.Unlock()
		if empty {
			return
		}
		var resp pollResponse
		if _, err := p.c.do(ctx, "POST", "/plugin/v1/poll", p.token, "", "drain", nil, req, &resp); err != nil {
			continue
		}
		p.mu.Lock()
		keep := p.outbox[:0]
		for _, e := range p.outbox {
			if e.Seq <= resp.Ack {
				p.pending -= e.events
				continue
			}
			keep = append(keep, e)
		}
		p.outbox = keep
		for _, in := range resp.Envelopes {
			p.hubSeq = max(p.hubSeq, in.Seq)
		}
		p.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Admin clients.

type panelClient struct {
	idx    int
	token  string
	cred   string
	c      *client
	m      *metrics
	rng    *rand.Rand
	plugin []*plugin
}

// browse re-reads the three views every 5 s, staggered, as the panel does
// with the server list, an event feed and the live map open.
func (pc *panelClient) browse(ctx context.Context) {
	time.Sleep(time.Duration(pc.rng.IntN(5000)) * time.Millisecond)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	// Each admin watches one server's feed and map at a time and moves on
	// every minute.
	var watched *plugin
	var since time.Time
	for {
		if watched == nil || time.Since(since) > time.Minute {
			watched = pc.plugin[pc.rng.IntN(len(pc.plugin))]
			since = time.Now()
		}
		for i, path := range []string{
			"/api/v1/servers",
			"/api/v1/servers/" + watched.serverID + "/events?limit=100",
			"/api/v1/servers/" + watched.serverID + "/state/players",
			"/api/v1/servers/" + watched.serverID + "/state/vehicles",
			"/api/v1/servers/" + watched.serverID + "/state/world",
		} {
			route := i
			rtt, err := pc.c.doAdmin(ctx, "GET", path, pc.token, pc.cred, "read "+routeNames[route], &pc.m.adminRates, nil, nil)
			if err == nil && pc.m.measuring.Load() {
				pc.m.adminReadRTT.add(rtt)
				pc.m.readByRoute[route].add(rtt)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// dispatchOne queues one action on a random server and then reads its record
// every second until it is final, the way the panel's action form does.
func (pc *panelClient) dispatchOne(ctx context.Context, verbs []string, rng *rand.Rand) {
	target := pc.plugin[rng.IntN(len(pc.plugin))]
	started := time.Now()
	var accepted struct {
		ActionID string `json:"actionId"`
	}
	if pc.m.measuring.Load() {
		pc.m.dispatchArrivals.Add(1)
	}
	rtt, err := pc.c.doAdmin(ctx, "POST", "/api/v1/servers/"+target.serverID+"/actions", pc.token, pc.cred, "dispatch", &pc.m.adminRates,
		map[string]any{
			"code": "capacity." + verbs[rng.IntN(len(verbs))], "context": "player",
			"referenceKey": target.players[rng.IntN(len(target.players))].id,
			"params":       map[string]any{"amount": 1 + rng.IntN(100), "sentAt": started.UnixMicro()},
		}, &accepted)
	if err != nil {
		return
	}
	dispatchedIDs.Store(accepted.ActionID, struct{}{})
	if pc.m.measuring.Load() {
		pc.m.dispatched.Add(1)
		pc.m.dispatchRTT.add(rtt)
	}
	for range 120 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		var record struct {
			State string `json:"state"`
		}
		rtt, err := pc.c.doAdmin(ctx, "GET", "/api/v1/actions/"+accepted.ActionID, pc.token, pc.cred, "action read", &pc.m.adminRates, nil, &record)
		if err != nil {
			continue
		}
		if pc.m.measuring.Load() {
			pc.m.actionReadRTT.add(rtt)
		}
		if record.State == "completed" || record.State == "failed" || record.State == "expired" {
			if record.State == "completed" && pc.m.measuring.Load() {
				pc.m.actionResult.add(time.Since(started))
			}
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Webhook sink.

func webhookSink(addr string, m *metrics) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		now := time.Now()
		sinkTotal.Add(1)
		var delivery struct {
			Data struct {
				EmittedAtUs int64  `json:"emittedAtUs"`
				DeathID     string `json:"deathId"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &delivery) == nil && delivery.Data.EmittedAtUs > 0 && m.measuring.Load() {
			m.webhookHits.Add(1)
			m.webhookLag.add(now.Sub(time.UnixMicro(delivery.Data.EmittedAtUs)))
			if sent, ok := deathSent.Load(delivery.Data.DeathID); ok {
				m.webhookHubLag.add(now.Sub(sent.(time.Time)))
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	return srv, nil
}

// ---------------------------------------------------------------------------
// Hub resource sampler (cgroup v2 and the database files).

type sample struct {
	At          time.Time `json:"at"`
	MemBytes    int64     `json:"anonBytes"`
	CgroupBytes int64     `json:"cgroupBytes"`
	CPUUsec     int64     `json:"cpuUsec"`
	DBBytes     int64     `json:"dbBytes"`
	// cpu.stat throttle counters: CFS periods elapsed, periods in which the
	// quota ran out, and the time the group spent throttled.
	NrPeriods     int64 `json:"nrPeriods"`
	NrThrottled   int64 `json:"nrThrottled"`
	ThrottledUsec int64 `json:"throttledUsec"`
	WALBytes      int64 `json:"walBytes"`
}

type sampler struct {
	cgroup, dbDir string
	mu            sync.Mutex
	series        []sample
}

func readInt(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return n
}

func readKey(path, key string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == key {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	return -1
}

func (s *sampler) take() sample {
	out := sample{At: time.Now(), MemBytes: -1, CPUUsec: -1}
	if s.cgroup != "" {
		// anon is the hub's own memory; memory.current also counts the page
		// cache the database writes leave behind, which the kernel reclaims.
		out.MemBytes = readKey(filepath.Join(s.cgroup, "memory.stat"), "anon")
		out.CgroupBytes = readInt(filepath.Join(s.cgroup, "memory.current"))
		out.CPUUsec = readKey(filepath.Join(s.cgroup, "cpu.stat"), "usage_usec")
		out.NrPeriods = readKey(filepath.Join(s.cgroup, "cpu.stat"), "nr_periods")
		out.NrThrottled = readKey(filepath.Join(s.cgroup, "cpu.stat"), "nr_throttled")
		out.ThrottledUsec = readKey(filepath.Join(s.cgroup, "cpu.stat"), "throttled_usec")
	}
	if s.dbDir != "" {
		if info, err := os.Stat(filepath.Join(s.dbDir, "vyshka.db")); err == nil {
			out.DBBytes = info.Size()
		}
		if info, err := os.Stat(filepath.Join(s.dbDir, "vyshka.db-wal")); err == nil {
			out.WALBytes = info.Size()
		}
	}
	return out
}

func (s *sampler) run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		one := s.take()
		s.mu.Lock()
		s.series = append(s.series, one)
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

type resourceSummary struct {
	PeakMemBytes   int64   `json:"peakAnonBytes"`
	PeakCgroupMem  int64   `json:"peakCgroupBytes"`
	MeanMemBytes   int64   `json:"meanMemBytes"`
	CPUPercentMean float64 `json:"cpuPercentMean"` // of one CPU
	CPUPercentP90  float64 `json:"cpuPercentP90"`  // over 1 s samples
	DBStartBytes   int64   `json:"dbStartBytes"`
	DBEndBytes     int64   `json:"dbEndBytes"`
	DBBytesPerHour float64 `json:"dbBytesPerHour"` // main database file only
	WALEndBytes    int64   `json:"walEndBytes"`
	WALPeakBytes   int64   `json:"walPeakBytes"`
	OOMKills       int64   `json:"oomKills"`
	// Throttling over the window: the share of CFS periods in which the quota
	// ran out, and the time the hub spent throttled, as a share of the window.
	ThrottledPeriodsPct float64 `json:"throttledPeriodsPct"`
	ThrottledTimePct    float64 `json:"throttledTimePct"`
	MemPeakCgroup       int64   `json:"memPeakCgroup"`
}

func (s *sampler) summary(from, to time.Time) resourceSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	var in []sample
	for _, x := range s.series {
		if !x.At.Before(from) && !x.At.After(to) {
			in = append(in, x)
		}
	}
	out := resourceSummary{OOMKills: -1, MemPeakCgroup: -1}
	if s.cgroup != "" {
		out.OOMKills = readKey(filepath.Join(s.cgroup, "memory.events"), "oom_kill")
		out.MemPeakCgroup = readInt(filepath.Join(s.cgroup, "memory.peak"))
	}
	if len(in) < 2 {
		return out
	}
	var memSum int64
	var cpu []float64
	for i, x := range in {
		out.WALPeakBytes = max(out.WALPeakBytes, x.WALBytes)
		out.PeakMemBytes = max(out.PeakMemBytes, x.MemBytes)
		out.PeakCgroupMem = max(out.PeakCgroupMem, x.CgroupBytes)
		memSum += x.MemBytes
		if i > 0 && x.CPUUsec >= 0 {
			dt := x.At.Sub(in[i-1].At).Microseconds()
			cpu = append(cpu, 100*float64(x.CPUUsec-in[i-1].CPUUsec)/float64(dt))
		}
	}
	out.MeanMemBytes = memSum / int64(len(in))
	first, last := in[0], in[len(in)-1]
	wall := last.At.Sub(first.At)
	if first.CPUUsec >= 0 {
		out.CPUPercentMean = 100 * float64(last.CPUUsec-first.CPUUsec) / float64(wall.Microseconds())
		sort.Float64s(cpu)
		out.CPUPercentP90 = cpu[min(len(cpu)-1, int(math.Ceil(0.9*float64(len(cpu))))-1)]
	}
	out.DBStartBytes, out.DBEndBytes, out.WALEndBytes = first.DBBytes, last.DBBytes, last.WALBytes
	if periods := last.NrPeriods - first.NrPeriods; periods > 0 {
		out.ThrottledPeriodsPct = 100 * float64(last.NrThrottled-first.NrThrottled) / float64(periods)
	}
	if wall > 0 && first.ThrottledUsec >= 0 {
		out.ThrottledTimePct = 100 * float64(last.ThrottledUsec-first.ThrottledUsec) / float64(wall.Microseconds())
	}
	out.DBBytesPerHour = float64(last.DBBytes-first.DBBytes) / wall.Hours()
	return out
}

// ---------------------------------------------------------------------------
// Criteria, fixed before the first run (README.md, "Failure criteria").

type criterion struct {
	Name     string  `json:"name"`
	Value    float64 `json:"value"`
	Limit    float64 `json:"limit"`
	Unit     string  `json:"unit"`
	Failed   bool    `json:"failed"`
	Measured bool    `json:"measured"`
	// Samples is how many values a latency criterion's p99 was taken over;
	// a p99 of n samples is the ceil(0.01 n)-th largest of them. A latency
	// criterion with no samples fails: nothing came back to be graded.
	Samples int `json:"samples,omitempty"`
}

// kindReport is one request kind's started and completed counts in the
// window, and for the admin reads what the 5 s schedule called for.
type kindReport struct {
	Scheduled int64 `json:"scheduled,omitempty"`
	Started   int64 `json:"started"`
	Completed int64 `json:"completed"`
	// Cancelled were started in the window and cut off by the run's end.
	Cancelled int64 `json:"cancelled"`
	// Missing is what the schedule called for that neither completed nor was
	// cut off by the run's end: timed out, refused, or never started.
	Missing int64 `json:"missing"`
}

type report struct {
	Label        string                `json:"label"`
	Servers      int                   `json:"servers"`
	Players      int                   `json:"players"`
	Profile      string                `json:"profile"`
	EventsPerSec float64               `json:"offeredEventsPerSecond"`
	ActionsPerS  float64               `json:"offeredActionsPerSecond"`
	Panels       int                   `json:"panels"`
	WindowStart  time.Time             `json:"windowStart"`
	WindowEnd    time.Time             `json:"windowEnd"`
	Pass         bool                  `json:"pass"`
	Criteria     []criterion           `json:"criteria"`
	Latency      map[string]dist       `json:"latency"`
	Counts       map[string]int64      `json:"counts"`
	Failures     map[string]int64      `json:"failures"`
	PluginRates  rateSummary           `json:"pluginRates"`
	AdminRates   rateSummary           `json:"adminRates"`
	Resources    resourceSummary       `json:"resources"`
	Backlog      map[string]int64      `json:"backlog"`
	Requests     map[string]kindReport `json:"requests"`
	Verify       map[string]any        `json:"verify,omitempty"`
	Notes        map[string]string     `json:"notes,omitempty"`
}

func run(cfg config, prof profile) error {
	m := &metrics{}
	base := strings.TrimRight(cfg.url, "/")
	// c is the operator's own client for setup and -verify; every synthetic
	// plugin and admin client gets one of its own below.
	c := newClient(base, m, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink, err := webhookSink(cfg.webhookAddr, m)
	if err != nil {
		return fmt.Errorf("webhook sink: %w", err)
	}
	defer sink.Close()
	var hook struct {
		Webhook struct{ ID string } `json:"webhook"`
	}
	if _, err := c.do(ctx, "POST", "/api/v1/webhooks", cfg.adminToken, "", "setup", nil, map[string]any{
		"url": "http://" + cfg.webhookAddr + "/hook", "events": []string{"core.player.death"},
	}, &hook); err != nil {
		return fmt.Errorf("register webhook: %w", err)
	}

	// Setup.
	setupStart := time.Now()
	plugins := make([]*plugin, cfg.servers)
	sem := make(chan struct{}, cfg.setupParallel)
	var wg sync.WaitGroup
	var setupErr atomic.Value
	for i := range plugins {
		plugins[i] = &plugin{idx: i, cfg: cfg, prof: prof, c: newClient(base, m, 1), m: m, rng: rand.New(rand.NewPCG(uint64(i)+1, 87))}
		wg.Add(1)
		sem <- struct{}{}
		go func(p *plugin) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := p.setup(ctx); err != nil {
				setupErr.Store(err)
			}
		}(plugins[i])
	}
	wg.Wait()
	if err, _ := setupErr.Load().(error); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	panels := make([]*panelClient, cfg.panels)
	for i := range panels {
		var minted struct {
			Secret string `json:"secret"`
		}
		if _, err := c.do(ctx, "POST", "/api/v1/tokens", cfg.adminToken, "", "setup", nil,
			map[string]any{"name": fmt.Sprintf("capacity panel %d", i), "scopes": []string{"admin"}}, &minted); err != nil {
			return fmt.Errorf("mint panel token: %w", err)
		}
		panels[i] = &panelClient{idx: i, token: minted.Secret, cred: fmt.Sprintf("panel-%d", i), c: newClient(base, m, 6), m: m,
			rng: rand.New(rand.NewPCG(uint64(i)+1000, 87)), plugin: plugins}
	}
	fmt.Fprintf(os.Stderr, "loadgen: %d servers set up in %s\n", cfg.servers, time.Since(setupStart).Round(time.Millisecond))

	smp := &sampler{cgroup: cfg.cgroup, dbDir: cfg.dbDir}
	sctx, scancel := context.WithCancel(ctx)
	go smp.run(sctx)

	// Load.
	lctx, lcancel := context.WithCancel(ctx)
	var lwg sync.WaitGroup
	for _, p := range plugins {
		lwg.Add(2)
		go func(p *plugin) {
			defer lwg.Done()
			// Stagger the first polls across one cycle, as servers booted at
			// different times would be.
			time.Sleep(time.Duration(p.rng.IntN(cfg.pollTimeout*1000)) * time.Millisecond)
			p.pollLoop(lctx)
		}(p)
		go func(p *plugin) { defer lwg.Done(); p.emitter(lctx) }(p)
	}
	for _, pc := range panels {
		lwg.Add(1)
		go func(pc *panelClient) { defer lwg.Done(); pc.browse(lctx) }(pc)
	}
	verbs := []string{"heal", "teleport", "kick", "message", "repair", "refuel", "spawn", "freeze"}
	actionRate := cfg.actionsPerMin * float64(cfg.servers) / 60
	if actionRate > 0 {
		lwg.Add(1)
		go func() {
			defer lwg.Done()
			rng := rand.New(rand.NewPCG(7, 87))
			var dwg sync.WaitGroup
			defer dwg.Wait()
			for {
				// Poisson arrivals.
				wait := time.Duration(rng.ExpFloat64() / actionRate * float64(time.Second))
				select {
				case <-lctx.Done():
					return
				case <-time.After(wait):
				}
				pc := panels[rng.IntN(len(panels))]
				dwg.Add(1)
				seed := rng.Uint64()
				go func() { defer dwg.Done(); pc.dispatchOne(lctx, verbs, rand.New(rand.NewPCG(seed, 87))) }()
			}
		}()
	}

	time.Sleep(cfg.warmup)
	backlogStart := totalBacklog(m)
	from := time.Now()
	m.measuring.Store(true)
	fmt.Fprintf(os.Stderr, "loadgen: measuring for %s\n", cfg.duration)
	time.Sleep(cfg.duration)
	m.measuring.Store(false)
	to := time.Now()
	backlogEnd := totalBacklog(m)
	maxOutbox := int64(0)
	m.outboxEvents.Range(func(_, v any) bool { maxOutbox = max(maxOutbox, int64(v.(int))); return true })
	resources := smp.summary(from, to)
	lcancel()
	lwg.Wait()

	rep := report{
		Label: cfg.label, Servers: cfg.servers, Players: cfg.players, Profile: cfg.profile,
		EventsPerSec: prof.perPlayerPerSecond() * float64(cfg.players*cfg.servers),
		ActionsPerS:  actionRate, Panels: cfg.panels, WindowStart: from, WindowEnd: to,
		Latency: map[string]dist{
			"pollOverhead": m.pollOverhead.summary(), "pollEarlyRtt": m.pollEarlyRTT.summary(), "pollIngestMore": m.pollIngest.summary(),
			"actionQueueToDelivery": m.actionDeliver.summary(), "actionDispatchToCompletedSeen": m.actionResult.summary(),
			"dispatchRtt": m.dispatchRTT.summary(), "adminReadRtt": m.adminReadRTT.summary(),
			"readServersList": m.readByRoute[0].summary(), "readEventsPage": m.readByRoute[1].summary(),
			"readStatePlayers": m.readByRoute[2].summary(), "readStateVehicles": m.readByRoute[3].summary(),
			"readStateWorld": m.readByRoute[4].summary(), "actionReadRtt": m.actionReadRTT.summary(),
			"webhookEmitToReceipt": m.webhookLag.summary(), "webhookPollToReceipt": m.webhookHubLag.summary(),
			"eventEmitToAck": m.eventAckLag.summary(),
		},
		Counts: map[string]int64{
			"requests": m.requests.Load(), "failures": m.failures.Load(), "eventsSent": m.eventsSent.Load(),
			"eventsAcked": m.eventsAcked.Load(), "deathsAcked": m.deathsAcked.Load(), "snapshots": m.snapshots.Load(),
			"dispatched": m.dispatched.Load(), "delivered": m.delivered.Load(), "resultsAcked": m.results.Load(),
			"webhookDeliveries": m.webhookHits.Load(), "requestBytesSent": m.bytesSent.Load(),
		},
		Failures:    map[string]int64{},
		PluginRates: m.pluginRates.summary(from, to),
		AdminRates:  m.adminRates.summary(from, to),
		Resources:   resources,
		Backlog:     map[string]int64{"start": backlogStart, "end": backlogEnd, "maxPerServer": maxOutbox},
	}
	m.failuresBy.Range(func(k, v any) bool { rep.Failures[k.(string)] = v.(*atomic.Int64).Load(); return true })
	rep.Requests = map[string]kindReport{}
	m.kinds.Range(func(k, v any) bool {
		kc := v.(*kindCount)
		rep.Requests[k.(string)] = kindReport{Started: kc.started.Load(), Completed: kc.completed.Load(), Cancelled: kc.cancelled.Load()}
		return true
	})
	// Every admin client's five reads are due once per 5 s of the window.
	ticks := int64(to.Sub(from) / (5 * time.Second))
	for _, name := range routeNames {
		kr := rep.Requests["read "+name]
		kr.Scheduled = int64(cfg.panels) * ticks
		rep.Requests["read "+name] = kr
	}
	kr := rep.Requests["dispatch"]
	kr.Scheduled = m.dispatchArrivals.Load()
	rep.Requests["dispatch"] = kr
	// The scheduled kinds: what the schedule called for and did not get.
	var missing int64
	for _, name := range append([]string{"dispatch"}, func() []string {
		names := make([]string, 0, len(routeNames))
		for _, n := range routeNames {
			names = append(names, "read "+n)
		}
		return names
	}()...) {
		kr := rep.Requests[name]
		kr.Missing = max(0, kr.Scheduled-kr.Completed-kr.Cancelled)
		rep.Requests[name] = kr
		missing += kr.Missing
	}

	// One poll cycle's worth of events across all servers is what a healthy
	// outbox holds at any moment (2 s buffer plus the 25 s hold).
	cycleEvents := rep.EventsPerSec * float64(cfg.pollTimeout+2)
	failRate := 0.0
	if rep.Counts["requests"] > 0 {
		failRate = 100 * float64(rep.Counts["failures"]) / float64(rep.Counts["requests"])
	}
	memPct := -1.0
	if resources.PeakMemBytes > 0 {
		memPct = 100 * float64(resources.PeakMemBytes) / float64(cfg.memLimitBytes)
	}
	add := func(name string, value, limit float64, unit string, measured bool) {
		rep.Criteria = append(rep.Criteria, criterion{Name: name, Value: round1(value), Limit: limit, Unit: unit,
			Failed: measured && value > limit, Measured: measured})
	}
	// addP99 grades a latency: no samples is a failure, not "n/a".
	addP99 := func(name string, d dist, limit float64) {
		rep.Criteria = append(rep.Criteria, criterion{Name: name, Value: round1(d.P99), Limit: limit, Unit: "ms",
			Failed: d.N == 0 || d.P99 > limit, Measured: d.N > 0, Samples: d.N})
	}
	addP99("action queue-to-delivery p99", rep.Latency["actionQueueToDelivery"], 2000)
	addP99("held poll overhead p99 (RTT minus hold)", rep.Latency["pollOverhead"], 1000)
	for i, name := range routeNames {
		addP99("admin read p99: "+name, m.readByRoute[i].summary(), 1000)
	}
	add("non-2xx (429 included), transport error or timeout rate", failRate, 0.1, "%", true)
	// Every scheduled admin read and dispatch completed (review C3): a read
	// that times out drops out of its route's p99, so the p99 alone cannot
	// see it.
	add("scheduled admin requests not completed", float64(missing), 0, "", true)
	add("ingest backlog at window end / one cycle of events", float64(backlogEnd)/math.Max(cycleEvents, 1), 2, "x", true)
	addP99("webhook poll-to-receipt p99", rep.Latency["webhookPollToReceipt"], 10000)
	add("hub peak anon memory / limit", memPct, 80, "%", memPct >= 0)
	add("OOM kills", float64(resources.OOMKills), 0, "", resources.OOMKills >= 0)
	rep.Pass = true
	for _, cr := range rep.Criteria {
		if cr.Failed {
			rep.Pass = false
		}
	}

	if cfg.verify {
		rep.Verify = verify(ctx, c, cfg, plugins, hook.Webhook.ID, m)
	}

	scancel()
	out, _ := json.MarshalIndent(rep, "", "  ")
	if cfg.out != "" {
		if err := os.WriteFile(cfg.out, out, 0o644); err != nil {
			return err
		}
	}
	printSummary(rep)
	return nil
}

func totalBacklog(m *metrics) int64 {
	var n int64
	m.outboxEvents.Range(func(_, v any) bool { n += int64(v.(int)); return true })
	return n
}

func printSummary(r report) {
	verdict := "PASS"
	if !r.Pass {
		verdict = "FAIL"
	}
	fmt.Printf("%s %s servers=%d players=%d profile=%s offered=%.1f ev/s %.2f act/s\n", verdict, r.Label, r.Servers, r.Players, r.Profile, r.EventsPerSec, r.ActionsPerS)
	for _, c := range r.Criteria {
		mark := "ok  "
		if c.Failed {
			mark = "FAIL"
		} else if !c.Measured {
			mark = "n/a "
		}
		if c.Unit == "ms" && c.Samples == 0 {
			fmt.Printf("  %s %-52s none returned (limit %g ms)\n", mark, c.Name, c.Limit)
			continue
		}
		samples := ""
		if c.Samples > 0 {
			samples = fmt.Sprintf(" over %d", c.Samples)
		}
		fmt.Printf("  %s %-52s %10.1f %s%s (limit %g)\n", mark, c.Name, c.Value, c.Unit, samples, c.Limit)
	}
	fmt.Printf("  cpu mean %.0f%% p90 %.0f%%  anon peak %d MiB (cgroup %d MiB)  db %+.1f MiB/h (wal peak %d MiB)  reqs %d fails %d\n",
		r.Resources.CPUPercentMean, r.Resources.CPUPercentP90, r.Resources.PeakMemBytes>>20, r.Resources.PeakCgroupMem>>20,
		r.Resources.DBBytesPerHour/(1<<20), r.Resources.WALPeakBytes>>20, r.Counts["requests"], r.Counts["failures"])
	fmt.Printf("  plugin req/s per credential mean %.3f peak1s %d peak10s %.2f | admin mean %.3f peak1s %d peak10s %.2f\n",
		r.PluginRates.MeanRPS, r.PluginRates.Peak1sMax, r.PluginRates.Peak10sMax,
		r.AdminRates.MeanRPS, r.AdminRates.Peak1sMax, r.AdminRates.Peak10sMax)
	names := []string{"dispatch", "action read", "poll"}
	for _, n := range routeNames {
		names = append(names, "read "+n)
	}
	for _, name := range names {
		if k, ok := r.Requests[name]; ok {
			fmt.Printf("  %s: scheduled %d started %d completed %d cut off %d missing %d timeouts %d\n",
				name, k.Scheduled, k.Started, k.Completed, k.Cancelled, k.Missing, r.Failures[name+" timeout"])
		}
	}
	fmt.Printf("  cpu throttled in %.1f%% of periods, %.1f%% of the window\n", r.Resources.ThrottledPeriodsPct, r.Resources.ThrottledTimePct)
	if len(r.Failures) > 0 {
		raw, _ := json.Marshal(r.Failures)
		fmt.Printf("  failures %s\n", raw)
	}
	fmt.Printf("  latency p50/p99 ms: delivery %.1f/%.1f  completedSeen %.0f/%.0f  webhook(poll->sink) %.0f/%.0f  dispatch %.1f/%.1f  earlyPoll %.0f/%.0f\n",
		r.Latency["actionQueueToDelivery"].P50, r.Latency["actionQueueToDelivery"].P99,
		r.Latency["actionDispatchToCompletedSeen"].P50, r.Latency["actionDispatchToCompletedSeen"].P99,
		r.Latency["webhookPollToReceipt"].P50, r.Latency["webhookPollToReceipt"].P99,
		r.Latency["dispatchRtt"].P50, r.Latency["dispatchRtt"].P99,
		r.Latency["pollEarlyRtt"].P50, r.Latency["pollEarlyRtt"].P99)
	if r.Verify != nil {
		raw, _ := json.Marshal(r.Verify)
		fmt.Printf("  verify %s\n", raw)
	}
}

// verify drains every outbox and then counts, through the Admin API, what the
// hub stored against what the plugins were acked for over the whole run.
func verify(ctx context.Context, c *client, cfg config, plugins []*plugin, webhookID string, m *metrics) map[string]any {
	var wg sync.WaitGroup
	for _, p := range plugins {
		wg.Add(1)
		go func(p *plugin) { defer wg.Done(); p.drain(ctx) }(p)
	}
	wg.Wait()
	var unacked int
	for _, p := range plugins {
		unacked += len(p.outbox)
	}
	out := map[string]any{"outboxLeftAfterDrain": unacked}
	// Events stored, per server, by walking the feed.
	var stored, deathsStored int64
	for _, p := range plugins {
		cursor := ""
		for {
			path := "/api/v1/servers/" + p.serverID + "/events?limit=500"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			var page struct {
				Events []struct {
					Type string `json:"type"`
				} `json:"events"`
				NextCursor string `json:"nextCursor"`
			}
			if _, err := c.do(ctx, "GET", path, cfg.adminToken, "", "verify", nil, nil, &page); err != nil {
				out["error"] = err.Error()
				break
			}
			for _, e := range page.Events {
				stored++
				if e.Type == "core.player.death" {
					deathsStored++
				}
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	out["eventsStoredAtHub"] = stored
	out["deathsStoredAtHub"] = deathsStored
	// Every event the plugins ever emitted (warm-up included) is the
	// comparison, since the hub stores the warm-up too.
	var emitted, deaths int64
	for _, p := range plugins {
		emitted += p.emittedTotal
		deaths += p.deathsTotal
	}
	out["eventsEmittedByPlugins"] = emitted
	out["deathsEmittedByPlugins"] = deaths
	// Actions: every accepted dispatch, read back one by one (the Admin API
	// has no action list).
	completed, total := 0, 0
	dispatchedIDs.Range(func(k, _ any) bool {
		total++
		var record struct {
			State string `json:"state"`
		}
		if _, err := c.do(ctx, "GET", "/api/v1/actions/"+k.(string), cfg.adminToken, "", "verify", nil, nil, &record); err == nil && record.State == "completed" {
			completed++
		}
		return true
	})
	out["actionsDispatchedAccepted"] = total
	out["actionsCompletedAtHub"] = completed
	// Webhook deliveries: give the dispatcher time to finish, then count the
	// hub's delivery records and the sink's receipts.
	time.Sleep(10 * time.Second)
	delivered, records := 0, 0
	cursor := ""
	for {
		path := "/api/v1/webhooks/" + webhookID + "/deliveries?limit=500"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var page struct {
			Deliveries []struct {
				State string `json:"state"`
			} `json:"deliveries"`
			NextCursor string `json:"nextCursor"`
		}
		if _, err := c.do(ctx, "GET", path, cfg.adminToken, "", "verify", nil, nil, &page); err != nil {
			out["deliveriesError"] = err.Error()
			break
		}
		for _, d := range page.Deliveries {
			records++
			if d.State == "delivered" {
				delivered++
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	out["webhookDeliveryRecords"] = records
	out["webhookDeliveredAtHub"] = delivered
	out["sinkReceiptsTotal"] = sinkTotal.Load()
	return out
}

// sinkTotal counts every delivery the sink received, warm-up included, and
// dispatchedIDs every action accepted, for -verify.
var (
	sinkTotal     atomic.Int64
	dispatchedIDs sync.Map
)
