package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/client"
	"github.com/That1Drifter/vyshka/cmd/vyshka/internal/params"
)

// waitInterval is how often --wait reads the action. A variable so tests can
// shorten it; nothing else changes it.
var waitInterval = 500 * time.Millisecond

// waitSlack is added to an action's deadline when --wait has no --timeout:
// the hub flips an overdue action to expired within about a second, so a
// wait that outlasts the deadline by this much always sees a terminal state.
const waitSlack = 5 * time.Second

func cmdRun(e *env, args []string) error {
	fs, apply := e.flagSet("run")
	targetKey := fs.String("target", "", "")
	playerFragment := fs.String("player", "", "")
	ttl := fs.Duration("ttl", 0, "")
	idempotencyKey := fs.String("idempotency-key", "", "")
	wait := fs.Bool("wait", false, "")
	timeout := fs.Duration("timeout", 0, "")
	yes := fs.Bool("yes", false, "")
	positionals, err := e.parse(fs, apply, args, "run")
	if err != nil {
		return err
	}
	if len(positionals) < 2 {
		return usagef("run takes SERVER and CODE, then params; run \"vyshka help run\" for usage")
	}
	serverArg, code, paramArgs := positionals[0], positionals[1], positionals[2:]
	if flagGiven(fs, "target") && flagGiven(fs, "player") {
		return usagef("--target and --player both name the referenceKey; pass one")
	}
	if flagGiven(fs, "player") && *playerFragment == "" {
		return usagef("--player needs a name or id to look for")
	}
	ttlSeconds := 0
	if flagGiven(fs, "ttl") {
		if ttlSeconds, err = seconds("ttl", *ttl); err != nil {
			return err
		}
	}
	if flagGiven(fs, "timeout") && *timeout <= 0 {
		return usagef("--timeout must be positive")
	}

	c, err := e.client()
	if err != nil {
		return err
	}
	server, err := e.resolveServer(c, serverArg)
	if err != nil {
		return err
	}
	// A retry with an idempotency key is answered with the original action
	// whatever the manifest says now (spec section 7), so with a key the
	// manifest is advice rather than a gate: an action it no longer declares,
	// or params its current schema refuses, go out as written and the hub
	// decides. Refusing here would strand an accepted action whose first
	// answer was lost, which is what the key exists to recover.
	retry := *idempotencyKey != ""
	asWritten := fmt.Sprintf("sending the retry with key %s as written", strconv.Quote(clean(*idempotencyKey)))
	action := client.ManifestAction{Code: code}
	record, err := c.GetManifest(e.ctx, server.ID)
	switch {
	case client.IsNotFound(err) && !retry:
		return usagef("%s has no manifest, so it declares no action to run", serverLabel(server))
	case client.IsNotFound(err):
		fmt.Fprintf(e.stderr, "notice: %s has no manifest; %s\n", serverLabel(server), asWritten)
	case err != nil:
		return err
	default:
		declared, err := findAction(server, record, code)
		switch {
		case err == nil:
			action = declared
		case !retry:
			return err
		default:
			fmt.Fprintf(e.stderr, "notice: %v; %s\n", err, asWritten)
		}
	}
	// Checked here, before any confirmation or dispatch: a params_invalid
	// round trip would say the same thing later, less clearly.
	coerced, err := params.Coerce(action.Params, paramArgs)
	if err != nil && retry {
		// Lenient cannot fail on a value, only on a malformed argument,
		// which the first dispatch would have refused the same way.
		fmt.Fprintf(e.stderr, "notice: %v; %s\n", err, asWritten)
		coerced, err = params.Lenient(paramArgs)
	}
	if err != nil {
		return usagef("%v", err)
	}

	referenceKey := *targetKey
	if *playerFragment != "" {
		player, err := e.resolvePlayer(c, server, *playerFragment)
		if err != nil {
			return err
		}
		referenceKey = player.Player.ID
	}

	if err := e.confirm(action, server, *yes); err != nil {
		return err
	}

	dispatched, err := c.DispatchAction(e.ctx, server.ID, client.DispatchRequest{
		Code:           action.Code,
		Context:        action.Context,
		ReferenceKey:   referenceKey,
		Params:         coerced,
		TTLSeconds:     ttlSeconds,
		IdempotencyKey: *idempotencyKey,
	})
	if err != nil {
		return err
	}
	if !*wait {
		if e.g.json {
			return e.emitJSON(rawOr(dispatched.Raw, dispatched))
		}
		return e.keyValues(
			[2]string{"actionId", cell(dispatched.ActionID)},
			[2]string{"state", cell(dispatched.State)},
		)
	}
	return e.waitAction(c, dispatched.ActionID, *timeout, true)
}

