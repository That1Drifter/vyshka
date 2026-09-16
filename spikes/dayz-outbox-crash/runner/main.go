// Command runner drives the outbox crash spike end to end.
//
// It builds the hub, starts it on a loopback port with a fresh SQLite
// database, registers one server, derives a spike mission that carries the
// load generator (../harness/VyshkaOutboxLoad.c), and then loops: boot the
// DayZ server with the Vyshka mod, wait for the link and the load, kill the
// process at a random moment, read the outbox left on disk, and count on the
// next boot what the hub received of the killed run. The load of a boot starts
// only once the previous run has been delivered and acked (the runner writes a
// go file the generator waits for), so the kill schedule owes nothing to the
// recovery. The last boot delivers the last run and is stopped without load.
//
// Run from the repository root:
//
//	go run ./spikes/dayz-outbox-crash/runner -trials 8 -per-tick 5 -tick-ms 100
//
// Every trial is appended to trials.jsonl under -results and summarized on
// stdout.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	adminToken  = "outbox-spike-admin-token"
	missionName = "vyshkaOutboxSpike.chernarusplus"
	cfgName     = "vyshka_outbox_spike_serverDZ.cfg"
	eventType   = "spike.load"
)

type options struct {
	serverDir   string
	mod         string
	baseMission string
	results     string
	hubAddr     string
	port        int
	trials      int
	perTick     int
	tickMs      int
	pollTimeout int
	killMin     time.Duration
	killMax     time.Duration
	bootWait    time.Duration
	drainWait   time.Duration
	seed        int64
}

