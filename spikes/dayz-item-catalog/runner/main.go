// Command runner boots a DayZ dedicated server on a mission carrying the
// item catalog probe (../harness/VyshkaCatalogProbe.c) with the Vyshka mod
// loaded, follows the script log for the probe's lines, and prints them.
//
//	go run ./spikes/dayz-item-catalog/runner [-server DIR] [-mod DIR] [-profiles DIR]
//
// The probe's lines go to stdout, tab separated, one per tree, per type,
// per sample, and a finished line; everything else goes to stderr. The exit
// code is non-zero when the probe did not finish (a phase that faults the
// process leaves its trace in the newest crash log, whose head is printed).
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

const (
	tag        = "VYSHKA_CATALOG"
	probeEntry = "\tVyshkaCatalogProbe.Run();"
	prefix     = "vyshkaCatalogSpike"
	cfgName    = "vyshka_catalog_spike_serverDZ.cfg"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		os.Exit(1)
	}
}

func run() error {
	serverDir := flag.String("server", `C:\Program Files (x86)\Steam\steamapps\common\DayZServer`, "DayZ dedicated server install directory")
	mod := flag.String("mod", "plugins/dayz/build/@Vyshka", "built @Vyshka mod directory (see `vyshka-dayz build`)")
	mission := flag.String("mission", "dayzOffline.chernarusplus", "mission template under the server's mpmissions")
	profiles := flag.String("profiles", "spikes/dayz-item-catalog/results/profile", "profile directory the server writes to")
	probePath := flag.String("probe", "spikes/dayz-item-catalog/harness/VyshkaCatalogProbe.c", "the probe appended to the mission's init.c")
	port := flag.Int("port", 2402, "game port for the server")
	timeout := flag.Duration("timeout", 10*time.Minute, "how long the boot and the probe may take together")
	flag.Parse()
	if runtime.GOOS != "windows" {
		return fmt.Errorf("the DayZ dedicated server runs on Windows only")
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(root, path)
	}
	exe := filepath.Join(*serverDir, "DayZServer_x64.exe")
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("no server binary at %s: %w", exe, err)
	}
	modAbs := resolve(*mod)
	if _, err := os.Stat(filepath.Join(modAbs, "addons", "Vyshka.pbo")); err != nil {
		return fmt.Errorf("no built mod at %s; run `go run ./plugins/dayz/cmd/vyshka-dayz build` first", modAbs)
	}
	probe, err := os.ReadFile(resolve(*probePath))
	if err != nil {
		return fmt.Errorf("reading the probe: %w", err)
	}
	profilesAbs := resolve(*profiles)
	if err := os.MkdirAll(profilesAbs, 0o755); err != nil {
		return err
	}

	missionName, err := deriveMission(*serverDir, *mission, string(probe))
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(*serverDir, cfgName)
	if err := os.WriteFile(cfgPath, []byte(serverConfig(missionName)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	defer os.Remove(cfgPath)

	earlier := map[string]bool{}
	if matches, _ := filepath.Glob(filepath.Join(profilesAbs, "script_*.log")); matches != nil {
		for _, m := range matches {
			earlier[m] = true
		}
	}

	// No -freezecheck: the walk holds the main thread on purpose, and the
	// stall is part of what is measured.
	start := time.Now()
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
	fmt.Fprintf(os.Stderr, "runner: server pid %d, profiles %s, mission %s\n", cmd.Process.Pid, profilesAbs, missionName)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	finished, followErr := follow(profilesAbs, start, earlier, exited, *timeout)
	if followErr == nil || !strings.Contains(followErr.Error(), "exited") {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
	}
	if crash := newestCrashReport(profilesAbs, start); crash != "" {
		fmt.Fprintf(os.Stderr, "runner: crash report:\n%s\n", crash)
	}
	if followErr != nil {
		return followErr
	}
	if !finished {
		return fmt.Errorf("the probe did not report finishing")
	}
	return nil
}

// follow reads the newest script log the server writes until the probe's
// finished line, the server exits, or the timeout passes. Probe lines go
// to stdout; plugin and compile lines to stderr.
func follow(profiles string, since time.Time, earlier map[string]bool, exited <-chan error, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	var file *os.File
	var reader *bufio.Reader
	var partial string
	finished := false
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
			if at := strings.Index(text, tag+"\t"); at >= 0 {
				payload := text[at+len(tag)+1:]
				fmt.Println(payload)
				if strings.HasPrefix(payload, "finished") {
					finished = true
					return true
				}
				continue
			}
			if strings.Contains(text, "[Vyshka]") || strings.Contains(text, "Can't compile") || (strings.Contains(text, "SCRIPT") && strings.Contains(text, "(E)")) {
				fmt.Fprintln(os.Stderr, "  dayz:", text)
			}
		}
	}
	for {
		select {
		case <-exited:
			consume()
			return finished, fmt.Errorf("the server exited on its own")
		case <-time.After(500 * time.Millisecond):
		}
		if file == nil {
			matches, _ := filepath.Glob(filepath.Join(profiles, "script_*.log"))
			sort.Strings(matches)
			for i := len(matches) - 1; i >= 0; i-- {
				if earlier[matches[i]] {
					continue
				}
				info, err := os.Stat(matches[i])
				if err != nil || info.ModTime().Before(since.Add(-time.Minute)) {
					continue
				}
				f, err := os.Open(matches[i])
				if err != nil {
					continue
				}
				file, reader = f, bufio.NewReader(f)
				fmt.Fprintf(os.Stderr, "runner: following %s\n", matches[i])
				break
			}
		}
		if consume() {
			return true, nil
		}
		if time.Now().After(deadline) {
			return finished, fmt.Errorf("the probe did not finish within %s", timeout)
		}
	}
}