// confirm applies the action's danger level: destructive needs --yes or a
// yes typed at a terminal, warning prints a notice, and anything else,
// including a level this client does not know, proceeds.
func (e *env) confirm(action client.ManifestAction, server client.Server, yes bool) error {
	switch action.Danger {
	case "destructive":
		if yes {
			return nil
		}
		if !e.stdio.StdinIsTerminal {
			return usagef("%s is marked destructive by its plugin and there is no terminal to confirm on; pass --yes to dispatch it",
				clean(action.Code))
		}
		fmt.Fprintf(e.stderr, "run %s on %s (destructive)? [y/N] ", clean(action.Code), clean(server.Name))
		answer, err := readLine(e.ctx, e.stdio.Stdin)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(e.stderr)
			return usagef("interrupted at the confirmation; nothing was dispatched")
		}
		if err != nil && err != io.EOF {
			return usagef("reading the confirmation: %v", err)
		}
		if err == io.EOF && answer == "" {
			// Stdin looked like a terminal but ended before a line arrived:
			// nobody is there to answer, so say what a script should do.
			fmt.Fprintln(e.stderr)
			return usagef("no confirmation was read for %s; pass --yes to dispatch it without a prompt", clean(action.Code))
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return nil
		}
		return usagef("not confirmed; nothing was dispatched")
	case "warning":
		fmt.Fprintf(e.stderr, "warning: %s is marked warning by its plugin\n", clean(action.Code))
	}
	return nil
}

// readLine reads one line, giving up when ctx ends: a prompt on a terminal
// must yield to Ctrl-C, and a blocking read cannot see the signal by itself.
// On interruption the reading goroutine is left behind, which is fine for a
// process on its way out.
func readLine(ctx context.Context, r io.Reader) (string, error) {
	type lineResult struct {
		line string
		err  error
	}
	done := make(chan lineResult, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		done <- lineResult{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case got := <-done:
		return got.line, got.err
	}
}

func cmdJob(e *env, args []string) error {
	fs, apply := e.flagSet("job")
	wait := fs.Bool("wait", false, "")
	timeout := fs.Duration("timeout", 0, "")
	positionals, err := e.parse(fs, apply, args, "job")
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usagef("job takes one ACTION_ID")
	}
	if flagGiven(fs, "timeout") && *timeout <= 0 {
		return usagef("--timeout must be positive")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	if *wait {
		return e.waitAction(c, positionals[0], *timeout, false)
	}
	action, err := c.GetAction(e.ctx, positionals[0])
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(action.Raw, action))
	}
	return e.printJob(action)
}

func (e *env) printJob(action client.Action) error {
	ok := "-"
	if action.OK != nil {
		ok = strconv.FormatBool(*action.OK)
	}
	failure := "-"
	if action.Error != nil {
		failure = cell(*action.Error)
	}
	duration := "-"
	if action.DurationMs != nil {
		duration = strconv.FormatInt(*action.DurationMs, 10) + "ms"
	}
	params := rawMember(action.Raw, "params")
	shownParams := "{}"
	if len(params) > 0 && string(params) != "null" {
		shownParams = compactJSON(params)
	}
	if err := e.keyValues(
		[2]string{"id", cell(action.ID)},
		[2]string{"server", cell(action.ServerID)},
		[2]string{"code", cell(action.Code)},
		[2]string{"context", cell(action.Context)},
		[2]string{"referenceKey", cell(action.ReferenceKey)},
		[2]string{"idempotencyKey", cell(action.IdempotencyKey)},
		[2]string{"params", shownParams},
		[2]string{"state", cell(action.State)},
		[2]string{"created", formatTime(action.CreatedAt)},
		[2]string{"expires", formatTime(action.ExpiresAt)},
		[2]string{"delivered", formatTimePtr(action.DeliveredAt, "-")},
		[2]string{"running", formatTimePtr(action.RunningAt, "-")},
		[2]string{"finished", formatTimePtr(action.FinishedAt, "-")},
		[2]string{"ok", ok},
		[2]string{"error", failure},
		[2]string{"duration", duration},
	); err != nil {
		return err
	}
	if action.HasResult() {
		fmt.Fprintln(e.stdout, "result:")
		fmt.Fprintln(e.stdout, prettyJSON(action.Result))
	} else {
		fmt.Fprintln(e.stdout, "result: none")
	}
	return nil
}

