// Command runner drives the ban list pull size spike end to end.
//
// It serves the spike stub on a loopback port (synthetic ban lists in the
// shape the pull of issue #80 will answer, and raw padded bodies for the
// ceiling series), derives a spike mission that carries the probe
// (../harness/VyshkaBansProbe.c), and boots the DayZ dedicated server with
// the Vyshka mod. The probe persists its progress, so when a step takes
// the process down (the reader's line limit does, natively) the runner
// records the crash against the step that was in flight and boots again
// until the probe reports the series finished. It then copies the probe's
// lines and every crash report's script section under -results and prints
// one row per step with the phases converted to milliseconds using the
// ticks-per-millisecond ratio derived from the probe's calibration pairs.
//
// Run from the repository root:
//
//	go run ./spikes/dayz-bans-pull-size/runner
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	missionName = "vyshkaBansSpike.chernarusplus"
	cfgName     = "vyshka_bans_spike_serverDZ.cfg"
	runEntry    = "\tVyshkaBansProbe.Run();"
)

type options struct {
	serverDir   string
	mod         string
	baseMission string
	results     string
	stubAddr    string
	port        int
	wait        time.Duration
	maxBoots    int
	keep        bool
	startAt     int
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
	flag.StringVar(&o.results, "results", filepath.FromSlash("spikes/dayz-bans-pull-size/results"), "where the profile, the logs, and the table go")
	flag.StringVar(&o.stubAddr, "stub-addr", "127.0.0.1:8096", "loopback address the stub listens on; the probe has it hard-coded")
	flag.IntVar(&o.port, "port", 2402, "game port for the server")
	flag.DurationVar(&o.wait, "wait", 25*time.Minute, "how long one boot's share of the series may take")
	flag.IntVar(&o.maxBoots, "max-boots", 12, "how many boots the series may take, crashes included")
	flag.BoolVar(&o.keep, "keep", false, "leave the server running when the series ends")
	flag.IntVar(&o.startAt, "start-at", 0, "index of the first probe step to run (ban lists 0 to 7, reader line limit 8 to 15, indexing 16, raw responses 17 to 21, padded lists 22 and 23, allocation and append 24, member strings 25)")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	modAbs, err := filepath.Abs(o.mod)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(modAbs, "addons")); err != nil {
		return fmt.Errorf("no built mod at %s (run `go run ./plugins/dayz/cmd/vyshka-dayz build`): %w", modAbs, err)
	}
	exe := filepath.Join(o.serverDir, "DayZServer_x64.exe")
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("no server binary at %s: %w", exe, err)
	}
	resultsAbs, err := filepath.Abs(o.results)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(resultsAbs, 0o755); err != nil {
		return err
	}
	profile := filepath.Join(resultsAbs, "profile")
	if err := os.RemoveAll(profile); err != nil {
		return err
	}
	if err := os.MkdirAll(profile, 0o755); err != nil {
		return err
	}
	if o.startAt > 0 {
		// The probe resumes from this file, so seeding it skips the steps
		// before -start-at exactly as a crash would have.
		spikeDir := filepath.Join(profile, "VyshkaSpike")
		if err := os.MkdirAll(spikeDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(spikeDir, "progress.txt"), []byte(strconv.Itoa(o.startAt)), 0o644); err != nil {
			return err
		}
	}

	stubLog, err := os.Create(filepath.Join(resultsAbs, "stub.log"))
	if err != nil {
		return err
	}
	defer stubLog.Close()
	stub, err := startStub(o.stubAddr, stubLog)
	if err != nil {
		return err
	}
	defer stub.Close()
	fmt.Printf("runner: stub on %s\n", o.stubAddr)

	if err := prepareMission(o.serverDir, o.baseMission, filepath.Join(root, "spikes", "dayz-bans-pull-size", "harness", "VyshkaBansProbe.c")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.serverDir, cfgName), []byte(serverConfig(missionName)), 0o644); err != nil {
		return err
	}

	var allLines []string
	var crashes []crash
	unfinished := map[int]string{}
	finished := false
	var stopped error
	for boot := 1; boot <= o.maxBoots && !finished; boot++ {
		// Every script log that exists before this boot belongs to an
		// earlier one, however late it appeared; only a log created after
		// this point can be this boot's.
		earlier := existingLogs(profile)
		bootAt := time.Now()
		srv, err := startServer(exe, o.serverDir, cfgName, o.port, profile, modAbs)
		if err != nil {
			return err
		}
		fmt.Printf("runner: boot %d, pid %d\n", boot, srv.cmd.Process.Pid)

		logPath, outcome, seriesErr := waitSeries(profile, bootAt, earlier, srv, o.wait)
		if seriesErr != nil {
			fmt.Printf("runner: boot %d: %v\n", boot, seriesErr)
		}
		fmt.Printf("runner: boot %d %s\n", boot, outcome)

		if outcome == "finished" && o.keep {
			fmt.Println("runner: leaving the server running (-keep)")
		} else if err := srv.stop(); err != nil && outcome != "server-exited" {
			fmt.Printf("runner: stopping the server: %v\n", err)
		}
		time.Sleep(2 * time.Second)

		var lines []string
		if logPath != "" {
			lines, err = probeLines(logPath)
			if err != nil {
				return err
			}
		}
		allLines = append(allLines, fmt.Sprintf("# boot %d %s %s", boot, bootAt.UTC().Format(time.RFC3339), outcome))
		allLines = append(allLines, lines...)

		// A boot that ended with a step in flight (a timeout, a crash) left
		// that step unmeasured, and the next boot skips it; the table must
		// say so rather than let the step's fetch pass for a measurement. A
		// step whose completion records are all there finished before the
		// boot ended, whatever ended it, and is not marked.
		if outcome != "finished" {
			if step, inFlight := leftInFlight(lines); inFlight {
				if _, already := unfinished[step]; !already {
					unfinished[step] = outcome
				}
			}
		}

		switch outcome {
		case "finished":
			finished = true
		case "compile-failure", "aborted":
			// Nothing was measured and nothing will be: stop booting, and
			// keep the series error (not the log-reading error, which is nil
			// when the log was read fine) so the run does not report
			// success with empty tables.
			stopped = seriesErr
			if stopped == nil {
				stopped = fmt.Errorf("the series stopped: %s", outcome)
			}
		case "server-exited", "vm-exception":
			c := crash{boot: boot, outcome: outcome}
			if fire := lastFire(lines); fire != nil {
				c.step, _ = strconv.Atoi(fire.fields["step"])
				c.label = fire.fields["label"]
				c.kind = fire.fields["kind"]
			} else {
				c.step = -1
			}
			c.report = crashReport(profile, bootAt)
			crashes = append(crashes, c)
			fmt.Printf("runner: boot %d died in step %d (%s)\n", boot, c.step, c.label)
			// The line series answers its question at the first fault; skip
			// the rest of it rather than crash once per size.
			if c.kind == "line" {
				if err := skipSeries(profile, lines, "line"); err != nil {
					fmt.Printf("runner: skipping the rest of the line series: %v\n", err)
				}
			}
		default:
			// A timeout: let the next boot resume from the progress file.
		}
		if stopped != nil {
			break
		}
	}

	if err := os.WriteFile(filepath.Join(resultsAbs, "probe-script.log"), []byte(strings.Join(allLines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	var crashText strings.Builder
	for _, c := range crashes {
		fmt.Fprintf(&crashText, "boot %d: %s during step %d (%s)\n%s\n\n", c.boot, c.outcome, c.step, c.label, c.report)
	}
	if err := os.WriteFile(filepath.Join(resultsAbs, "crashes.log"), []byte(crashText.String()), 0o644); err != nil {
		return err
	}
	table := summarize(allLines, crashes, unfinished)
	fmt.Print(table)
	if err := os.WriteFile(filepath.Join(resultsAbs, "table.md"), []byte(table), 0o644); err != nil {
		return err
	}
	if stopped != nil {
		return stopped
	}
	if !finished {
		return errors.New("the series did not finish")
	}
	return nil
}

type crash struct {
	boot    int
	outcome string
	step    int
	label   string
	kind    string
	report  string
}

// skipSeries advances the probe's progress file past every remaining step of
// the given kind, using the step list the probe printed at boot in its fire
// lines. The step order is fixed in the probe: the runner only needs the
// index of the first step after the series, which it finds by scanning the
// probe source for the step table.
func skipSeries(profile string, lines []string, kind string) error {
	labels, err := probeStepKinds()
	if err != nil {
		return err
	}
	fire := lastFire(lines)
	if fire == nil {
		return errors.New("no fire line to skip from")
	}
	current, _ := strconv.Atoi(fire.fields["step"])
	next := current + 1
	for next < len(labels) && labels[next] == kind {
		next++
	}
	return os.WriteFile(filepath.Join(profile, "VyshkaSpike", "progress.txt"), []byte(strconv.Itoa(next)), 0o644)
}

var stepTableRe = regexp.MustCompile(`new VyshkaBansStep\("([^"]+)",\s*"([^"]+)"`)

// probeStepKinds reads the step kinds in order from the probe source.
func probeStepKinds() ([]string, error) {
	data, err := os.ReadFile(filepath.FromSlash("spikes/dayz-bans-pull-size/harness/VyshkaBansProbe.c"))
	if err != nil {
		return nil, err
	}
	var kinds []string
	for _, m := range stepTableRe.FindAllStringSubmatch(string(data), -1) {
		kinds = append(kinds, m[2])
	}
	return kinds, nil
}

// crashReport returns the script section of the newest crash report the
// boot produced, or a note that none was found.
func crashReport(profile string, after time.Time) string {
	matches, _ := filepath.Glob(filepath.Join(profile, "*.RPT"))
	best := ""
	var bestAt time.Time
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || info.ModTime().Before(after) {
			continue
		}
		if best == "" || info.ModTime().After(bestAt) {
			best, bestAt = m, info.ModTime()
		}
	}
	if best == "" {
		return "(no crash report)"
	}
	data, err := os.ReadFile(best)
	if err != nil {
		return "(unreadable crash report)"
	}
	text := string(data)
	idx := strings.LastIndex(text, "Fault address:")
	if idx < 0 {
		return "(crash report without a fault section: " + filepath.Base(best) + ")"
	}
	section := text[idx:]
	if end := strings.Index(section, "note: Minidump"); end > 0 {
		section = section[:end]
	}
	return filepath.Base(best) + "\n" + strings.TrimSpace(section)
}

// ---- stub ----

type stub struct {
	srv *http.Server
	ln  net.Listener
}

func startStub(addr string, logTo io.Writer) (*stub, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	var mu sync.Mutex
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(logTo, time.Now().UTC().Format("15:04:05.000")+" "+format+"\n", args...)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/bans", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n < 0 {
			n = 0
		}
		pad, _ := strconv.Atoi(r.URL.Query().Get("pad"))
		if pad < 0 {
			pad = 0
		}
		start := time.Now()
		body := banList(n, pad)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		written, err := w.Write(body)
		logf(">> GET %s n=%d bytes=%d written=%d err=%v took=%s", r.URL.RequestURI(), n, len(body), written, err, time.Since(start))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n < 16 {
			n = 16
		}
		start := time.Now()
		prefix := `{"pad":"`
		suffix := `"}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(n))
		w.WriteHeader(http.StatusOK)
		total := 0
		c, err := io.WriteString(w, prefix)
		total += c
		pad := strings.Repeat("x", 64*1024)
		remaining := n - len(prefix) - len(suffix)
		for remaining > 0 && err == nil {
			chunk := pad
			if remaining < len(chunk) {
				chunk = chunk[:remaining]
			}
			c, err = io.WriteString(w, chunk)
			total += c
			remaining -= c
		}
		if err == nil {
			c, err = io.WriteString(w, suffix)
			total += c
		}
		logf(">> GET %s n=%d written=%d err=%v took=%s", r.URL.RequestURI(), n, total, err, time.Since(start))
	})
	s := &http.Server{Handler: mux, ErrorLog: log.New(logTo, "stub: ", 0)}
	go func() { _ = s.Serve(ln) }()
	return &stub{srv: s, ln: ln}, nil
}

func (s *stub) Close() { _ = s.srv.Close() }

// banList renders n entries in the planned pull shape. Every fourth entry is
// finite; the rest are permanent. Identities match the probe's SyntheticId.
// A pad above zero adds a leading string member of that many bytes, so a
// body can be made large without adding objects: the way to tell a
// per-byte parse cost from a per-object one.
func banList(n, pad int) []byte {
	var b strings.Builder
	b.Grow(n*240 + pad + 64)
	b.WriteString(`{"revision":42,`)
	if pad > 0 {
		b.WriteString(`"pad":"`)
		b.WriteString(strings.Repeat("x", pad))
		b.WriteString(`",`)
	}
	b.WriteString(`"bans":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"banId":"01K5R%021d","identity":{"platform":"steam","id":"76561198%09d"},"name":"Player %d","reason":"repeated glitching through the base wall at the airfield","createdAt":"2026-09-21T10:00:00Z","expiresAt":`, i, i, i)
		if i%4 == 3 {
			b.WriteString(`"2027-01-01T00:00:00Z"`)
		} else {
			b.WriteString("null")
		}
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return []byte(b.String())
}

