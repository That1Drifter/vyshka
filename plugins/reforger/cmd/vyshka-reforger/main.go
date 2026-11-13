// Command vyshka-reforger is the developer tool for the Arma Reforger plugin.
//
//	vyshka-reforger harness   launches a Reforger dedicated server carrying the
//	                          addon as a candidate for the plugin conformance
//	                          harness
//
// The harness subcommand is what makes `go run ./conformance/plugin -- go run
// ./plugins/reforger/cmd/vyshka-reforger harness` work: it takes the hub URL
// and enrollment token from the environment the harness sets, writes them
// into the plugin's config file under a fresh profile directory, starts the
// server on a world with the addon loaded the way Workbench's dedicated-server
// plugin loads one (-server, -addonsDir, -addons), mirrors the plugin's log
// lines to stderr, and stops the server when its own stdin closes, which is
// how the harness asks a candidate to shut down.
//
// A server started this way is not registered with the game's backend, so it
// reads no player identities (spikes/reforger-player-identity). The harness
// needs none: it grades the link, the manifest, and the actions.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	addonName = "Vyshka"
	usageTxt  = "usage: vyshka-reforger harness [flags]\n"
)

// guidPattern finds the addon's GUID in its .gproj, which -addons names it by.
var guidPattern = regexp.MustCompile(`(?m)^\s*GUID\s+"([0-9A-F]{16})"`)