// trial is one boot that was killed, and what the boot after it delivered.
type trial struct {
	Trial       int   `json:"trial"`
	Run         int64 `json:"run"`
	PerTick     int   `json:"perTick"`
	TickMs      int   `json:"tickMs"`
	PollTimeout int   `json:"pollTimeoutSeconds"`

	// The kill: how long after the load started it was planned and when it
	// landed, and what each source said the generator had emitted when the
	// process died. The true count lies between EmittedLog (the last tick
	// confirmed after its Emit calls) and AnnouncedLog (the last tick
	// announced before them); the marker carries the announced value.
	KillPlannedMs int64 `json:"killPlannedMs"`
	KillAfterMs   int64 `json:"killAfterMs"`
	EmittedLog    int   `json:"emittedLog"`    // last confirmed n in the script log
	AnnouncedLog  int   `json:"announcedLog"`  // last announced n in the script log
	EmittedMarker int   `json:"emittedMarker"` // last announced n in the marker file

	// The outbox on disk right after the kill.
	DiskFiles int   `json:"diskFiles"`
	DiskTorn  int   `json:"diskTorn"`
	DiskBytes int64 `json:"diskBytes"`
	DiskFirst int   `json:"diskFirstN"` // lowest n on disk for this run (0 when none)
	DiskLast  int   `json:"diskLastN"`  // highest n on disk for this run
	DiskCount int   `json:"diskCount"`  // distinct n on disk for this run

	// What the hub already had of this run at the moment of the kill.
	HubBeforeKill    int `json:"hubBeforeKillCount"`
	HubBeforeKillMax int `json:"hubBeforeKillMaxN"`
	ExpectedDistinct int `json:"expectedDistinct"` // union of disk and already delivered

	// What the hub had once the next boot drained the outbox.
	HubAfterCount    int     `json:"hubAfterCount"`
	HubAfterDistinct int     `json:"hubAfterDistinct"`
	HubAfterMax      int     `json:"hubAfterMaxN"`
	HubAfterGaps     []int   `json:"hubAfterGapsBelowMax"`
	HubDuplicates    int     `json:"hubDuplicates"`
	DiskLeftAfter    int     `json:"diskLeftAfter"` // the run's events still on disk when the wait ended (0 once acked)
	DrainSeconds     float64 `json:"drainSeconds"`  // from the link connecting to delivered and acked
	DrainTimedOut    bool    `json:"drainTimedOut"`

	// Derived: events the hub never got, as an interval (see EmittedLog and
	// AnnouncedLog), and the seconds of load the upper bound spans.
	LostMin        int     `json:"lostMin"`
	LostMax        int     `json:"lostMax"`
	LostSecondsMax float64 `json:"lostSecondsMax"`
	Note           string  `json:"note,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	flag.StringVar(&o.serverDir, "server", `C:\Program Files (x86)\Steam\steamapps\common\DayZServer`, "DayZ dedicated server install directory")
	flag.StringVar(&o.mod, "mod", filepath.FromSlash("plugins/dayz/build/@Vyshka"), "built @Vyshka mod directory (see `vyshka-dayz build`)")
	flag.StringVar(&o.baseMission, "mission", "dayzOffline.chernarusplus", "stock mission the spike mission is derived from")
	flag.StringVar(&o.results, "results", filepath.FromSlash("spikes/dayz-outbox-crash/results"), "where the profile, the hub database, and trials.jsonl go")
	flag.StringVar(&o.hubAddr, "hub-addr", "127.0.0.1:8097", "loopback address the spike hub listens on")
	flag.IntVar(&o.port, "port", 2402, "game port for the server")
	flag.IntVar(&o.trials, "trials", 6, "how many boots to kill")
	flag.IntVar(&o.perTick, "per-tick", 5, "events the generator emits per tick")
	flag.IntVar(&o.tickMs, "tick-ms", 100, "generator tick period in milliseconds")
	flag.IntVar(&o.pollTimeout, "poll-timeout", 5, "pollTimeoutSeconds the plugin requests")
	flag.DurationVar(&o.killMin, "kill-min", 6*time.Second, "shortest time between the load starting and the kill")
	flag.DurationVar(&o.killMax, "kill-max", 20*time.Second, "longest time between the load starting and the kill")
	flag.DurationVar(&o.bootWait, "boot-wait", 300*time.Second, "how long a boot may take to reach a connected link")
	flag.DurationVar(&o.drainWait, "drain-wait", 120*time.Second, "how long the boot after a kill may take to deliver the killed run")
	flag.Int64Var(&o.seed, "seed", 0, "random seed for the kill delays; 0 takes the clock")
	flag.Parse()

	if runtime.GOOS != "windows" {
		return errors.New("the DayZ dedicated server runs on Windows only")
	}
	if o.trials < 1 {
		return errors.New("-trials must be at least 1")
	}
	if o.killMax < o.killMin {
		return errors.New("-kill-max must not be below -kill-min")
	}
	if o.seed == 0 {
		o.seed = time.Now().UnixNano()
	}
	rng := rand.New(rand.NewSource(o.seed))
	fmt.Printf("runner: seed %d\n", o.seed)

	root, err := repoRoot()
	if err != nil {
		return err
	}
	exe := filepath.Join(o.serverDir, "DayZServer_x64.exe")
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("no server binary at %s: %w", exe, err)
	}
	modAbs, err := filepath.Abs(o.mod)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(modAbs, "addons", "Vyshka.pbo")); err != nil {
		return fmt.Errorf("no built mod at %s; run `go run ./plugins/dayz/cmd/vyshka-dayz build` first", modAbs)
	}
	resultsAbs, err := filepath.Abs(o.results)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(resultsAbs, 0o755); err != nil {
		return err
	}

	// The hub: built under its own name so a stray Stop-Process on the
	// regular binary cannot take it down, run against a fresh database.
	hubBin := filepath.Join(os.TempDir(), "vyshka-hub-outbox-spike.exe")
	build := exec.Command("go", "build", "-o", hubBin, "./hub/cmd/vyshka-hub")
	build.Dir = root
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("building the hub: %w", err)
	}
	for _, name := range []string{"vyshka.db", "vyshka.db-shm", "vyshka.db-wal"} {
		_ = os.Remove(filepath.Join(resultsAbs, name))
	}
	hubLog, err := os.Create(filepath.Join(resultsAbs, "hub.log"))
	if err != nil {
		return err
	}
	defer hubLog.Close()
	hub := exec.Command(hubBin, "serve", "-addr", o.hubAddr, "-admin-token", adminToken, "-panel=false")
	hub.Dir = resultsAbs
	hub.Stdout, hub.Stderr = hubLog, hubLog
	if err := hub.Start(); err != nil {
		return fmt.Errorf("starting the hub: %w", err)
	}
	defer func() {
		_ = hub.Process.Kill()
		_, _ = hub.Process.Wait()
	}()
	hubURL := "http://" + o.hubAddr
	if err := waitHealthy(hubURL, 30*time.Second); err != nil {
		return err
	}
	fmt.Printf("runner: hub up at %s\n", hubURL)

	serverID, token, err := createServer(hubURL)
	if err != nil {
		return err
	}
	fmt.Printf("runner: server %s registered\n", serverID)

	// A fresh profile: credentials from this registration, an empty outbox.
	profile := filepath.Join(resultsAbs, "profile")
	if err := os.RemoveAll(profile); err != nil {
		return err
	}
	pluginDir := filepath.Join(profile, "Vyshka")
	spikeDir := filepath.Join(profile, "VyshkaSpike")
	for _, dir := range []string{pluginDir, spikeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	config := map[string]any{
		"hubUrl": hubURL, "enrollmentToken": token, "pollTimeoutSeconds": o.pollTimeout, "game": "dayz",
		// Only the load rides the outbox: snapshots and fps samples off.
		"snapshotIntervalSeconds": 0, "fpsIntervalSeconds": 0,
	}
	if err := writeJSON(filepath.Join(pluginDir, "config.json"), config); err != nil {
		return err
	}

	if err := prepareMission(o.serverDir, o.baseMission, filepath.Join(root, "spikes", "dayz-outbox-crash", "harness", "VyshkaOutboxLoad.c")); err != nil {
		return err
	}
	cfgPath := filepath.Join(o.serverDir, cfgName)
	if err := os.WriteFile(cfgPath, []byte(serverConfig(missionName)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	defer os.Remove(cfgPath)

	trialsPath := filepath.Join(resultsAbs, "trials.jsonl")
	trialsFile, err := os.OpenFile(trialsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer trialsFile.Close()

	var results []trial
	var pending *trial // the killed run the next boot has to deliver
	goPath := filepath.Join(spikeDir, "go")
	for i := 1; i <= o.trials+1; i++ {
		last := i == o.trials+1
		load := map[string]any{"tickMs": o.tickMs, "perTick": o.perTick}
		if last {
			load["perTick"] = 0
		}
		if err := writeJSON(filepath.Join(spikeDir, "load.json"), load); err != nil {
			return err
		}
		// The generator waits for this file, so the load never starts before
		// the previous boot's outbox has been delivered and acked.
		_ = os.Remove(goPath)

		fmt.Printf("runner: boot %d of %d\n", i, o.trials+1)
		bootAt := time.Now()
		srv, err := startServer(exe, o.serverDir, cfgName, o.port, profile, modAbs)
		if err != nil {
			return err
		}
		logPath, run, connectedAt, err := waitLog(profile, bootAt, srv, o.bootWait, connectedRe, "")
		if err != nil {
			_ = srv.kill()
			return fmt.Errorf("boot %d: %w", i, err)
		}
		fmt.Printf("runner: boot %d run %d, link connected after %s\n", i, run, connectedAt.Sub(bootAt).Round(time.Second))

		if pending != nil {
			// The killed run's outbox rides this boot. Wait for the hub to
			// hold everything the disk held and for the acks to have cleared
			// the run's records from disk, or give up after the drain wait.
			for {
				ns, err := hubEvents(hubURL, serverID, pending.Run)
				if err != nil {
					return err
				}
				count, distinct, maxN, gaps := analyze(ns)
				pending.HubAfterCount, pending.HubAfterDistinct, pending.HubAfterMax, pending.HubAfterGaps = count, distinct, maxN, gaps
				pending.HubDuplicates = count - distinct
				_, _, _, left := inspectOutbox(filepath.Join(pluginDir, "outbox"), pending.Run)
				pending.DiskLeftAfter = len(left)
				if distinct >= pending.ExpectedDistinct && maxN >= pending.DiskLast && len(left) == 0 {
					break
				}
				if time.Since(connectedAt) > o.drainWait {
					pending.DrainTimedOut = true
					break
				}
				time.Sleep(2 * time.Second)
			}
			pending.DrainSeconds = time.Since(connectedAt).Seconds()
			pending.LostMin = max(pending.EmittedLog-pending.HubAfterDistinct, 0)
			pending.LostMax = max(pending.AnnouncedLog-pending.HubAfterDistinct, 0)
			perSecond := float64(pending.PerTick) * 1000 / float64(pending.TickMs)
			pending.LostSecondsMax = float64(pending.LostMax) / perSecond
			if err := appendJSON(trialsFile, pending); err != nil {
				return err
			}
			results = append(results, *pending)
			printTrial(*pending)
			pending = nil
		}

		if last {
			fmt.Println("runner: final boot drained; stopping the server")
			_ = srv.kill()
			break
		}

		// The kill is scheduled from the moment the load starts, which is
		// after the recovery above, so the schedule owes nothing to it.
		if err := os.WriteFile(goPath, []byte("go\n"), 0o644); err != nil {
			return err
		}
		_, _, startedAt, err := waitLog(profile, bootAt, srv, o.bootWait, startedRe, logPath)
		if err != nil {
			_ = srv.kill()
			return fmt.Errorf("boot %d: %w", i, err)
		}
		delay := o.killMin + time.Duration(rng.Int63n(int64(o.killMax-o.killMin)+1))
		time.Sleep(time.Until(startedAt.Add(delay)))
		killAt := time.Now()
		if err := srv.kill(); err != nil {
			return fmt.Errorf("killing the server: %w", err)
		}
		t := trial{
			Trial: i, Run: run, PerTick: o.perTick, TickMs: o.tickMs, PollTimeout: o.pollTimeout,
			KillPlannedMs: delay.Milliseconds(), KillAfterMs: killAt.Sub(startedAt).Milliseconds(),
		}
		t.AnnouncedLog = lastLogged(logPath, run, intentRe)
		t.EmittedLog = lastLogged(logPath, run, emittedRe)
		t.EmittedMarker = readMarker(filepath.Join(spikeDir, "emitted.txt"), run)
		var onDisk map[int]bool
		t.DiskFiles, t.DiskTorn, t.DiskBytes, onDisk = inspectOutbox(filepath.Join(pluginDir, "outbox"), run)
		t.DiskFirst, t.DiskLast = bounds(onDisk)
		t.DiskCount = len(onDisk)
		before, err := hubEvents(hubURL, serverID, run)
		if err != nil {
			return err
		}
		t.HubBeforeKill, _, t.HubBeforeKillMax, _ = analyze(before)
		// What the next boot should leave at the hub: everything on disk
		// plus what was already delivered (the two overlap when a batch was
		// delivered but its ack never came back).
		expected := map[int]bool{}
		for n := range onDisk {
			expected[n] = true
		}
		for _, n := range before {
			expected[n] = true
		}
		t.ExpectedDistinct = len(expected)
		if t.EmittedMarker != t.AnnouncedLog {
			t.Note = "the marker file and the log's last intent line disagree"
		}
		fmt.Printf("runner: trial %d killed run %d after %d ms (planned %d): emitted %d..%d, marker %d, disk %d..%d in %d file(s) (%d torn), hub already %d\n",
			i, run, t.KillAfterMs, t.KillPlannedMs, t.EmittedLog, t.AnnouncedLog, t.EmittedMarker, t.DiskFirst, t.DiskLast, t.DiskFiles, t.DiskTorn, t.HubBeforeKill)
		pending = &t
	}

	printSummary(results)
	fmt.Printf("runner: %d trial(s) written to %s\n", len(results), trialsPath)
	return nil
}

// ---- the server process ----

type server struct {
	cmd    *exec.Cmd
	exited chan error
}

func startServer(exe, dir, cfg string, port int, profile, mod string) (*server, error) {
	cmd := exec.Command(exe,
		"-config="+cfg,
		"-port="+strconv.Itoa(port),
		"-profiles="+profile,
		"-serverMod="+mod,
		"-dologs", "-adminlog", "-freezecheck",
	)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the server: %w", err)
	}
	s := &server{cmd: cmd, exited: make(chan error, 1)}
	go func() { s.exited <- cmd.Wait() }()
	return s, nil
}

// kill terminates the process the way a crash does: TerminateProcess, no
// notice to the engine, nothing flushed that the OS did not already hold.
func (s *server) kill() error {
	select {
	case <-s.exited:
		return nil
	default:
	}
	if err := s.cmd.Process.Kill(); err != nil {
		return err
	}
	select {
	case <-s.exited:
	case <-time.After(20 * time.Second):
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(s.cmd.Process.Pid)).Run()
		select {
		case <-s.exited:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("the server (pid %d) did not exit", s.cmd.Process.Pid)
		}
	}
	// The game port is released with the process; a moment for the OS.
	time.Sleep(2 * time.Second)
	return nil
}

// seenLogs holds the script logs earlier boots were read from, so a later
// boot never matches the previous boot's lines.
var seenLogs = map[string]bool{}

var (
	connectedRe = regexp.MustCompile(`VYSHKA_LOAD\tconnected\trun=(\d+)`)
	startedRe   = regexp.MustCompile(`VYSHKA_LOAD\tstarted\trun=(\d+)`)
	intentRe    = regexp.MustCompile(`VYSHKA_LOAD\tintent\trun=(\d+)\tfirst=(\d+)\tlast=(\d+)`)
	emittedRe   = regexp.MustCompile(`VYSHKA_LOAD\temitted\trun=(\d+)\tfirst=(\d+)\tlast=(\d+)`)
)

// waitLog follows the boot's script log until a line matches re, and returns
// the log path, the run id the line carries, and when it was seen. With an
// empty logPath it first finds the log this boot created.
func waitLog(profile string, bootAt time.Time, srv *server, limit time.Duration, re *regexp.Regexp, logPath string) (string, int64, time.Time, error) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		select {
		case err := <-srv.exited:
			return "", 0, time.Time{}, fmt.Errorf("the server exited on its own: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		if logPath == "" {
			// Only a log created by this boot: the previous boot's log was
			// last written seconds before the kill, which is not long ago.
			matches, _ := filepath.Glob(filepath.Join(profile, "script_*.log"))
			for _, m := range matches {
				info, err := os.Stat(m)
				if err == nil && !info.ModTime().Before(bootAt) && !seenLogs[m] {
					logPath = m
				}
			}
			if logPath == "" {
				continue
			}
			seenLogs[logPath] = true
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("Can't compile")) || bytes.Contains(data, []byte("SCRIPT       (E):")) {
			return "", 0, time.Time{}, fmt.Errorf("script error in %s", logPath)
		}
		if m := re.FindSubmatch(data); m != nil {
			run, _ := strconv.ParseInt(string(m[1]), 10, 64)
			return logPath, run, time.Now(), nil
		}
	}
	return "", 0, time.Time{}, fmt.Errorf("no line matching %s within %s", re, limit)
}

// lastLogged reads the script log after the kill and returns the highest n
// in the run's lines matching re: what reached the log file's bytes on disk
// by the time the process died.
func lastLogged(logPath string, run int64, re *regexp.Regexp) int {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return 0
	}
	last := 0
	for _, m := range re.FindAllSubmatch(data, -1) {
		r, _ := strconv.ParseInt(string(m[1]), 10, 64)
		if r != run {
			continue
		}
		n, _ := strconv.Atoi(string(m[3]))
		if n > last {
			last = n
		}
	}
	return last
}

func readMarker(path string, run int64) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0
	}
	r, _ := strconv.ParseInt(fields[0], 10, 64)
	if r != run {
		return 0
	}
	n, _ := strconv.Atoi(fields[1])
	return n
}

// inspectOutbox reads every record in the outbox directory and reports how
// many files there are, how many are unreadable (a torn write), their
// total size, and the range and count of the run's event numbers they hold.
func inspectOutbox(dir string, run int64) (files, torn int, size int64, seen map[int]bool) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	seen = map[int]bool{}
	for _, path := range matches {
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			torn++
			continue
		}
		size += int64(len(data))
		var record struct {
			Type string `json:"type"`
			Body struct {
				Events []struct {
					T    string `json:"t"`
					Data struct {
						Run int64 `json:"run"`
						N   int   `json:"n"`
					} `json:"data"`
				} `json:"events"`
			} `json:"body"`
		}
		if len(bytes.TrimSpace(data)) == 0 || json.Unmarshal(data, &record) != nil {
			torn++
			continue
		}
		if record.Type != "event.batch" {
			continue
		}
		for _, e := range record.Body.Events {
			if e.T != eventType || e.Data.Run != run {
				continue
			}
			seen[e.Data.N] = true
		}
	}
	return
}

// bounds returns the lowest and highest members of a set, 0 and 0 when empty.
func bounds(set map[int]bool) (first, last int) {
	for n := range set {
		if first == 0 || n < first {
			first = n
		}
		if n > last {
			last = n
		}
	}
	return
}

// ---- the hub ----

func waitHealthy(hubURL string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		resp, err := http.Get(hubURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the hub at %s did not answer /healthz within %s", hubURL, limit)
}

func createServer(hubURL string) (string, string, error) {
	body, _ := json.Marshal(map[string]any{"name": "outbox-crash-spike", "game": "dayz", "enrollmentTokenTtlSeconds": 3600})
	req, _ := http.NewRequest(http.MethodPost, hubURL+"/api/v1/servers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		text, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("creating the server: %s: %s", resp.Status, text)
	}
	var created struct {
		Server     struct{ ID string }    `json:"server"`
		Enrollment struct{ Token string } `json:"enrollment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", "", err
	}
	return created.Server.ID, created.Enrollment.Token, nil
}