// ---- server ----

type server struct {
	cmd    *exec.Cmd
	exited chan error
}

func startServer(exe, dir, cfg string, port int, profile, mod string) (*server, error) {
	// No -freezecheck: the probe stalls the main thread on purpose, and the
	// stall is the number being measured.
	cmd := exec.Command(exe,
		"-config="+cfg,
		"-port="+strconv.Itoa(port),
		"-profiles="+profile,
		"-serverMod="+mod,
		"-dologs", "-adminlog",
	)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the server: %w", err)
	}
	s := &server{cmd: cmd, exited: make(chan error, 1)}
	go func() { s.exited <- cmd.Wait() }()
	return s, nil
}

func (s *server) exitedAlready() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

func (s *server) stop() error {
	if s.exitedAlready() {
		return nil
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
	return nil
}

var (
	finishedRe = regexp.MustCompile(`VYSHKA_BPROBE\tstep=-1\tevent=finished`)
	abortRe    = regexp.MustCompile(`VYSHKA_BPROBE\tstep=-1\tevent=abort`)
	compileRe  = regexp.MustCompile(`Can't compile`)
	vmRe       = regexp.MustCompile(`Virtual Machine Exception`)
)

// waitSeries follows the boot's script log until the probe reports it
// finished, aborted, the mission failed to compile, the VM died, the process
// exited, or the limit passed. It returns the log path and a one-word outcome.
// A crash is reported as server-exited a few seconds after the process is
// gone, once the engine has had time to write its report. Only a log that did
// not exist before the boot (earlier) can be this boot's.
func waitSeries(profile string, bootAt time.Time, earlier map[string]bool, srv *server, limit time.Duration) (string, string, error) {
	deadline := time.Now().Add(limit)
	logPath := ""
	lastStep := ""
	for {
		if logPath == "" {
			logPath = newestLog(profile, bootAt, earlier)
		}
		if logPath != "" {
			data, _ := os.ReadFile(logPath)
			text := string(data)
			if compileRe.MatchString(text) {
				return logPath, "compile-failure", errors.New("the mission did not compile; see the script log")
			}
			if finishedRe.MatchString(text) {
				return logPath, "finished", nil
			}
			if abortRe.MatchString(text) {
				return logPath, "aborted", errors.New("the probe aborted; see the script log")
			}
			if vmRe.MatchString(text) {
				return logPath, "vm-exception", errors.New("the script VM died; see the script log")
			}
			if step := lastProbeStep(text); step != lastStep {
				lastStep = step
				fmt.Printf("runner: %s  %s\n", time.Now().UTC().Format("15:04:05"), step)
			}
		}
		if srv.exitedAlready() {
			time.Sleep(5 * time.Second)
			return logPath, "server-exited", errors.New("the server process exited before the probe finished")
		}
		if time.Now().After(deadline) {
			return logPath, "timeout", fmt.Errorf("the series did not finish within %s", limit)
		}
		time.Sleep(2 * time.Second)
	}
}

// existingLogs lists the script logs in the profile directory. Taken before a
// boot starts, it is the set of logs that belong to earlier boots, including
// one an earlier boot wrote late, after its last scan and before it was
// stopped; newestLog never judges a boot on any of those.
func existingLogs(profile string) map[string]bool {
	matches, _ := filepath.Glob(filepath.Join(profile, "script_*.log"))
	known := make(map[string]bool, len(matches))
	for _, m := range matches {
		known[m] = true
	}
	return known
}

func newestLog(profile string, after time.Time, earlier map[string]bool) string {
	matches, _ := filepath.Glob(filepath.Join(profile, "script_*.log"))
	best := ""
	var bestAt time.Time
	for _, m := range matches {
		if earlier[m] {
			continue
		}
		info, err := os.Stat(m)
		if err != nil || info.ModTime().Before(after.Add(-5*time.Second)) {
			continue
		}
		if best == "" || info.ModTime().After(bestAt) {
			best, bestAt = m, info.ModTime()
		}
	}
	return best
}

var stepLineRe = regexp.MustCompile(`VYSHKA_BPROBE\tstep=(-?\d+)\tevent=(\w[\w-]*)`)

func lastProbeStep(text string) string {
	all := stepLineRe.FindAllStringSubmatch(text, -1)
	if len(all) == 0 {
		return ""
	}
	last := all[len(all)-1]
	return "step " + last[1] + " " + last[2]
}

// ---- results ----

func probeLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "VYSHKA_BPROBE") || strings.Contains(line, "Virtual Machine Exception") || strings.Contains(line, "SCRIPT    (E)") {
			lines = append(lines, line)
		}
	}
	return lines, sc.Err()
}