// waitAction follows one action to its outcome and turns the outcome into
// the exit code. With no --timeout it waits until the action's own deadline
// plus waitSlack, measured from the action's lifetime rather than from
// expiresAt against the local clock, so a skewed clock cannot end the wait
// before the hub has expired the action. justDispatched says the action was
// created a moment ago, when its whole lifetime is still ahead. The first
// read counts against --timeout like every later one, and an action already
// terminal on that read is reported from it: nothing is read a second time
// to learn what the first read said.
func (e *env) waitAction(c *client.Client, actionID string, timeout time.Duration, justDispatched bool) error {
	ctx, cancel := e.ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(e.ctx, timeout)
	}
	defer cancel()

	progress := &stateProgress{w: e.stderr}
	first, err := c.GetAction(ctx, actionID)
	if err != nil {
		if ctx.Err() != nil {
			return e.waitCutShort(timeout, actionID, client.Action{})
		}
		return err
	}
	progress.add(first)
	final := first
	if !first.Terminal() {
		if timeout <= 0 {
			lifetime := max(first.ExpiresAt.Sub(first.CreatedAt), 0)
			remaining := lifetime
			if !justDispatched {
				remaining = min(max(time.Until(first.ExpiresAt), 0), lifetime)
			}
			timeout = remaining + waitSlack
			ctx, cancel = context.WithTimeout(e.ctx, timeout)
			defer cancel()
		}
		final, err = c.WaitAction(ctx, actionID, client.WaitOptions{
			Interval: waitInterval,
			OnChange: progress.add,
		})
		if err != nil {
			progress.end()
			if ctx.Err() == nil {
				return err
			}
			// Out of time, or interrupted: the action is still in flight,
			// and the last record read says where it got to.
			if final.ID == "" {
				final = first
			}
			return e.waitCutShort(timeout, actionID, final)
		}
	}
	progress.end()

	if e.g.json {
		if err := e.emitJSON(rawOr(final.Raw, final)); err != nil {
			return err
		}
	}
	switch final.State {
	case "completed":
		if !e.g.json {
			if final.HasResult() {
				fmt.Fprintln(e.stdout, prettyJSON(final.Result))
			} else {
				fmt.Fprintln(e.stdout, "(no result)")
			}
		}
		return nil
	case "failed":
		if !e.g.json && final.HasResult() {
			fmt.Fprintln(e.stdout, prettyJSON(final.Result))
		}
		message := "action failed"
		if final.Error != nil && *final.Error != "" {
			message += ": " + clean(*final.Error)
		}
		return &exitError{code: ExitActionFailed, message: message}
	case "expired":
		return &exitError{code: ExitActionExpired, message: fmt.Sprintf(
			"action %s expired at %s before the plugin finished it", clean(actionID), formatTime(final.ExpiresAt))}
	}
	// Terminal() knows only the three terminal states of this draft, so a
	// state it does not know is waited on until the deadline; this branch
	// is for the day it learns another, so that day cannot exit 0 by
	// accident.
	return &exitError{code: ExitUsage, message: fmt.Sprintf("action %s ended in state %s", clean(actionID), cell(final.State))}
}

// waitCutShort reports a wait that ended before the action did: out of time,
// or interrupted. The action is still in flight, so the id is named for a
// later read. last is the record last read, or empty when none was.
func (e *env) waitCutShort(timeout time.Duration, actionID string, last client.Action) error {
	if e.g.json && last.ID != "" {
		if err := e.emitJSON(rawOr(last.Raw, last)); err != nil {
			return err
		}
	}
	why := fmt.Sprintf("stopped waiting after %s", timeout)
	if e.ctx.Err() != nil {
		why = "interrupted"
	}
	state := "not yet read"
	if last.ID != "" {
		state = "still " + cell(last.State)
	}
	return &exitError{code: ExitWaitTimeout, message: fmt.Sprintf(
		"%s with action %s %s; read it later with \"vyshka job %s\"", why, clean(actionID), state, clean(actionID))}
}

// stateProgress prints an action's states on one stderr line as they
// change: queued -> delivered -> running -> completed. A state equal to the
// last one printed is not repeated, so a wait that starts from a record it
// has already shown adds nothing for it.
type stateProgress struct {
	w       io.Writer
	started bool
	last    string
}

func (p *stateProgress) add(action client.Action) {
	if p.started && action.State == p.last {
		return
	}
	if p.started {
		fmt.Fprint(p.w, " -> ")
	}
	fmt.Fprint(p.w, cell(action.State))
	p.started, p.last = true, action.State
}

func (p *stateProgress) end() {
	if p.started {
		fmt.Fprintln(p.w)
	}
}
