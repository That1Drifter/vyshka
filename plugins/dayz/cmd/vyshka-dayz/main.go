// Command vyshka-dayz is the developer tool for the DayZ plugin.
//
//	vyshka-dayz build        packs plugins/dayz/mod into
//	                         build/@Vyshka/addons/Vyshka.pbo
//	vyshka-dayz build-sample packs plugins/dayz/sample into
//	                         build/@VyshkaSample/addons/VyshkaSample.pbo
//	vyshka-dayz harness      launches a DayZ dedicated server carrying the mod
//	                         as a candidate for the plugin conformance harness
//	vyshka-dayz selftest     launches a DayZ dedicated server on a mission that
//	                         runs the plugin's engine-limit self-test and grades it
//
// The harness subcommand is what makes `go run ./conformance/plugin -- go run
// ./plugins/dayz/cmd/vyshka-dayz harness` work: it takes the hub URL and
// enrollment token from the environment the harness sets, writes them into
// the plugin's config file under a fresh profile directory, starts the
// server with -serverMod, mirrors the plugin's log lines to stderr, and
// stops the server when its own stdin closes, which is how the harness asks
// a candidate to shut down. Repeat -extra-mod to load further built mods
// after the plugin's own: they are appended to -serverMod in order, and the
// plugin's mod stays first, because a mod only sees the VYSHKA define when
// it loads after the mod that declares it.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
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

	"github.com/That1Drifter/vyshka/plugins/dayz/pbo"
)

const (
	prefix  = "Vyshka"
	modDir  = "@Vyshka"
	pboName = "Vyshka.pbo"
	// The sample mod: a second addon, built the same way, that shows what a
	// mod can do with the plugin's API. It carries its own prefix and folder
	// so it can be loaded beside the plugin rather than instead of it.
	samplePrefix  = "VyshkaSample"
	sampleModDir  = "@VyshkaSample"
	samplePboName = "VyshkaSample.pbo"
	gitIgn        = "*\n"
	usageTxt      = "usage: vyshka-dayz build|build-sample|version|harness|selftest [flags]\n"
	// versionFile is where the plugin states its own version, relative to
	// the mod source directory. mod.cpp and the release tag both take it
	// from there, so the manifest, the launcher, and the tag cannot drift.
	versionFile = "scripts/3_Game/Vyshka/VyshkaPlugin.c"
)

// versionPattern matches the PLUGIN_VERSION declaration in versionFile once
// the comments are gone: a whole line, so nothing else on it can pass for
// the declaration. The value is SemVer 2.0.0 without build metadata, the
// same expression that guards the release tags (scripts/release-hub.sh and
// the release workflow); change all three together.
var versionPattern = regexp.MustCompile(`(?m)^[ \t]*static const string PLUGIN_VERSION = "((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?)";[ \t]*\r?$`)

// stripComments removes block and line comments the way the engine's parser
// skips them, so a version mentioned in a comment above, beside, or around
// the real declaration is never the answer. String literals are copied
// through untouched (a "/*" inside quotes is text, not a comment), and every
// newline survives so the whole-line match still sees lines.
func stripComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"':
			out = append(out, c)
			i++
			for i < len(src) && src[i] != '"' && src[i] != '\n' {
				if src[i] == '\\' && i+1 < len(src) {
					out = append(out, src[i], src[i+1])
					i += 2
					continue
				}
				out = append(out, src[i])
				i++
			}
			if i < len(src) && src[i] == '"' {
				out = append(out, '"')
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i < len(src) && !(src[i] == '*' && i+1 < len(src) && src[i+1] == '/') {
				if src[i] == '\n' {
					out = append(out, '\n')
				}
				i++
			}
			i = min(i+2, len(src))
		default:
			out = append(out, c)
			i++
		}
	}
	return out
}

// modVersion reads the plugin version out of the mod source. Exactly one
// declaration must exist: the manifest, mod.cpp, and the release tag all
// take their number from it.
func modVersion(src string) (string, error) {
	data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(versionFile)))
	if err != nil {
		return "", err
	}
	data = stripComments(data)
	matches := versionPattern.FindAllSubmatch(data, -1)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no PLUGIN_VERSION declaration in %s (one line: static const string PLUGIN_VERSION = \"<major>.<minor>.<patch>\";)", versionFile)
	case 1:
		return string(matches[0][1]), nil
	default:
		return "", fmt.Errorf("%d PLUGIN_VERSION declarations in %s, want one", len(matches), versionFile)
	}
}

// runVersion prints the plugin version the mod source states, for the
// release workflow to compare with its tag.
func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	src := fs.String("src", defaultPath("plugins/dayz/mod"), "mod source directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	version, err := modVersion(*src)
	if err != nil {
		return err
	}
	fmt.Println(version)
	return nil
}