// printLimit is where the engine's Print cuts a line, without a marker. A
// line that long may have lost its tail, so its last field is suspect.
const printLimit = 255

type probeEvent struct {
	step   int
	event  string
	t      int
	ticks  int64
	fields map[string]string
	// suspect is the last field of a line that reached the Print limit:
	// its digits may be incomplete, and no number is made from it.
	suspect string
}

func parseEvent(line string) (probeEvent, bool) {
	idx := strings.Index(line, "VYSHKA_BPROBE")
	if idx < 0 {
		return probeEvent{}, false
	}
	parts := strings.Split(line[idx:], "\t")
	ev := probeEvent{fields: map[string]string{}}
	last := ""
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		ev.fields[k] = v
		last = k
	}
	// Only the final physical token can have lost digits. When the cut fell
	// inside the next field's name (the tail has no "="), the last field
	// with a value ended at a tab and is complete.
	if len(strings.TrimRight(line, "\r\n")) >= printLimit {
		tail := parts[len(parts)-1]
		if k, _, ok := strings.Cut(tail, "="); ok && k == last {
			ev.suspect = last
		}
	}
	ev.step, _ = strconv.Atoi(ev.fields["step"])
	ev.event = ev.fields["event"]
	ev.t, _ = strconv.Atoi(ev.fields["t"])
	ev.ticks, _ = strconv.ParseInt(ev.fields["ticks"], 10, 64)
	return ev, ev.event != ""
}

