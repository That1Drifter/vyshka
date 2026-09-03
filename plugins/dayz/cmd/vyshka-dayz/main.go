// Command vyshka-dayz is the developer tool for the DayZ plugin.
//
//	vyshka-dayz build   packs plugins/dayz/mod into build/@Vyshka/addons/Vyshka.pbo
//	vyshka-dayz harness launches a DayZ dedicated server carrying the mod as a
//	                    candidate for the plugin conformance harness
//
// The harness subcommand is what makes `go run ./conformance/plugin -- go run
// ./plugins/dayz/cmd/vyshka-dayz harness` work: it takes the hub URL and
// enrollment token from the environment the harness sets, writes them into
// the plugin's config file under a fresh profile directory, starts the
// server with -serverMod, mirrors the plugin's log lines to stderr, and
// stops the server when its own stdin closes, which is how the harness asks
// a candidate to shut down.
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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/plugins/dayz/pbo"
)

const (
	prefix   = "Vyshka"
	modDir   = "@Vyshka"
	pboName  = "Vyshka.pbo"
	modCpp   = "name = \"Vyshka\";\nauthor = \"Vyshka contributors\";\noverview = \"Vyshka hub integration plugin (server side).\";\n"
	gitIgn   = "*\n"
	usageTxt = "usage: vyshka-dayz build|harness [flags]\n"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageTxt)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "build":
		err = runBuild(os.Args[2:])
	case "harness":
		err = runHarness(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usageTxt)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "vyshka-dayz:", err)
		os.Exit(1)
	}
}

// repoRelative resolves a path against the repository root when run through
// `go run` from the root, and against the working directory otherwise.
func defaultPath(rel string) string {
	return filepath.FromSlash(rel)
}

func runBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	src := fs.String("src", defaultPath("plugins/dayz/mod"), "mod source directory (config.cpp plus scripts/)")
	out := fs.String("out", defaultPath("plugins/dayz/build"), "output directory; the @Vyshka folder is created inside it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*src, "config.cpp")); err != nil {
		return fmt.Errorf("no config.cpp under %s: %w", *src, err)
	}
	addons := filepath.Join(*out, modDir, "addons")
	if err := os.MkdirAll(addons, 0o755); err != nil {
		return err
	}
	target := filepath.Join(addons, pboName)
	file, err := os.Create(target)
	if err != nil {
		return err
	}
	if err := pbo.Pack(file, *src, prefix); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, modDir, "mod.cpp"), []byte(modCpp), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, ".gitignore"), []byte(gitIgn), 0o644); err != nil {
		return err
	}
	info, _ := os.Stat(target)
	fmt.Fprintf(os.Stderr, "vyshka-dayz: wrote %s (%d bytes)\n", target, info.Size())
	return nil
}

type pluginConfig struct {
	HubURL             string `json:"hubUrl"`
	EnrollmentToken    string `json:"enrollmentToken"`
	PollTimeoutSeconds int    `json:"pollTimeoutSeconds,omitempty"`
	Game               string `json:"game,omitempty"`
}