type pluginConfig struct {
	HubURL             string `json:"hubUrl"`
	EnrollmentToken    string `json:"enrollmentToken"`
	PollTimeoutSeconds int    `json:"pollTimeoutSeconds,omitempty"`
	Game               string `json:"game,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageTxt)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "harness":
		err = runHarness(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usageTxt)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "vyshka-reforger:", err)
		os.Exit(1)
	}
}

func defaultServerDir() string {
	if dir := os.Getenv("VYSHKA_REFORGER_SERVER"); dir != "" {
		return dir
	}
	return `C:\Program Files (x86)\Steam\steamapps\common\Arma Reforger Server`
}

// addonGUID reads the GUID from the addon's project file.
func addonGUID(addonDir string) (string, error) {
	src, err := os.ReadFile(filepath.Join(addonDir, addonName+".gproj"))
	if err != nil {
		return "", err
	}
	m := guidPattern.FindSubmatch(src)
	if m == nil {
		return "", fmt.Errorf("no GUID in %s.gproj", addonName)
	}
	return string(m[1]), nil
}

func runHarness(args []string) error {
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	serverDir := fs.String("server", defaultServerDir(), "Reforger dedicated server install directory (env VYSHKA_REFORGER_SERVER)")
	addons := fs.String("addons-dir", filepath.FromSlash("plugins/reforger/addon"), "directory holding the Vyshka addon folder")
	world := fs.String("world", "worlds/GameMaster/GM_Arland.ent", "world the server loads")
	profile := fs.String("profile", filepath.FromSlash("plugins/reforger/build/harness-profile"), "profile directory the server writes to; wiped of plugin state on every run")
	hubURL := fs.String("url", os.Getenv("VYSHKA_HUB_URL"), "hub base URL (env VYSHKA_HUB_URL)")
	token := fs.String("token", os.Getenv("VYSHKA_ENROLLMENT_TOKEN"), "one-time enrollment token (env VYSHKA_ENROLLMENT_TOKEN)")
	pollTimeout := fs.Int("poll-timeout", 25, "pollTimeoutSeconds the plugin requests")
	keep := fs.Bool("keep", false, "keep the server running after stdin closes (for manual runs)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *hubURL == "" || *token == "" {
		return fmt.Errorf("need -url and -token (or VYSHKA_HUB_URL and VYSHKA_ENROLLMENT_TOKEN)")
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("the harness drives the Windows dedicated server only")
	}

	exe := filepath.Join(*serverDir, "ArmaReforgerServer.exe")
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("no server binary at %s (set -server or VYSHKA_REFORGER_SERVER): %w", exe, err)
	}
	addonsAbs, err := filepath.Abs(*addons)
	if err != nil {
		return err
	}
	guid, err := addonGUID(filepath.Join(addonsAbs, addonName))
	if err != nil {
		return fmt.Errorf("reading the addon at %s: %w", addonsAbs, err)
	}
	profileAbs, err := filepath.Abs(*profile)
	if err != nil {
		return err
	}

	// Fresh plugin state: the harness issues a one-time token for a server
	// it has never seen, so credentials and an outbox from an earlier run
	// would only confuse it. $profile: is <profile>/profile on Reforger.
	pluginDir := filepath.Join(profileAbs, "profile", addonName)
	if err := os.RemoveAll(pluginDir); err != nil {
		return err
	}
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}
	config, _ := json.MarshalIndent(pluginConfig{
		HubURL: *hubURL, EnrollmentToken: *token, PollTimeoutSeconds: *pollTimeout, Game: "reforger",
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(pluginDir, "config.json"), config, 0o644); err != nil {
		return err
	}

	logStart := time.Now()
	// The server finds its own data relative to its working directory, so it
	// runs from its install folder (spikes/reforger-player-identity).
	cmd := exec.Command(exe,
		"-server", *world,
		"-addonsDir", addonsAbs,
		"-addons", guid,
		"-profile", profileAbs,
		"-nothrow",
		"-maxFPS", "60",
	)
	cmd.Dir = *serverDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the server: %w", err)
	}
	fmt.Fprintf(os.Stderr, "vyshka-reforger: server pid %d, profile %s, addon %s\n", cmd.Process.Pid, profileAbs, guid)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	stdinClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stdinClosed)
	}()

	stopTail := make(chan struct{})
	go tailConsoleLog(profileAbs, logStart, stopTail)

	var result error
	select {
	case err := <-exited:
		result = fmt.Errorf("the server exited on its own: %v", err)
	case <-stdinClosed:
		if *keep {
			fmt.Fprintln(os.Stderr, "vyshka-reforger: stdin closed; leaving the server running (-keep)")
			<-exited
		} else {
			fmt.Fprintln(os.Stderr, "vyshka-reforger: stdin closed; stopping the server")
			if err := killTree(cmd); err != nil {
				fmt.Fprintf(os.Stderr, "vyshka-reforger: taskkill: %v\n", err)
			}
			select {
			case <-exited:
			case <-time.After(15 * time.Second):
				fmt.Fprintln(os.Stderr, "vyshka-reforger: server still alive; retrying taskkill")
				_ = killTree(cmd)
				select {
				case <-exited:
				case <-time.After(10 * time.Second):
					result = fmt.Errorf("the server (pid %d) did not exit after two taskkills; it may still hold the game port", cmd.Process.Pid)
				}
			}
		}
	}
	close(stopTail)
	time.Sleep(200 * time.Millisecond)
	return result
}

// tailConsoleLog mirrors the plugin's own log lines from the console log of
// the newest log directory the server writes (<profile>/logs/<timestamp>/).
// The server's stdout is buffered, so the file is the live view.
func tailConsoleLog(profile string, since time.Time, stop <-chan struct{}) {
	var file *os.File
	var reader *bufio.Reader
	var partial string
	for {
		select {
		case <-stop:
			if file != nil {
				drain(reader, &partial)
				file.Close()
			}
			return
		case <-time.After(500 * time.Millisecond):
		}
		if file == nil {
			// The newest log directory created since this server started:
			// an earlier run's log, written up to the moment it was killed,
			// sits in an older directory.
			matches, _ := filepath.Glob(filepath.Join(profile, "logs", "*", "console.log"))
			newest, newestTime := "", time.Time{}
			for _, m := range matches {
				info, err := os.Stat(filepath.Dir(m))
				if err != nil || info.ModTime().Before(since.Add(-2*time.Second)) {
					continue
				}
				if newest == "" || info.ModTime().After(newestTime) {
					newest, newestTime = m, info.ModTime()
				}
			}
			if newest == "" {
				continue
			}
			f, err := os.Open(newest)
			if err != nil {
				continue
			}
			file, reader = f, bufio.NewReader(f)
			fmt.Fprintf(os.Stderr, "vyshka-reforger: following %s\n", newest)
		}
		drain(reader, &partial)
	}
}

func drain(reader *bufio.Reader, partial *string) {
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			*partial += line
		}
		if err != nil {
			return
		}
		text := strings.TrimRight(*partial, "\r\n")
		*partial = ""
		if strings.Contains(text, "[Vyshka]") || strings.Contains(text, "SCRIPT") && strings.Contains(text, "(E)") || strings.Contains(text, "Can't compile") {
			fmt.Fprintln(os.Stderr, "  reforger:", text)
		}
	}
}

func killTree(cmd *exec.Cmd) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
