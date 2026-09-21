package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The self-test (plugins/dayz/selftest/VyshkaSelfTest.c) grades what the
// conformance harness cannot see from the wire: the plugin's own file and
// JSON classes against the engine's limits, inside a real server. The
// subcommand derives a mission that carries the probe, boots the server on
// it, follows the script log for the probe's lines, and reports them the
// way the conformance suites do, one line per check and a non-zero exit
// when any failed, when the probe did not finish (a fault takes the whole
// process, which is exactly the failure the file checks exist to catch),
// or when the server exited on its own.

const (
	selfTestTag   = "VYSHKA_SELFTEST"
	selfTestEntry = "\tVyshkaSelfTest.Run();"
)

type selfTestResult struct {
	Check  string
	Passed bool
	Detail string
}

// selfTestReport is what the probe's lines add up to.
type selfTestReport struct {
	Plan     int
	Results  []selfTestResult
	Finished bool
}

// consume reads one script-log line and records the probe line in it, if
// any; it reports whether the probe has said it is finished.
func (r *selfTestReport) consume(line string) bool {
	at := strings.Index(line, selfTestTag+"\t")
	if at < 0 {
		return r.Finished
	}
	fields := strings.Split(line[at+len(selfTestTag)+1:], "\t")
	switch {
	case strings.HasPrefix(fields[0], "plan="):
		r.Plan, _ = strconv.Atoi(strings.TrimPrefix(fields[0], "plan="))
	case strings.HasPrefix(fields[0], "check="):
		result := selfTestResult{Check: strings.TrimPrefix(fields[0], "check=")}
		if len(fields) > 1 {
			result.Passed = fields[1] == "result=PASS"
		}
		if len(fields) > 2 {
			result.Detail = strings.Join(fields[2:], " ")
		}
		r.Results = append(r.Results, result)
	case fields[0] == "finished":
		r.Finished = true
	}
	return r.Finished
}

// verdict is nil when every planned check ran and passed.
func (r *selfTestReport) verdict() error {
	var failed []string
	for _, result := range r.Results {
		if !result.Passed {
			failed = append(failed, result.Check)
		}
	}
	switch {
	case len(failed) > 0:
		return fmt.Errorf("%d check(s) failed: %s", len(failed), strings.Join(failed, ", "))
	case !r.Finished:
		return fmt.Errorf("the probe did not report finishing; %d of %d planned check(s) reported", len(r.Results), r.Plan)
	case r.Plan == 0 || len(r.Results) != r.Plan:
		return fmt.Errorf("the probe planned %d check(s) and reported %d", r.Plan, len(r.Results))
	}
	return nil
}

func (r *selfTestReport) print(target string) {
	fmt.Printf("selftest: plugin self-test on %s\n\n", target)
	for _, result := range r.Results {
		status := "FAIL"
		if result.Passed {
			status = "PASS"
		}
		fmt.Printf("%s  %-22s %s\n", status, result.Check, result.Detail)
	}
	failed := 0
	for _, result := range r.Results {
		if !result.Passed {
			failed++
		}
	}
	fmt.Printf("\n%d checks, %d failed\n", len(r.Results), failed)
}

// insertProbe appends the probe script to a mission's init.c and calls its
// entry point first thing in main(), after the LootDebug line is dropped.
func insertProbe(initC, probe string) (string, error) {
	initC, err := dropLootDebug(initC)
	if err != nil {
		return "", err
	}
	var result []string
	inserted, inMain := false, false
	for _, line := range strings.Split(initC, "\n") {
		trimmed := strings.TrimSpace(line)
		result = append(result, line)
		if trimmed == "void main()" {
			inMain = true
			continue
		}
		if inMain && trimmed == "{" && !inserted {
			result = append(result, selfTestEntry)
			inserted = true
		}
		if trimmed != "{" {
			inMain = false
		}
	}
	if !inserted {
		return "", fmt.Errorf("could not find `void main()` followed by `{` in the mission's init.c")
	}
	return strings.Join(result, "\n") + "\n\n" + probe, nil
}