func lastFire(lines []string) *probeEvent {
	var last *probeEvent
	for _, l := range lines {
		if e, ok := parseEvent(l); ok && e.event == "fire" {
			ev := e
			last = &ev
		}
	}
	return last
}

// calibrationMaxMs bounds the intervals the fit may use: the counter is a
// signed 32-bit value, so an interval over 2^31 ticks can have wrapped and
// still read as increasing, and one that did would drag the median toward a
// fraction of the true unit. At the measured 10 MHz that is about 215 s; at
// 100 MHz, ten times faster, a full cycle is 43 s. Ten seconds is safe up to
// 200 MHz, and every fetch interval (20 ms to 6 s in every run) fits under
// it, so the fit never lacks samples.
const calibrationMaxMs = 10000

// ticksPerMs fits the counter against the frame clock: within one step, t
// is frame milliseconds since the fire and ticks is the counter, so the
// slope of ticks over t is the unit. The median over pairs between 50 ms and
// calibrationMaxMs apart is taken. Run 2 measured about 10 000 ticks per
// millisecond (a 10 MHz counter), which is the fallback when no pair
// qualifies.
func ticksPerMs(events []probeEvent) float64 {
	byStep := map[int][]probeEvent{}
	for _, e := range events {
		if e.step >= 0 && e.ticks != 0 {
			byStep[e.step] = append(byStep[e.step], e)
		}
	}
	var slopes []float64
	for _, evs := range byStep {
		first := evs[0]
		for _, e := range evs[1:] {
			span := e.t - first.t
			if span >= 50 && span <= calibrationMaxMs && e.ticks > first.ticks {
				slopes = append(slopes, float64(e.ticks-first.ticks)/float64(span))
			}
		}
	}
	if len(slopes) == 0 {
		return 10000
	}
	sort.Float64s(slopes)
	return slopes[len(slopes)/2]
}