func runHarness(args []string) error {
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	serverDir := fs.String("server", `C:\Program Files (x86)\Steam\steamapps\common\DayZServer`, "DayZ dedicated server install directory")
	mod := fs.String("mod", defaultPath("plugins/dayz/build/@Vyshka"), "built @Vyshka mod directory (see `vyshka-dayz build`)")
	mission := fs.String("mission", "dayzOffline.chernarusplus", "mission template under the server's mpmissions")
	profiles := fs.String("profiles", defaultPath("plugins/dayz/build/harness-profile"), "profile directory the server writes to; wiped of plugin state on every run")
	port := fs.Int("port", 2402, "game port for the server")
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
	profilesAbs, err := filepath.Abs(*profiles)
	if err != nil {
		return err
	}

	// Fresh plugin state: the harness issues a one-time token for a server
	// it has never seen, so credentials and an outbox from an earlier run
	// would only confuse it.
	pluginDir := filepath.Join(profilesAbs, prefix)
	if err := os.RemoveAll(pluginDir); err != nil {
		return err
	}
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}
	config, _ := json.MarshalIndent(pluginConfig{
		HubURL: *hubURL, EnrollmentToken: *token, PollTimeoutSeconds: *pollTimeout, Game: "dayz",
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(pluginDir, "config.json"), config, 0o644); err != nil {
		return err
	}

	missionName, err := prepareMission(*serverDir, *mission)
	if err != nil {
		return err
	}

	// The server config lives next to serverDZ.cfg because -config is
	// resolved against the server's working directory.
	cfgName := "vyshka_harness_serverDZ.cfg"
	cfgPath := filepath.Join(*serverDir, cfgName)
	if err := os.WriteFile(cfgPath, []byte(serverConfig(missionName)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	defer os.Remove(cfgPath)

	logStart := time.Now()
	cmd := exec.Command(exe,
		"-config="+cfgName,
		"-port="+strconv.Itoa(*port),
		"-profiles="+profilesAbs,
		"-serverMod="+modAbs,
		"-dologs", "-adminlog", "-freezecheck",
	)
	cmd.Dir = *serverDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the server: %w", err)
	}
	fmt.Fprintf(os.Stderr, "vyshka-dayz: server pid %d, profiles %s, mod %s\n", cmd.Process.Pid, profilesAbs, modAbs)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	stdinClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stdinClosed)
	}()

	stopTail := make(chan struct{})
	go tailScriptLog(profilesAbs, logStart, stopTail)

	var result error
	select {
	case err := <-exited:
		result = fmt.Errorf("the server exited on its own: %v", err)
	case <-stdinClosed:
		if *keep {
			fmt.Fprintln(os.Stderr, "vyshka-dayz: stdin closed; leaving the server running (-keep)")
			<-exited
		} else {
			fmt.Fprintln(os.Stderr, "vyshka-dayz: stdin closed; stopping the server")
			if err := killTree(cmd); err != nil {
				fmt.Fprintf(os.Stderr, "vyshka-dayz: taskkill: %v\n", err)
			}
			select {
			case <-exited:
			case <-time.After(15 * time.Second):
				// The server outlived the kill. Report it rather than return
				// success, because a survivor holds the game port and breaks
				// the next run.
				result = fmt.Errorf("the server (pid %d) did not exit after taskkill; it may still hold the game port", cmd.Process.Pid)
			}
		}
	}
	close(stopTail)
	time.Sleep(200 * time.Millisecond)
	return result
}

// prepareMission derives a harness copy of the named stock mission, once.
// The stock offline missions ship an init.c that calls LootDebug, a class a
// plain dedicated server does not have, and a mission whose init script
// fails to compile aborts the server. The copy drops that line and is
// otherwise the stock mission; it is reused on later runs.
func prepareMission(serverDir, base string) (string, error) {
	missions := filepath.Join(serverDir, "mpmissions")
	source := filepath.Join(missions, base)
	if _, err := os.Stat(filepath.Join(source, "init.c")); err != nil {
		return "", fmt.Errorf("no mission at %s: %w", source, err)
	}
	world := base
	if dot := strings.LastIndex(base, "."); dot >= 0 {
		world = base[dot+1:]
	}
	name := "vyshkaHarness." + world
	target := filepath.Join(missions, name)
	if _, err := os.Stat(filepath.Join(target, "init.c")); err == nil {
		return name, nil
	}
	fmt.Fprintf(os.Stderr, "vyshka-dayz: deriving mission %s from %s\n", name, base)
	err := filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if rel == "init.c" {
			var kept []string
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, "LootDebug") {
					continue
				}
				kept = append(kept, line)
			}
			data = []byte(strings.Join(kept, "\n"))
		}
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		// Leave no half-copied mission behind: a later run keys completeness
		// off target/init.c existing, and a partial copy would start DayZ
		// against a mission missing files.
		_ = os.RemoveAll(target)
		return "", fmt.Errorf("deriving %s: %w", target, err)
	}
	return name, nil
}

// serverConfig is a minimal dedicated-server config: one slot, no
// signature checks (the mod is unsigned and server-side), the given mission.
func serverConfig(mission string) string {
	return strings.Join([]string{
		`hostname = "vyshka-harness";`,
		`password = "";`,
		`passwordAdmin = "";`,
		`description = "Vyshka plugin conformance run";`,
		`enableWhitelist = 0;`,
		`maxPlayers = 1;`,
		`verifySignatures = 0;`,
		`forceSameBuild = 0;`,
		`disableVoN = 1;`,
		`shardId = "vysh01";`, // the engine requires exactly six characters
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

// tailScriptLog mirrors the plugin's own log lines from the newest script
// log the server writes, so a harness run shows what the plugin saw.
func tailScriptLog(profiles string, since time.Time, stop <-chan struct{}) {
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
			matches, _ := filepath.Glob(filepath.Join(profiles, "script_*.log"))
			for _, m := range matches {
				info, err := os.Stat(m)
				if err != nil || info.ModTime().Before(since.Add(-time.Minute)) {
					continue
				}
				f, err := os.Open(m)
				if err != nil {
					continue
				}
				file, reader = f, bufio.NewReader(f)
				fmt.Fprintf(os.Stderr, "vyshka-dayz: following %s\n", m)
				break
			}
			if file == nil {
				continue
			}
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
			fmt.Fprintln(os.Stderr, "  dayz:", text)
		}
	}
}

func killTree(cmd *exec.Cmd) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
