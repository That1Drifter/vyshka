package cli

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/client"
)

func cmdState(e *env, args []string) error {
	fs, apply := e.flagSet("state")
	history := fs.Int("history", 0, "")
	positionals, err := e.parse(fs, apply, args, "state")
	if err != nil {
		return err
	}
	if len(positionals) != 2 {
		return usagef("state takes SERVER and one of players, vehicles, entities, world")
	}
	stateType := positionals[1]
	switch stateType {
	case "players", "vehicles", "entities", "world":
	default:
		return usagef("state type %q is not players, vehicles, entities, or world", stateType)
	}
	if *history < 0 {
		return usagef("--history must not be negative")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	server, err := e.resolveServer(c, positionals[0])
	if err != nil {
		return err
	}

	if *history > 0 {
		snapshots, err := c.StateHistory(e.ctx, server.ID, stateType, *history)
		if err != nil {
			return err
		}
		if e.g.json {
			list := make([]any, len(snapshots))
			for i, snapshot := range snapshots {
				list[i] = rawOr(snapshot.Raw, snapshot)
			}
			return e.emitJSON(map[string]any{"snapshots": list})
		}
		tw := e.table()
		fmt.Fprintln(tw, "CAPTURED\tAGE\tRECEIVED\tENTRIES")
		for _, snapshot := range snapshots {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", formatTime(snapshot.CapturedAt), age(snapshot.CapturedAt),
				formatTime(snapshot.ReceivedAt), entryCount(snapshot, stateType))
		}
		return tw.Flush()
	}

	snapshot, err := c.GetState(e.ctx, server.ID, stateType)
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(snapshot.Raw, snapshot))
	}
	fmt.Fprintf(e.stdout, "captured %s (%s ago), received %s\n",
		formatTime(snapshot.CapturedAt), age(snapshot.CapturedAt), formatTime(snapshot.ReceivedAt))

	switch stateType {
	case "players":
		players, err := snapshot.Players()
		if err != nil {
			return usagef("the players snapshot does not decode: %v", err)
		}
		if len(players) == 0 {
			fmt.Fprintln(e.stdout, "no players")
			return nil
		}
		tw := e.table()
		fmt.Fprintln(tw, "NAME\tPLATFORM\tID\tPOSITION")
		for _, player := range players {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", cell(player.Name), cell(player.Player.Platform),
				cell(player.Player.ID), formatPosition(player.Position))
		}
		return tw.Flush()
	case "vehicles", "entities":
		things, err := snapshot.Vehicles()
		if stateType == "entities" {
			things, err = snapshot.Entities()
		}
		if err != nil {
			return usagef("the %s snapshot does not decode: %v", stateType, err)
		}
		if len(things) == 0 {
			fmt.Fprintln(e.stdout, "no "+stateType)
			return nil
		}
		tw := e.table()
		fmt.Fprintln(tw, "ID\tKIND\tPOSITION")
		for _, thing := range things {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", cell(thing.ID), cell(thing.Kind), formatPosition(thing.Position))
		}
		return tw.Flush()
	default:
		world := rawMember(snapshot.Snapshot, "world")
		fmt.Fprintf(e.stdout, "time: %s\n", cell(unquoted(rawMember(world, "time"))))
		data := rawMember(world, "data")
		if len(data) == 0 || string(data) == "null" {
			fmt.Fprintln(e.stdout, "data: none")
			return nil
		}
		fmt.Fprintln(e.stdout, "data:")
		fmt.Fprintln(e.stdout, prettyJSON(data))
		return nil
	}
}

// unquoted reads a raw JSON string member; anything else comes back empty.
func unquoted(raw []byte) string {
	value, err := strconv.Unquote(string(raw))
	if err != nil {
		return ""
	}
	return value
}