// hubEvents returns every n the hub holds for the run, one entry per stored
// event, so a duplicate shows up as a repeated n.
func hubEvents(hubURL, serverID string, run int64) ([]int, error) {
	var ns []int
	cursor := ""
	for {
		q := url.Values{}
		q.Set("type", eventType)
		q.Set("limit", "500")
		q.Set("since", time.Unix(run, 0).UTC().Format(time.RFC3339))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		req, _ := http.NewRequest(http.MethodGet, hubURL+"/api/v1/servers/"+serverID+"/events?"+q.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Events []struct {
				Data struct {
					Run int64 `json:"run"`
					N   int   `json:"n"`
				} `json:"data"`
			} `json:"events"`
			NextCursor string `json:"nextCursor"`
		}
		if resp.StatusCode != http.StatusOK {
			text, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("listing events: %s: %s", resp.Status, text)
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, e := range page.Events {
			if e.Data.Run == run {
				ns = append(ns, e.Data.N)
			}
		}
		if page.NextCursor == "" {
			return ns, nil
		}
		cursor = page.NextCursor
	}
}

// analyze reduces a run's received numbers to a count, the distinct count,
// the highest, and the numbers below the highest that are missing (capped).
func analyze(ns []int) (count, distinct, maxN int, gaps []int) {
	seen := map[int]bool{}
	for _, n := range ns {
		seen[n] = true
		if n > maxN {
			maxN = n
		}
	}
	count, distinct = len(ns), len(seen)
	for n := 1; n < maxN && len(gaps) < 50; n++ {
		if !seen[n] {
			gaps = append(gaps, n)
		}
	}
	sort.Ints(gaps)
	return
}

// ---- the mission and the config ----

// prepareMission derives the spike mission from the stock one: a full copy
// the first time (minus the LootDebug line a dedicated server cannot
// compile), and a rewritten init.c every time, carrying the load generator
// and its Run() call as the first statement of main().
func prepareMission(serverDir, base, loadScript string) error {
	missions := filepath.Join(serverDir, "mpmissions")
	source := filepath.Join(missions, base)
	target := filepath.Join(missions, missionName)
	if _, err := os.Stat(filepath.Join(source, "init.c")); err != nil {
		return fmt.Errorf("no mission at %s: %w", source, err)
	}
	if _, err := os.Stat(filepath.Join(target, "init.c")); err != nil {
		fmt.Printf("runner: deriving %s from %s\n", missionName, base)
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		err := filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(source, path)
			dest := filepath.Join(target, rel)
			if d.IsDir() {
				return os.MkdirAll(dest, 0o755)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(dest, data, 0o644)
		})
		if err != nil {
			return fmt.Errorf("deriving %s: %w", target, err)
		}
	}

	stock, err := os.ReadFile(filepath.Join(source, "init.c"))
	if err != nil {
		return err
	}
	load, err := os.ReadFile(loadScript)
	if err != nil {
		return err
	}
	var out []string
	inserted, inMain := false, false
	for _, line := range strings.Split(string(stock), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(line, "LootDebug") {
			continue
		}
		out = append(out, line)
		if trimmed == "void main()" {
			inMain = true
			continue
		}
		if inMain && trimmed == "{" && !inserted {
			out = append(out, "\tVyshkaOutboxLoad.Run();")
			inserted = true
		}
		if trimmed != "{" {
			inMain = false
		}
	}
	if !inserted {
		return fmt.Errorf("could not find `void main()` followed by `{` in %s", filepath.Join(source, "init.c"))
	}
	text := strings.Join(out, "\n") + "\n\n" + string(load)
	return os.WriteFile(filepath.Join(target, "init.c"), []byte(text), 0o644)
}