// modCpp is the launcher-facing description of the mod folder. It is not
// part of the PBO; the release archive carries it beside it.
func modCpp(version string) string {
	return "name = \"Vyshka\";\n" +
		"author = \"Vyshka contributors\";\n" +
		"version = \"" + version + "\";\n" +
		"overview = \"Vyshka hub integration plugin (server side). Enrolls a DayZ dedicated server with a Vyshka hub: actions, telemetry, and live state for the panel.\";\n" +
		"action = \"https://github.com/That1Drifter/vyshka\";\n"
}

// sampleModCpp describes the sample mod folder. It carries the plugin's
// version, because it ships with the plugin and is only useful beside it.
func sampleModCpp(version string) string {
	return "name = \"VyshkaSample\";\n" +
		"author = \"Vyshka contributors\";\n" +
		"version = \"" + version + "\";\n" +
		"overview = \"The sample mod for the Vyshka plugin's mod surface: it registers an action, a context, an event, and a map marker through the plugin's API. Load it after @Vyshka.\";\n" +
		"action = \"https://github.com/That1Drifter/vyshka\";\n"
}

// buildTime is the timestamp every PBO entry carries. SOURCE_DATE_EPOCH
// wins when set (the reproducible-builds convention); otherwise it is the
// commit time of the last commit that touched the mod source, which every
// clone agrees on; a tree outside git falls back to zero and says so.
func buildTime(src string) (uint32, string) {
	if raw := os.Getenv("SOURCE_DATE_EPOCH"); raw != "" {
		if n, err := strconv.ParseUint(raw, 10, 32); err == nil {
			return uint32(n), "SOURCE_DATE_EPOCH"
		}
		fmt.Fprintf(os.Stderr, "vyshka-dayz: ignoring unparsable SOURCE_DATE_EPOCH %q\n", raw)
	}
	cmd := exec.Command("git", "log", "-1", "--format=%ct", "--", ".")
	cmd.Dir = src
	if out, err := cmd.Output(); err == nil {
		if n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 32); err == nil {
			// A shallow clone cannot see past its boundary: the query
			// answers with the boundary commit, and the digest differs
			// from a full clone's. Say so rather than print a digest
			// nothing else will match.
			shallow := exec.Command("git", "rev-parse", "--is-shallow-repository")
			shallow.Dir = src
			if flag, err := shallow.Output(); err == nil && strings.TrimSpace(string(flag)) == "true" {
				fmt.Fprintln(os.Stderr, "vyshka-dayz: WARNING: shallow clone; the timestamp is the clone boundary, not the last commit touching the mod, so the digest will not match a full clone's (git fetch --unshallow, or set SOURCE_DATE_EPOCH)")
			}
			return uint32(n), "the last commit touching the mod source"
		}
	}
	return 0, "no git history and no SOURCE_DATE_EPOCH; timestamps are zero"
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageTxt)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "build":
		err = runBuild(os.Args[2:])
	case "build-sample":
		err = runBuildSample(os.Args[2:])
	case "version":
		err = runVersion(os.Args[2:])
	case "harness":
		err = runHarness(os.Args[2:])
	case "selftest":
		err = runSelfTest(os.Args[2:])
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
	version, err := modVersion(*src)
	if err != nil {
		return err
	}
	return packAddon(addon{
		src: *src, out: *out, prefix: prefix, modDir: modDir, pboName: pboName,
		version: version, modCpp: modCpp(version),
	})
}

// runBuildSample packs the sample mod the same way `build` packs the plugin,
// so the two archives are reproducible under the same rules and a release
// can carry both.
func runBuildSample(args []string) error {
	fs := flag.NewFlagSet("build-sample", flag.ContinueOnError)
	src := fs.String("src", defaultPath("plugins/dayz/sample"), "sample mod source directory (config.cpp plus scripts/)")
	out := fs.String("out", defaultPath("plugins/dayz/build"), "output directory; the @VyshkaSample folder is created inside it")
	// The sample states no version of its own: it ships with the plugin and
	// carries the plugin's number, read from the one place that states it.
	mod := fs.String("mod", defaultPath("plugins/dayz/mod"), "plugin mod source directory the version is read from")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*src, "config.cpp")); err != nil {
		return fmt.Errorf("no config.cpp under %s: %w; -src takes the sample mod source directory (plugins/dayz/sample)", *src, err)
	}
	version, err := modVersion(*mod)
	if err != nil {
		return err
	}
	return packAddon(addon{
		src: *src, out: *out, prefix: samplePrefix, modDir: sampleModDir, pboName: samplePboName,
		version: version, modCpp: sampleModCpp(version),
	})
}

// addon is one mod to pack: where its source is, where the built folder
// goes, and what names and mod.cpp it carries.
type addon struct {
	src     string
	out     string
	prefix  string
	modDir  string
	pboName string
	version string
	modCpp  string
}