func entryCount(snapshot client.StateSnapshot, stateType string) string {
	var count int
	var err error
	switch stateType {
	case "players":
		var players []client.PlayerEntry
		players, err = snapshot.Players()
		count = len(players)
	case "vehicles":
		var things []client.ThingEntry
		things, err = snapshot.Vehicles()
		count = len(things)
	case "entities":
		var things []client.ThingEntry
		things, err = snapshot.Entities()
		count = len(things)
	default:
		return "-"
	}
	if err != nil {
		return "?"
	}
	return strconv.Itoa(count)
}

// parseWhen reads --since and --until: RFC 3339, or a duration meaning that
// long before now.
func parseWhen(flagName, value string) (*time.Time, error) {
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return &at, nil
	}
	if ago, err := time.ParseDuration(value); err == nil && ago >= 0 {
		at := time.Now().Add(-ago)
		return &at, nil
	}
	return nil, usagef("--%s %q is neither an RFC 3339 time nor a duration such as 15m", flagName, value)
}

// eventDataLimit is how much of an event's payload a table row shows.
const eventDataLimit = 100

func eventData(event client.Event) string {
	data := rawMember(event.Raw, "data")
	if len(data) == 0 {
		return "{}"
	}
	return truncate(clean(compactJSON(data)), eventDataLimit)
}

func cmdEvents(e *env, args []string) error {
	fs, apply := e.flagSet("events")
	var types stringList
	fs.Var(&types, "type", "")
	since := fs.String("since", "", "")
	until := fs.String("until", "", "")
	limit := fs.Int("limit", 0, "")
	all := fs.Bool("all", false, "")
	follow := fs.Bool("follow", false, "")
	interval := fs.Duration("interval", 2*time.Second, "")
	positionals, err := e.parse(fs, apply, args, "events")
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usagef("events takes one SERVER")
	}
	if *limit < 0 {
		return usagef("--limit must not be negative")
	}
	if *follow && (*until != "" || *all) {
		return usagef("--follow does not combine with --until or --all")
	}
	if *interval <= 0 {
		return usagef("--interval must be positive")
	}

	query := client.EventQuery{Types: types, Limit: *limit}
	if *since != "" {
		if query.Since, err = parseWhen("since", *since); err != nil {
			return err
		}
	}
	if *until != "" {
		if query.Until, err = parseWhen("until", *until); err != nil {
			return err
		}
	}

	c, err := e.client()
	if err != nil {
		return err
	}
	server, err := e.resolveServer(c, positionals[0])
	if err != nil {
		return err
	}
	if *follow {
		return e.followEvents(c, server.ID, query, *interval)
	}

	tw := e.table()
	if !e.g.json {
		fmt.Fprintln(tw, "OCCURRED\tTYPE\tDATA")
	}
	more := false
	for {
		page, err := c.ListEvents(e.ctx, server.ID, query)
		if err != nil {
			return err
		}
		if e.g.json {
			if err := e.emitJSON(rawOr(page.Raw, page)); err != nil {
				return err
			}
		} else {
			for _, event := range page.Events {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", formatTime(event.OccurredAt), cell(event.Type), eventData(event))
			}
		}
		if page.NextCursor == "" {
			break
		}
		if !*all {
			more = true
			break
		}
		query.Cursor = page.NextCursor
	}
	if e.g.json {
		return nil
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if more {
		fmt.Fprintln(e.stderr, "more events are older than this page; pass --all to walk every page")
	}
	return nil
}

// followLookback is how far behind the newest printed event --follow asks
// again. Events are ordered by occurredAt, the game's clock, and one can land
// after a later-stamped one has been printed; asking again over this window,
// and skipping what was already printed, is what keeps such a late arrival
// from being missed.
const followLookback = 60 * time.Second