// deriveMission copies the stock mission to <prefix>.<world> with the probe
// appended to its init.c and its entry inserted at the top of main(). The
// copy is rebuilt on every run: the probe is source.
func deriveMission(serverDir, base, probe string) (string, error) {
	missions := filepath.Join(serverDir, "mpmissions")
	source := filepath.Join(missions, base)
	if _, err := os.Stat(filepath.Join(source, "init.c")); err != nil {
		return "", fmt.Errorf("no mission at %s: %w", source, err)
	}
	world := base
	if dot := strings.LastIndex(base, "."); dot >= 0 {
		world = base[dot+1:]
	}
	name := prefix + "." + world
	target := filepath.Join(missions, name)
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	staging := target + ".tmp"
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	err := filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(staging, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if rel == "init.c" {
			edited, err := insertProbe(string(data), probe)
			if err != nil {
				return err
			}
			data = []byte(edited)
		}
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("deriving %s: %w", target, err)
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("publishing %s: %w", target, err)
	}
	return name, nil
}

// insertProbe drops the stock mission's loot debug lines (they need a
// client), puts the probe's entry at the top of main(), and appends the
// probe.
func insertProbe(initC, probe string) (string, error) {
	var result []string
	inserted, inMain := false, false
	for _, line := range strings.Split(initC, "\n") {
		if strings.Contains(line, "LootDebug") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		result = append(result, line)
		if trimmed == "void main()" {
			inMain = true
			continue
		}
		if inMain && trimmed == "{" && !inserted {
			result = append(result, probeEntry)
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

func serverConfig(mission string) string {
	return strings.Join([]string{
		`hostname = "vyshka-catalog-spike";`,
		`password = "";`,
		`passwordAdmin = "";`,
		`description = "Vyshka item catalog spike";`,
		`enableWhitelist = 0;`,
		`maxPlayers = 1;`,
		`verifySignatures = 0;`,
		`forceSameBuild = 0;`,
		`disableVoN = 1;`,
		`shardId = "vysh01";`,
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

// repoRoot walks up from the working directory to the go.mod.
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
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