// leftInFlight is the decision the runner takes at the end of a boot that
// did not finish: the last step fired, when its lines lack the records that
// end it, was left in flight and must be reported as not measured. A boot
// with no fire line left nothing in flight.
func leftInFlight(lines []string) (int, bool) {
	fire := lastFire(lines)
	if fire == nil {
		return 0, false
	}
	step, _ := strconv.Atoi(fire.fields["step"])
	return step, !completed(lines, step)
}

// completed reports whether a step's lines carry the records that end it:
// the fetch verdict for a raw step, the write and read for a line step, the
// two measurement lines for a ban list step (or one that reports a failed
// parse), and the final line of the diagnostic steps. A step that lacks
// them at a boot's end was left in flight.
func completed(lines []string, step int) bool {
	kind := ""
	have := map[string]bool{}
	parseFailed := false
	for _, l := range lines {
		e, ok := parseEvent(l)
		if !ok || e.step != step {
			continue
		}
		if e.event == "fire" {
			kind = e.fields["kind"]
		}
		have[e.event] = true
		if e.event == "measured" && e.fields["ok"] == "0" {
			parseFailed = true
		}
	}
	verdict := have["success"] || have["error"] || have["timeout"] || have["budget-expired"]
	switch kind {
	case "bans":
		return (have["measured"] && have["measured-more"]) || parseFailed || (verdict && !have["success"])
	case "line":
		return have["line-read"]
	case "index":
		return have["index-measured"]
	case "alloc":
		return have["alloc-get"]
	case "member":
		return have["member-append"]
	case "raw":
		return verdict
	}
	return false
}

