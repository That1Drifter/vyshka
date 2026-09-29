package cli

import (
	"bufio"
	"context"
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
	record, err := c.GetManifest(e.ctx, server.ID)
	if client.IsNotFound(err) {
		return usagef("%s has no manifest, so it declares no action to run", serverLabel(server))
	}
	if err != nil {
		return err
	}
	action, err := findAction(server, record, code)
	if err != nil {
		return err
	}
	// Checked here, before any confirmation or dispatch: a params_invalid
	// round trip would say the same thing later, less clearly.
	coerced, err := params.Coerce(action.Params, paramArgs)
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
		answer, err := bufio.NewReader(e.stdio.Stdin).ReadString('\n')
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
// the exit code. Without a timeout it waits until the action's own deadline
// plus waitSlack, measured from the action's lifetime rather than from
// expiresAt against the local clock, so a skewed clock cannot end the wait
// before the hub has expired the action. justDispatched says the action was
// created a moment ago, when its whole lifetime is still ahead.
func (e *env) waitAction(c *client.Client, actionID string, timeout time.Duration, justDispatched bool) error {
	first, err := c.GetAction(e.ctx, actionID)
	if err != nil {
		return err
	}
	if timeout <= 0 {
		lifetime := max(first.ExpiresAt.Sub(first.CreatedAt), 0)
		remaining := lifetime
		if !justDispatched {
			remaining = min(max(time.Until(first.ExpiresAt), 0), lifetime)
		}
		timeout = remaining + waitSlack
	}

	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	progress := &stateProgress{w: e.stderr}
	final, err := c.WaitAction(ctx, actionID, client.WaitOptions{
		Interval: waitInterval,
		OnChange: progress.add,
	})
	progress.end()

	if err != nil {
		if ctx.Err() == nil {
			return err
		}
		// Out of time, or interrupted: the action is still in flight.
		last := final
		if last.ID == "" {
			last = first
		}
		if e.g.json {
			if jsonErr := e.emitJSON(rawOr(last.Raw, last)); jsonErr != nil {
				return jsonErr
			}
		}
		return &exitError{code: ExitWaitTimeout, message: fmt.Sprintf(
			"stopped waiting after %s with action %s still %s; read it later with \"vyshka job %s\"",
			timeout, clean(actionID), cell(last.State), clean(actionID))}
	}

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
	// WaitAction returns only on a state Terminal() knows, so this is a
	// terminal state from a newer hub; report it as it is.
	return &exitError{code: ExitUsage, message: fmt.Sprintf("action %s ended in state %s", clean(actionID), cell(final.State))}
}

// stateProgress prints an action's states on one stderr line as they
// change: queued -> delivered -> running -> completed.
type stateProgress struct {
	w       io.Writer
	started bool
}

func (p *stateProgress) add(action client.Action) {
	if p.started {
		fmt.Fprint(p.w, " -> ")
	}
	fmt.Fprint(p.w, cell(action.State))
	p.started = true
}

func (p *stateProgress) end() {
	if p.started {
		fmt.Fprintln(p.w)
	}
}