// followEvents prints the latest page oldest first, then polls for what
// arrives after it. It ends quietly when the run's context is cancelled
// (SIGINT) or stdout stops accepting output.
func (e *env) followEvents(c *client.Client, serverID string, query client.EventQuery, interval time.Duration) error {
	printed := map[string]time.Time{}
	var newest time.Time

	show := func(events []client.Event) bool {
		slices.SortStableFunc(events, func(a, b client.Event) int {
			if order := a.OccurredAt.Compare(b.OccurredAt); order != 0 {
				return order
			}
			return strings.Compare(a.ID, b.ID)
		})
		for _, event := range events {
			if _, seen := printed[event.ID]; seen {
				continue
			}
			printed[event.ID] = event.OccurredAt
			if event.OccurredAt.After(newest) {
				newest = event.OccurredAt
			}
			var err error
			if e.g.json {
				err = e.emitJSON(rawOr(event.Raw, event))
			} else {
				_, err = fmt.Fprintf(e.stdout, "%s  %s  %s\n", formatTime(event.OccurredAt), cell(event.Type), eventData(event))
			}
			if err != nil {
				return false
			}
		}
		return true
	}

	first, err := c.ListEvents(e.ctx, serverID, query)
	if err != nil {
		if e.ctx.Err() != nil {
			return nil
		}
		return err
	}
	if !show(first.Events) {
		return nil
	}

	for {
		select {
		case <-e.ctx.Done():
			return nil
		case <-time.After(interval):
		}

		next := client.EventQuery{Types: query.Types, Limit: query.Limit, Since: query.Since}
		if !newest.IsZero() {
			since := newest.Add(-followLookback)
			next.Since = &since
		}
		var fresh []client.Event
		for {
			page, err := c.ListEvents(e.ctx, serverID, next)
			if err != nil {
				if e.ctx.Err() != nil {
					return nil
				}
				// A hub restarting mid-tail is worth waiting out; a refusal
				// (a revoked token, say) is not.
				var transport *client.TransportError
				if errors.As(err, &transport) {
					fmt.Fprintln(e.stderr, "vyshka: "+transport.Error()+"; retrying")
					fresh = nil
					break
				}
				return err
			}
			fresh = append(fresh, page.Events...)
			if page.NextCursor == "" {
				break
			}
			next.Cursor = page.NextCursor
		}
		if !show(fresh) {
			return nil
		}
		// Anything older than the window can no longer come back, so its id
		// need not be remembered.
		horizon := newest.Add(-followLookback)
		for id, occurred := range printed {
			if occurred.Before(horizon) {
				delete(printed, id)
			}
		}
	}
}

func cmdContexts(e *env, args []string) error {
	fs, apply := e.flagSet("contexts")
	refresh := fs.Bool("refresh", false, "")
	positionals, err := e.parse(fs, apply, args, "contexts")
	if err != nil {
		return err
	}
	if len(positionals) != 2 {
		return usagef("contexts takes SERVER and CONTEXT_ID")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	server, err := e.resolveServer(c, positionals[0])
	if err != nil {
		return err
	}
	entries, err := c.EnumerateContext(e.ctx, server.ID, positionals[1], *refresh)
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(entries.Raw, entries))
	}
	fmt.Fprintf(e.stdout, "enumerated %s (%s ago)\n", formatTime(entries.EnumeratedAt), age(entries.EnumeratedAt))
	if entries.Reason != "" {
		fmt.Fprintf(e.stdout, "reason: %s\n", clean(entries.Reason))
	}
	if len(entries.Entries) == 0 {
		fmt.Fprintln(e.stdout, "no entries")
		return nil
	}
	tw := e.table()
	fmt.Fprintln(tw, "KEY\tLABEL\tPOSITION")
	for _, entry := range entries.Entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", cell(entry.ReferenceKey), cell(entry.Label), formatPosition(entry.Position))
	}
	return tw.Flush()
}

// overviewLimit is how many of each section player shows without one named.
const overviewLimit = 10