// packAddon is the packing every build subcommand shares. The archive is a
// function of the source tree's bytes: fixed timestamps, LF line endings
// whatever the checkout's, sorted paths. A build of the same commit anywhere
// produces the same file, so the digest printed is what a downloaded PBO can
// be checked against.
func packAddon(a addon) error {
	modTime, timeSource := buildTime(a.src)
	addons := filepath.Join(a.out, a.modDir, "addons")
	if err := os.MkdirAll(addons, 0o755); err != nil {
		return err
	}
	var archive bytes.Buffer
	if err := pbo.Pack(&archive, a.src, a.prefix, pbo.Options{ModTime: modTime, NormalizeLineEndings: true}); err != nil {
		return err
	}
	target := filepath.Join(addons, a.pboName)
	if err := os.WriteFile(target, archive.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(a.out, a.modDir, "mod.cpp"), []byte(a.modCpp), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(a.out, ".gitignore"), []byte(gitIgn), 0o644); err != nil {
		return err
	}
	digest := sha256.Sum256(archive.Bytes())
	fmt.Fprintf(os.Stderr, "vyshka-dayz: wrote %s (%d bytes, plugin %s, timestamps %d from %s)\n", target, archive.Len(), a.version, modTime, timeSource)
	fmt.Printf("%x  %s\n", digest, a.pboName)
	return nil
}

// repeatedPath is a flag that may be given more than once, keeping every
// value in the order the command line gave them.
type repeatedPath []string

func (p *repeatedPath) String() string { return strings.Join(*p, ";") }

func (p *repeatedPath) Set(value string) error {
	if value == "" {
		return fmt.Errorf("empty path")
	}
	*p = append(*p, value)
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
	var extraMods repeatedPath
	fs.Var(&extraMods, "extra-mod", "a further built mod directory to load after the plugin's own; repeat for several (for example plugins/dayz/build/@VyshkaSample)")
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
	// The plugin's own mod leads the list and every extra follows it, in the
	// order given: a mod sees the VYSHKA define only when it loads after the
	// mod that declares it, so an extra placed first would compile without
	// the plugin's API.
	serverMod := modAbs
	for _, extra := range extraMods {
		extraAbs, err := filepath.Abs(extra)
		if err != nil {
			return err
		}
		info, err := os.Stat(extraAbs)
		if err != nil {
			return fmt.Errorf("no mod directory at %s (-extra-mod): %w", extraAbs, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("-extra-mod %s is not a directory; it takes a built mod folder such as plugins/dayz/build/@VyshkaSample", extraAbs)
		}
		if info, err := os.Stat(filepath.Join(extraAbs, "addons")); err != nil || !info.IsDir() {
			return fmt.Errorf("the mod directory %s carries no addons folder; -extra-mod takes a built mod folder such as plugins/dayz/build/@VyshkaSample (see `vyshka-dayz build-sample`)", extraAbs)
		}
		serverMod += ";" + extraAbs
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
		"-serverMod="+serverMod,
		"-dologs", "-adminlog", "-freezecheck",
	)
	cmd.Dir = *serverDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the server: %w", err)
	}
	fmt.Fprintf(os.Stderr, "vyshka-dayz: server pid %d, profiles %s, mods %s\n", cmd.Process.Pid, profilesAbs, serverMod)

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
				// The server outlived the first kill. Try once more, then
				// report failure rather than return success: a survivor holds
				// the game port and breaks the next run.
				fmt.Fprintln(os.Stderr, "vyshka-dayz: server still alive; retrying taskkill")
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

// prepareMission derives a harness copy of the named stock mission, once.
// The stock offline missions ship an init.c that calls LootDebug, a class a
// plain dedicated server does not have, and a mission whose init script
// fails to compile aborts the server. The copy drops that line and is
// otherwise the stock mission; it is reused on later runs.
func prepareMission(serverDir, base string) (string, error) {
	return deriveMission(serverDir, base, "vyshkaHarness", true, dropLootDebug)
}

// dropLootDebug is the one edit every derived mission's init.c needs.
func dropLootDebug(initC string) (string, error) {
	var kept []string
	for _, line := range strings.Split(initC, "\n") {
		if strings.Contains(line, "LootDebug") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), nil
}

// deriveMission copies the stock mission base to <prefix>.<world> under the
// server's mpmissions with its init.c passed through editInit. With reuse
// set, a copy already there is used as it is; otherwise it is rebuilt, for
// a mission whose init.c carries a script that may have changed.
func deriveMission(serverDir, base, prefix string, reuse bool, editInit func(string) (string, error)) (string, error) {
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
	// The target is built in a temp directory and renamed into place, so its
	// existence normally means a complete copy. Still key completeness off
	// init.c so a partial directory left by an older build (or an interrupted
	// non-atomic copy) is rebuilt rather than started against.
	if _, err := os.Stat(filepath.Join(target, "init.c")); err == nil && reuse {
		return name, nil
	}
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "vyshka-dayz: deriving mission %s from %s\n", name, base)
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
			edited, err := editInit(string(data))
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