func serverConfig(mission string) string {
	return strings.Join([]string{
		`hostname = "vyshka-outbox-spike";`,
		`password = "";`,
		`passwordAdmin = "";`,
		`description = "Vyshka outbox crash spike";`,
		`enableWhitelist = 0;`,
		`maxPlayers = 1;`,
		`verifySignatures = 0;`,
		`forceSameBuild = 0;`,
		`disableVoN = 1;`,
		`shardId = "vysh02";`,
		`serverTime = "SystemTime";`,
		`serverTimeAcceleration = 1;`,
		`serverTimePersistent = 0;`,
		`guaranteedUpdates = 1;`,
		`loginQueueConcurrentPlayers = 1;`,
		`loginQueueMaxPlayers = 5;`,
		`instanceId = 9;`,
		`storageAutoFix = 1;`,
		`class Missions { class DayZ { template = "` + mission + `"; }; };`,
		"",
	}, "\n")
}

// ---- small helpers ----

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run from inside the repository (no go.mod found upward)")
		}
		dir = parent
	}
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func appendJSON(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

func printTrial(t trial) {
	fmt.Printf("runner: trial %d delivered: hub %d distinct of %d rows, max n %d, gaps %v, lost %d to %d event(s) = up to %.2f s of load, drained and acked %.0f s after connecting%s\n",
		t.Trial, t.HubAfterDistinct, t.HubAfterCount, t.HubAfterMax, t.HubAfterGaps, t.LostMin, t.LostMax, t.LostSecondsMax, t.DrainSeconds, timedOut(t))
}

func timedOut(t trial) string {
	if t.DrainTimedOut {
		return " (drain timed out)"
	}
	return ""
}

func printSummary(results []trial) {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	fmt.Fprintln(w, "\n| trial | kill after (planned) | emitted | on disk | files | torn | hub before | hub after | dup | gaps | lost | lost s |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|---|---|")
	for _, t := range results {
		fmt.Fprintf(w, "| %d | %.1f s (%.1f) | %d..%d | %d..%d (%d) | %d | %d | %d | %d | %d | %d | %d..%d | %.1f |\n",
			t.Trial, float64(t.KillAfterMs)/1000, float64(t.KillPlannedMs)/1000, t.EmittedLog, t.AnnouncedLog, t.DiskFirst, t.DiskLast, t.DiskCount, t.DiskFiles, t.DiskTorn,
			t.HubBeforeKill, t.HubAfterDistinct, t.HubDuplicates, len(t.HubAfterGaps), t.LostMin, t.LostMax, t.LostSecondsMax)
	}
}