func runSelfTest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	serverDir := fs.String("server", `C:\Program Files (x86)\Steam\steamapps\common\DayZServer`, "DayZ dedicated server install directory")
	mod := fs.String("mod", defaultPath("plugins/dayz/build/@Vyshka"), "built @Vyshka mod directory (see `vyshka-dayz build`)")
	mission := fs.String("mission", "dayzOffline.chernarusplus", "mission template under the server's mpmissions")
	profiles := fs.String("profiles", defaultPath("plugins/dayz/build/selftest-profile"), "profile directory the server writes to; wiped of plugin state on every run")
	probePath := fs.String("probe", defaultPath("plugins/dayz/selftest/VyshkaSelfTest.c"), "the self-test script appended to the mission's init.c")
	port := fs.Int("port", 2402, "game port for the server")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long the boot and the checks may take together")
	keep := fs.Bool("keep", false, "keep the server running after the checks (for a look at the profile directory)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("the DayZ dedicated server runs on Windows only")
	}
	exe := filepath.Join(*serverDir, "DayZServer_x64.exe")
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("no server binary at %s: %w", exe, err)
	}
	modAbs, err := filepath.Abs(*mod)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(modAbs, "addons", pboName)); err != nil {
		return fmt.Errorf("no built mod at %s; run `go run ./plugins/dayz/cmd/vyshka-dayz build` first", modAbs)
	}
	probe, err := os.ReadFile(*probePath)
	if err != nil {
		return fmt.Errorf("reading the probe: %w", err)
	}
	profilesAbs, err := filepath.Abs(*profiles)
	if err != nil {
		return err
	}

	// The probe writes under the plugin's own directory, through the
	// plugin's own classes; it starts from nothing, as a fresh install does.
	pluginDir := filepath.Join(profilesAbs, prefix)
	if err := os.RemoveAll(pluginDir); err != nil {
		return err
	}
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}

	// The mission is rebuilt on every run: the probe is source, and a stale
	// copy would grade the last version of it.
	missionName, err := deriveMission(*serverDir, *mission, "vyshkaSelfTest", false, func(initC string) (string, error) {
		return insertProbe(initC, string(probe))
	})
	if err != nil {
		return err
	}

	cfgName := "vyshka_selftest_serverDZ.cfg"
	cfgPath := filepath.Join(*serverDir, cfgName)
	if err := os.WriteFile(cfgPath, []byte(serverConfig(missionName)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	defer os.Remove(cfgPath)

	// No -freezecheck: the checks hold the main thread on purpose, and the
	// stall is part of what they measure.
	logStart := time.Now()
	cmd := exec.Command(exe,
		"-config="+cfgName,
		"-port="+strconv.Itoa(*port),
		"-profiles="+profilesAbs,
		"-serverMod="+modAbs,
		"-dologs", "-adminlog",
	)
	cmd.Dir = *serverDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the server: %w", err)
	}
	fmt.Fprintf(os.Stderr, "vyshka-dayz: server pid %d, profiles %s, mission %s\n", cmd.Process.Pid, profilesAbs, missionName)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	report := &selfTestReport{}
	outcome := followSelfTest(profilesAbs, logStart, report, exited, *timeout)

	if *keep {
		fmt.Fprintln(os.Stderr, "vyshka-dayz: leaving the server running (-keep)")
	} else if outcome != errServerExited {
		if err := killTree(cmd); err != nil {
			fmt.Fprintf(os.Stderr, "vyshka-dayz: taskkill: %v\n", err)
		}
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			fmt.Fprintln(os.Stderr, "vyshka-dayz: server still alive; retrying taskkill")
			_ = killTree(cmd)
		}
	}

	report.print(exe)
	if outcome == errServerExited {
		if crash := newestCrashReport(profilesAbs, logStart); crash != "" {
			fmt.Fprintf(os.Stderr, "vyshka-dayz: crash report:\n%s\n", crash)
		}
		return fmt.Errorf("the server exited on its own before the probe finished; %d of %d planned check(s) reported", len(report.Results), report.Plan)
	}
	if outcome != nil {
		return outcome
	}
	return report.verdict()
}

var errServerExited = fmt.Errorf("the server exited")

// followSelfTest reads the newest script log the server writes until the
// probe reports finishing, the server exits, or the timeout passes. Every
// probe line and every plugin log line is mirrored to stderr as it lands.
func followSelfTest(profiles string, since time.Time, report *selfTestReport, exited <-chan error, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var file *os.File
	var reader *bufio.Reader
	var partial string
	defer func() {
		if file != nil {
			file.Close()
		}
	}()
	consume := func() bool {
		if reader == nil {
			return false
		}
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				partial += line
			}
			if err != nil {
				return false
			}
			text := strings.TrimRight(partial, "\r\n")
			partial = ""
			if strings.Contains(text, selfTestTag) || strings.Contains(text, "[Vyshka]") || strings.Contains(text, "Can't compile") || (strings.Contains(text, "SCRIPT") && strings.Contains(text, "(E)")) {
				fmt.Fprintln(os.Stderr, "  dayz:", text)
			}
			if report.consume(text) {
				return true
			}
		}
	}
	for {
		select {
		case <-exited:
			// The log may hold lines written just before the exit.
			consume()
			return errServerExited
		case <-time.After(500 * time.Millisecond):
		}
		if file == nil {
			matches, _ := filepath.Glob(filepath.Join(profiles, "script_*.log"))
			sort.Strings(matches)
			for i := len(matches) - 1; i >= 0; i-- {
				info, err := os.Stat(matches[i])
				if err != nil || info.ModTime().Before(since.Add(-time.Minute)) {
					continue
				}
				f, err := os.Open(matches[i])
				if err != nil {
					continue
				}
				file, reader = f, bufio.NewReader(f)
				fmt.Fprintf(os.Stderr, "vyshka-dayz: following %s\n", matches[i])
				break
			}
		}
		if consume() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the probe did not finish within %s; %d of %d planned check(s) reported", timeout, len(report.Results), report.Plan)
		}
	}
}

// newestCrashReport returns the head of the newest crash log the engine
// wrote after the run started, or "" when there is none: a check that
// faults the process leaves its trace there and nowhere else.
func newestCrashReport(profiles string, since time.Time) string {
	matches, _ := filepath.Glob(filepath.Join(profiles, "crash_*.log"))
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0; i-- {
		info, err := os.Stat(matches[i])
		if err != nil || info.ModTime().Before(since) {
			continue
		}
		data, err := os.ReadFile(matches[i])
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) > 40 {
			lines = lines[:40]
		}
		return matches[i] + "\n" + strings.Join(lines, "\n")
	}
	return ""
}