func cmdPlayer(e *env, args []string) error {
	fs, apply := e.flagSet("player")
	limit := fs.Int("limit", 0, "")
	all := fs.Bool("all", false, "")
	var types stringList
	fs.Var(&types, "type", "")
	positionals, err := e.parse(fs, apply, args, "player")
	if err != nil {
		return err
	}
	if len(positionals) < 2 || len(positionals) > 3 {
		return usagef("player takes PLATFORM and ID, then optionally events, actions, or notes")
	}
	if *limit < 0 {
		return usagef("--limit must not be negative")
	}
	platform, playerID := positionals[0], positionals[1]
	sections := []string{"events", "actions", "notes"}
	overview := len(positionals) == 2
	if !overview {
		switch positionals[2] {
		case "events", "actions", "notes":
			sections = []string{positionals[2]}
		default:
			return usagef("player section %q is not events, actions, or notes", positionals[2])
		}
	}
	pageSize := *limit
	if overview && !flagGiven(fs, "limit") {
		pageSize = overviewLimit
	}

	c, err := e.client()
	if err != nil {
		return err
	}
	var firstRefusal error
	for i, section := range sections {
		if overview && !e.g.json {
			if i > 0 {
				fmt.Fprintln(e.stdout)
			}
			fmt.Fprintln(e.stdout, strings.ToUpper(section[:1])+section[1:])
		}
		err := e.playerSection(c, section, platform, playerID, pageSize, *all, types)
		if err == nil {
			continue
		}
		// In the overview, one section the token may not read (notes need
		// their own grant) should not hide the other two.
		var refusal *client.Error
		if overview && errors.As(err, &refusal) {
			e.printRefusal(refusal)
			if firstRefusal == nil {
				firstRefusal = &exitError{code: ExitRefused}
			}
			continue
		}
		return err
	}
	return firstRefusal
}

func (e *env) playerSection(c *client.Client, section, platform, playerID string, limit int, all bool, types []string) error {
	tw := e.table()
	switch section {
	case "events":
		query := client.EventQuery{Types: types, Limit: limit}
		if !e.g.json {
			fmt.Fprintln(tw, "OCCURRED\tTYPE\tROLES\tSERVER")
		}
		for {
			page, err := c.PlayerEvents(e.ctx, platform, playerID, query)
			if err != nil {
				return err
			}
			if e.g.json {
				if err := e.emitJSON(rawOr(page.Raw, page)); err != nil {
					return err
				}
			}
			for _, event := range page.Events {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", formatTime(event.OccurredAt), cell(event.Type),
					cell(strings.Join(event.Roles, ",")), cell(event.ServerID))
			}
			if page.NextCursor == "" || !all {
				break
			}
			query.Cursor = page.NextCursor
		}
	case "actions":
		query := client.PageQuery{Limit: limit}
		if !e.g.json {
			fmt.Fprintln(tw, "CREATED\tCODE\tSTATE\tSERVER\tID")
		}
		for {
			page, err := c.PlayerActions(e.ctx, platform, playerID, query)
			if err != nil {
				return err
			}
			if e.g.json {
				if err := e.emitJSON(rawOr(page.Raw, page)); err != nil {
					return err
				}
			}
			for _, action := range page.Actions {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", formatTime(action.CreatedAt), cell(action.Code),
					cell(action.State), cell(action.ServerID), cell(action.ID))
			}
			if page.NextCursor == "" || !all {
				break
			}
			query.Cursor = page.NextCursor
		}
	case "notes":
		query := client.PageQuery{Limit: limit}
		if !e.g.json {
			fmt.Fprintln(tw, "CREATED\tBY\tTEXT")
		}
		for {
			page, err := c.PlayerNotes(e.ctx, platform, playerID, query)
			if err != nil {
				return err
			}
			if e.g.json {
				if err := e.emitJSON(rawOr(page.Raw, page)); err != nil {
					return err
				}
			}
			for _, note := range page.Notes {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", formatTime(note.CreatedAt), cell(note.CreatedBy.TokenName),
					truncate(cell(note.Text), eventDataLimit))
			}
			if page.NextCursor == "" || !all {
				break
			}
			query.Cursor = page.NextCursor
		}
	}
	if e.g.json {
		return nil
	}
	return tw.Flush()
}
