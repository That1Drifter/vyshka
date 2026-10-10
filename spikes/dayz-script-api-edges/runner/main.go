// Runner for the DayZ script API edges spike.
//
// It serves the stub endpoints the probe calls, derives the spike mission
// with harness/VyshkaEdgeProbe.c in its init.c, and boots the dedicated
// server until the probe reports done. While a boot runs it follows the
// script log and acts on the probe's markers: it reads the size of a file
// the probe holds open from outside the process, and kills the process when
// the probe asks. A boot that ends on its own is a fault; the probe's
// progress file decides what the next boot measures.
//
// Run from the repository root:
//
//	go run ./spikes/dayz-script-api-edges/runner
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	missionName = "vyshkaEdgeSpike.chernarusplus"
	cfgName     = "vyshka_edge_spike_serverDZ.cfg"
	marker      = "VYSHKA_EDGE\t"
)

type options struct {
	serverDir   string
	baseMission string
	results     string
	stubAddr    string
	port        int
	bootLimit   time.Duration
	maxBoots    int
	fresh       bool
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
	flag.StringVar(&o.baseMission, "mission", "dayzOffline.chernarusplus", "stock mission the spike mission is derived from")
	flag.StringVar(&o.results, "results", filepath.FromSlash("spikes/dayz-script-api-edges/results"), "where the profile and logs go")
	flag.StringVar(&o.stubAddr, "stub-addr", "127.0.0.1:8094", "loopback address of the stub; the probe has it hard-coded")
	flag.IntVar(&o.port, "port", 2402, "game port")
	flag.DurationVar(&o.bootLimit, "boot-limit", 15*time.Minute, "how long one boot may run")
	flag.IntVar(&o.maxBoots, "max-boots", 6, "boots allowed, faults included")
	flag.BoolVar(&o.fresh, "fresh", true, "clear the probe's progress before the first boot")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		return err
	}
	results := filepath.Join(root, o.results)
	profile := filepath.Join(results, "profile")
	edgeDir := filepath.Join(profile, "VyshkaEdge")
	if o.fresh {
		if err := os.RemoveAll(edgeDir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(profile, 0o755); err != nil {
		return err
	}

	stubLog, err := os.Create(filepath.Join(results, "stub.log"))
	if err != nil {
		return err
	}
	defer stubLog.Close()
	go serveStub(o.stubAddr, log.New(io.MultiWriter(stubLog, os.Stdout), "stub ", log.LstdFlags|log.Lmicroseconds))

	probe := filepath.Join(root, "spikes", "dayz-script-api-edges", "harness", "VyshkaEdgeProbe.c")
	if err := prepareMission(o.serverDir, o.baseMission, probe); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.serverDir, cfgName), []byte(serverConfig()), 0o644); err != nil {
		return err
	}

	runnerLog, err := os.Create(filepath.Join(results, "runner.log"))
	if err != nil {
		return err
	}
	defer runnerLog.Close()
	say := func(format string, args ...any) {
		line := fmt.Sprintf("%s runner: %s", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
		fmt.Println(line)
		fmt.Fprintln(runnerLog, line)
	}

	for boot := 1; boot <= o.maxBoots; boot++ {
		done, err := oneBoot(o, profile, edgeDir, boot, say)
		if err != nil {
			return err
		}
		if done {
			say("probe reported done after %d boots", boot)
			return nil
		}
	}
	return fmt.Errorf("probe not done after %d boots", o.maxBoots)
}

// oneBoot runs the server once and reports whether the probe finished.
func oneBoot(o options, profile, edgeDir string, boot int, say func(string, ...any)) (bool, error) {
	bootAt := time.Now()
	cmd := exec.Command(filepath.Join(o.serverDir, "DayZServer_x64.exe"),
		"-config="+cfgName,
		"-port="+strconv.Itoa(o.port),
		"-profiles="+profile,
		"-dologs", "-adminlog", "-freezecheck",
	)
	cmd.Dir = o.serverDir
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("starting the server: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	say("boot %d: pid %d", boot, cmd.Process.Pid)

	kill := func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(20 * time.Second):
			_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
			<-exited
		}
		time.Sleep(2 * time.Second)
	}

	size := func(name string) string {
		info, err := os.Stat(filepath.Join(edgeDir, name))
		if err != nil {
			return "missing"
		}
		return strconv.FormatInt(info.Size(), 10)
	}

	var logPath string
	var offset int64
	deadline := bootAt.Add(o.bootLimit)
	for {
		select {
		case err := <-exited:
			say("boot %d: server exited on its own after %s: %v", boot, time.Since(bootAt).Round(time.Second), err)
			return false, nil
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			kill()
			return false, fmt.Errorf("boot %d ran past %s", boot, o.bootLimit)
		}
		if logPath == "" {
			logPath = newestLog(profile, bootAt)
			if logPath == "" {
				continue
			}
			say("boot %d: script log %s", boot, filepath.Base(logPath))
		}
		lines, next := readFrom(logPath, offset)
		offset = next
		for _, line := range lines {
			if strings.Contains(line, "Can't compile") || strings.Contains(line, "SCRIPT       (E):") {
				kill()
				return false, fmt.Errorf("script error: %s", strings.TrimSpace(line))
			}
			i := strings.Index(line, marker)
			if i < 0 {
				continue
			}
			probeLine := line[i:]
			say("probe %s", strings.ReplaceAll(strings.TrimSpace(probeLine), "\t", " "))
			switch {
			case strings.Contains(probeLine, "event=unclosed-written"):
				say("outside: unclosed.txt size=%s right after the write", size("unclosed.txt"))
				time.Sleep(3 * time.Second)
				say("outside: unclosed.txt size=%s 3 s after the write, handle still open", size("unclosed.txt"))
			case strings.Contains(probeLine, "event=unclosed-closed"):
				say("outside: unclosed.txt size=%s after CloseFile", size("unclosed.txt"))
			case strings.Contains(probeLine, "event=kill-now"):
				say("outside: killed.txt size=%s before the kill, handle open", size("killed.txt"))
				kill()
				say("outside: killed.txt size=%s after TerminateProcess", size("killed.txt"))
				return false, nil
			case strings.Contains(probeLine, "event=done"):
				kill()
				return true, nil
			}
		}
	}
}

