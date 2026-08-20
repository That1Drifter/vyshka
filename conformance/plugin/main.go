// Command conformance-plugin grades a candidate plugin against the Vyshka
// protocol. It is the mock hub spec section 14 promises: the candidate points
// at it instead of a real hub, and it drives the plugin through enrollment,
// sessions, manifest publish, action round-trips, forced re-delivery, a
// transport outage, a session change with envelopes still unacked, and a
// schema-invalid dispatch.
//
// It is deliberately a black-box grader: it serves HTTP and watches what the
// plugin does, so it can grade any implementation, not just this repository's.
//
// Usage, launching the candidate itself (the URL and enrollment token are
// passed in the environment as VYSHKA_HUB_URL and VYSHKA_ENROLLMENT_TOKEN):
//
//	go run ./conformance/plugin -- go run ./conformance/plugin/driver
//
// Or against a candidate started by hand: run with no command, point the
// candidate at the printed URL and token, and the suite begins when it
// enrolls.
//
// Exit code is 0 when every check passes, 1 when any check fails, and 2 when
// the suite could not be run at all.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// killTree is the forced-shutdown fallback behind the cooperative stdin-close.
// On Windows, Kill reaches only the direct child, and a wrapper like go run
// would leave the real plugin running, so taskkill fells the whole tree. On
// other platforms Kill is used as-is; a candidate started through a wrapper
// there should treat stdin EOF as shutdown, which is the documented contract.
func killTree(candidate *exec.Cmd) {
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(candidate.Process.Pid)).Run()
		return
	}
	_ = candidate.Process.Kill()
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address the mock hub listens on")
	enrollWait := flag.Duration("enroll-wait", 60*time.Second, "how long to wait for the candidate to enroll")
	checkTimeout := flag.Duration("check-timeout", 20*time.Second, "budget for each wait inside a check")
	asJSON := flag.Bool("json", false, "emit machine-readable results")
	flag.Parse()
	command := flag.Args()

	hub, err := startMockHub(*listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(2)
	}
	defer hub.Close()

	fmt.Fprintf(os.Stderr, "conformance: mock hub listening at %s\n", hub.baseURL)

	target := "externally launched plugin"
	var candidate *exec.Cmd
	var candidateStdin io.WriteCloser
	if len(command) > 0 {
		target = strings.Join(command, " ")
		candidate = exec.Command(command[0], command[1:]...)
		candidate.Env = append(os.Environ(),
			"VYSHKA_HUB_URL="+hub.baseURL,
			"VYSHKA_ENROLLMENT_TOKEN="+hub.enrollmentToken,
		)
		// The candidate's own output goes to stderr so a -json report on
		// stdout stays machine-readable.
		candidate.Stdout = os.Stderr
		candidate.Stderr = os.Stderr
		candidateStdin, err = candidate.StdinPipe()
		if err != nil {
			fmt.Fprintln(os.Stderr, "conformance: candidate stdin:", err)
			os.Exit(2)
		}
		if err := candidate.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "conformance: launch %q: %v\n", target, err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "conformance: launched candidate: %s\n", target)
		go func() {
			err := candidate.Wait()
			if err != nil {
				hub.notePluginExit(err.Error())
			} else {
				hub.notePluginExit("exit status 0")
			}
		}()
	} else {
		fmt.Fprintf(os.Stderr, "conformance: no candidate command given; start one against\n")
		fmt.Fprintf(os.Stderr, "  VYSHKA_HUB_URL=%s\n", hub.baseURL)
		fmt.Fprintf(os.Stderr, "  VYSHKA_ENROLLMENT_TOKEN=%s\n", hub.enrollmentToken)
		fmt.Fprintf(os.Stderr, "conformance: waiting up to %s for it to enroll\n", *enrollWait)
	}

	results := runStages(&harness{
		hub:          hub,
		enrollWait:   *enrollWait,
		checkTimeout: *checkTimeout,
	})

	// A candidate that died on its own is a failure even when every stage's
	// assertion had already passed by the time it died: a plugin that crashes
	// on its way out would crash a game server the same way.
	if candidate != nil {
		hub.mu.Lock()
		exitedEarly, exitMessage := hub.pluginExited, hub.pluginExitMsg
		hub.mu.Unlock()
		if exitedEarly {
			results = append(results, Result{
				ID: "candidate.exit", Title: "The candidate outlives the run", Section: "14",
				Error: "the candidate exited before the suite finished (" + exitMessage + ")",
			})
		}
	}

	// A candidate that exits when its stdin closes shuts down cleanly here;
	// one that does not is killed.
	if candidate != nil {
		_ = candidateStdin.Close()
		stopped := make(chan struct{})
		go func() {
			hub.await(3*time.Second, "the candidate to exit", func() bool { return hub.pluginExited })
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
		}
		hub.mu.Lock()
		exited := hub.pluginExited
		hub.mu.Unlock()
		if !exited {
			killTree(candidate)
		}
	}

	failed := 0
	for _, result := range results {
		if !result.Passed {
			failed++
		}
	}

	if *asJSON {
		report := map[string]any{
			"target":  target,
			"total":   len(results),
			"failed":  failed,
			"results": results,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(report)
	} else {
		fmt.Printf("conformance: plugin suite against %s\n\n", target)
		for _, result := range results {
			status := "PASS"
			if !result.Passed {
				status = "FAIL"
			}
			fmt.Printf("%s  %-26s %s\n", status, result.ID, result.Title)
			if !result.Passed {
				fmt.Printf("      %s\n", result.Error)
			}
		}
		fmt.Printf("\n%d checks, %d failed\n", len(results), failed)
	}

	if failed > 0 {
		os.Exit(1)
	}
}