func summarize(lines []string, crashes []crash, unfinished map[int]string) string {
	var events []probeEvent
	labels := map[int]string{}
	kinds := map[int]string{}
	for _, l := range lines {
		if e, ok := parseEvent(l); ok {
			events = append(events, e)
			if e.event == "fire" {
				labels[e.step] = e.fields["label"]
				kinds[e.step] = e.fields["kind"]
			}
		}
	}
	crashed := map[int]string{}
	for _, c := range crashes {
		crashed[c.step] = c.outcome
	}
	for step, outcome := range unfinished {
		if _, isCrash := crashed[step]; !isCrash {
			crashed[step] = "not measured: " + outcome
		}
	}
	tpm := ticksPerMs(events)
	// counterRangeMs is how long a step may take before a phase's 32-bit
	// counter difference could have wrapped more than once, at which point
	// no phase on that line can be converted honestly.
	counterRangeMs := float64(uint64(1)<<32) / tpm
	ms := func(field string, ev probeEvent) string {
		if ev.suspect == field {
			return "truncated"
		}
		if float64(ev.t) >= counterRangeMs {
			return "ambiguous"
		}
		v, err := strconv.ParseFloat(ev.fields[field], 64)
		if err != nil {
			return ev.fields[field]
		}
		if v < 0 {
			// TickCount(prev) is a 32-bit difference; a phase longer than
			// 2^31 ticks (about 215 s at 10 MHz) wraps negative once. A
			// phase longer than 2^32 ticks (about 429 s) would be reported
			// modulo that, which the frame clock check above rules out for
			// every value that reaches this line.
			v += 1 << 32
		}
		return fmt.Sprintf("%.1f", v/tpm)
	}
	var order []int
	for s := range labels {
		order = append(order, s)
	}
	sort.Ints(order)

	var b strings.Builder
	fmt.Fprintf(&b, "\nticks per ms (median slope, fallback 10000): %.1f\n\n", tpm)

	b.WriteString("## Ban lists\n\n")
	b.WriteString("| step | outcome | fetch ms | body bytes | entries | parse ms | build ms | 1000 lookups ms | serialize ms | write ms |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range order {
		if kinds[s] != "bans" {
			continue
		}
		outcome, fetch, size, measured := stepOutcome(events, s, crashed)
		if measured == nil {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | | | | | | |\n", labels[s], outcome, fetch, size)
			continue
		}
		if measured.fields["ok"] != "1" {
			fmt.Fprintf(&b, "| %s | %s (parse failed) | %s | %s | | %s | | | | |\n", labels[s], outcome, fetch, size, ms("parseTicks", *measured))
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			labels[s], outcome, fetch, size, measured.fields["entries"],
			ms("parseTicks", *measured), ms("buildTicks", *measured), ms("lookupTicks", *measured),
			ms("serializeTicks", *measured), ms("writeTicks", *measured))
	}

	b.WriteString("\n## Reader line limit\n\n")
	b.WriteString("| step | line bytes | written | write ms | read back | intact | read ms |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, s := range order {
		if kinds[s] != "line" {
			continue
		}
		var wrote, read *probeEvent
		for i := range events {
			if events[i].step != s {
				continue
			}
			switch events[i].event {
			case "line-written":
				e := events[i]
				wrote = &e
			case "line-read":
				e := events[i]
				read = &e
			}
		}
		size := ""
		for i := range events {
			if events[i].step == s && events[i].event == "fire" {
				size = events[i].fields["size"]
			}
		}
		w1, w2, r1, r2, r3 := "", "", "", "", ""
		if wrote != nil {
			w1, w2 = wrote.fields["written"], ms("writeTicks", *wrote)
		}
		if read != nil {
			r1, r2, r3 = read.fields["read"]+" ("+read.fields["len"]+" bytes)", read.fields["intact"], ms("readTicks", *read)
		} else if c, ok := crashed[s]; ok {
			r1 = "**" + c + "**"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", labels[s], size, w1, w2, r1, r2, r3)
	}

	b.WriteString("\n## Raw responses\n\n")
	b.WriteString("| step | outcome | fetch ms | body bytes | string length |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, s := range order {
		if kinds[s] != "raw" {
			continue
		}
		outcome, fetch, size, _ := stepOutcome(events, s, crashed)
		length := ""
		for i := range events {
			if events[i].step == s && events[i].event == "success" {
				length = events[i].fields["len"]
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", labels[s], outcome, fetch, size, length)
	}
	return b.String()
}

func stepOutcome(events []probeEvent, s int, crashed map[int]string) (outcome, fetch, size string, measured *probeEvent) {
	for i := range events {
		e := events[i]
		if e.step != s {
			continue
		}
		switch e.event {
		case "success":
			outcome = "success"
			fetch = strconv.Itoa(e.t)
			size = e.fields["size"]
		case "error":
			outcome = "error " + e.fields["code"]
			fetch = strconv.Itoa(e.t)
		case "timeout":
			outcome = "timeout"
			fetch = strconv.Itoa(e.t)
		case "budget-expired":
			outcome = "no callback"
			fetch = strconv.Itoa(e.t)
		case "measured":
			m := e
			if measured != nil {
				// The second line of a split measurement: merge its fields
				// and its suspect marker into the first.
				for k, v := range m.fields {
					measured.fields[k] = v
				}
				if m.suspect != "" {
					measured.suspect = m.suspect
				}
				continue
			}
			measured = &m
		case "measured-more":
			if measured != nil {
				for k, v := range e.fields {
					if k != "step" && k != "event" && k != "t" && k != "ticks" {
						measured.fields[k] = v
					}
				}
				if e.suspect != "" {
					measured.suspect = e.suspect
				}
			}
		}
	}
	if c, ok := crashed[s]; ok {
		if outcome == "" {
			outcome = "**" + c + "**"
		} else {
			outcome += ", then **" + c + "**"
		}
	}
	return outcome, fetch, size, measured
}

// ---- mission ----

func prepareMission(serverDir, base, probeScript string) error {
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
	probe, err := os.ReadFile(probeScript)
	if err != nil {
		return err
	}
	var result []string
	inserted, inMain := false, false
	for _, line := range strings.Split(string(stock), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(line, "LootDebug") {
			continue
		}
		result = append(result, line)
		if trimmed == "void main()" {
			inMain = true
			continue
		}
		if inMain && trimmed == "{" && !inserted {
			result = append(result, runEntry)
			inserted = true
		}
		if trimmed != "{" {
			inMain = false
		}
	}
	if !inserted {
		return fmt.Errorf("could not find `void main()` followed by `{` in %s", filepath.Join(source, "init.c"))
	}
	text := strings.Join(result, "\n") + "\n\n" + string(probe)
	return os.WriteFile(filepath.Join(target, "init.c"), []byte(text), 0o644)
}

func serverConfig(mission string) string {
	return strings.Join([]string{
		`hostname = "vyshka-bans-spike";`,
		`password = "";`,
		`passwordAdmin = "";`,
		`description = "Vyshka ban list pull size spike";`,
		`enableWhitelist = 0;`,
		`maxPlayers = 1;`,
		`verifySignatures = 0;`,
		`forceSameBuild = 0;`,
		`disableVoN = 1;`,
		`shardId = "vysh03";`,
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
