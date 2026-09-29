package client

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// defaultWaitInterval is how often WaitAction reads an action when the caller
// names no interval: quick enough that a fast action feels immediate, slow
// enough that a long one costs the hub little.
const defaultWaitInterval = 500 * time.Millisecond

// DispatchAction dispatches an action to a server. The hub validates it
// against the server's manifest before queueing anything, so a params_invalid
// or unknown_action refusal means nothing reached the game server.
func (c *Client) DispatchAction(ctx context.Context, serverID string, request DispatchRequest) (Dispatched, error) {
	if request.Params == nil {
		request.Params = map[string]any{}
	}
	var dispatched Dispatched
	err := c.do(ctx, http.MethodPost, []string{"api", "v1", "servers", serverID, "actions"}, nil, request, &dispatched)
	return dispatched, err
}

// GetAction reads one action's lifecycle record.
func (c *Client) GetAction(ctx context.Context, actionID string) (Action, error) {
	var action Action
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "actions", actionID}, nil, nil, &action)
	return action, err
}

// WaitAction reads an action until it reaches a terminal state or ctx ends.
// When ctx ends first it returns the last record it read together with an
// error wrapping ctx.Err(), so a caller can tell running out of time
// (errors.Is(err, context.DeadlineExceeded)) from a refusal or a transport
// failure, and still report how far the action got.
func (c *Client) WaitAction(ctx context.Context, actionID string, opts WaitOptions) (Action, error) {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultWaitInterval
	}

	var last Action
	seen := false
	stopped := func() (Action, error) {
		return last, fmt.Errorf("client: stopped waiting for action %s: %w", actionID, ctx.Err())
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return stopped()
		case <-timer.C:
		}

		action, err := c.GetAction(ctx, actionID)
		if err != nil {
			// A read cut short by ctx is the deadline, not a hub failure.
			if ctx.Err() != nil {
				return stopped()
			}
			return last, err
		}
		if opts.OnChange != nil && (!seen || action.State != last.State) {
			opts.OnChange(action)
		}
		last, seen = action, true
		if action.Terminal() {
			return action, nil
		}
		timer.Reset(interval)
	}
}