// newestLog returns a script log this boot created, or "".
func newestLog(profile string, bootAt time.Time) string {
	matches, _ := filepath.Glob(filepath.Join(profile, "script_*.log"))
	best := ""
	var bestTime time.Time
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || info.ModTime().Before(bootAt) {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = m, info.ModTime()
		}
	}
	return best
}

// readFrom returns the complete lines after offset and the new offset.
func readFrom(path string, offset int64) ([]string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset
	}
	var lines []string
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			// A partial last line is read again next time.
			return lines, offset
		}
		offset += int64(len(line))
		lines = append(lines, line)
	}
}

// ---- the stub ----

func serveStub(addr string, l *log.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		l.Printf("%s %s -> 200", r.Method, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(r.URL.Query().Get("code"))
		if err != nil {
			code = 200
		}
		l.Printf("%s %s -> %d", r.Method, r.URL.RequestURI(), code)
		w.Header().Set("Content-Type", "application/json")
		if code >= 300 && code < 400 {
			w.Header().Set("Location", "/ok")
		}
		w.WriteHeader(code)
		if code != http.StatusNoContent && code != http.StatusNotModified {
			fmt.Fprintf(w, `{"status":%d}`, code)
		}
	})
	mux.HandleFunc("/hold", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		start := time.Now()
		l.Printf("%s %s arrived", r.Method, r.URL.RequestURI())
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"heldMs":%d}`, ms)
			l.Printf("%s answered after %.3fs", r.URL.RequestURI(), time.Since(start).Seconds())
		case <-r.Context().Done():
			l.Printf("%s client gone after %.3fs", r.URL.RequestURI(), time.Since(start).Seconds())
		}
	})
	if err := http.ListenAndServe(addr, mux); err != nil {
		l.Fatalf("stub: %v", err)
	}
}

// ---- the mission and the config ----

// prepareMission derives the spike mission from the stock one: a full copy
// the first time (minus the LootDebug line a dedicated server cannot
// compile), and a rewritten init.c every time, with the probe appended and
// its Run() call as the first statement of main().
func prepareMission(serverDir, base, probe string) error {
	missions := filepath.Join(serverDir, "mpmissions")
	source := filepath.Join(missions, base)
	target := filepath.Join(missions, missionName)
	if _, err := os.Stat(filepath.Join(source, "init.c")); err != nil {
		return fmt.Errorf("no mission at %s: %w", source, err)
	}
	if _, err := os.Stat(filepath.Join(target, "init.c")); err != nil {
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
	code, err := os.ReadFile(probe)
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
			out = append(out, "\tVyshkaEdgeProbe.Run();")
			inserted = true
		}
		if trimmed != "{" {
			inMain = false
		}
	}
	if !inserted {
		return fmt.Errorf("could not find `void main()` followed by `{` in %s", filepath.Join(source, "init.c"))
	}
	text := strings.Join(out, "\n") + "\n\n" + string(code)
	return os.WriteFile(filepath.Join(target, "init.c"), []byte(text), 0o644)
}

func serverConfig() string {
	return strings.Join([]string{
		`hostname = "vyshka-edge-spike";`,
		`password = "";`,
		`passwordAdmin = "";`,
		`description = "Vyshka script API edges spike";`,
		`enableWhitelist = 0;`,
		`maxPlayers = 1;`,
		`verifySignatures = 0;`,
		`forceSameBuild = 0;`,
		`disableVoN = 1;`,
		`shardId = "vysh05";`,
		`serverTime = "SystemTime";`,
		`serverTimeAcceleration = 1;`,
		`serverTimePersistent = 0;`,
		`guaranteedUpdates = 1;`,
		`loginQueueConcurrentPlayers = 1;`,
		`loginQueueMaxPlayers = 5;`,
		`instanceId = 9;`,
		`storageAutoFix = 1;`,
		`class Missions { class DayZ { template = "` + missionName + `"; }; };`,
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
