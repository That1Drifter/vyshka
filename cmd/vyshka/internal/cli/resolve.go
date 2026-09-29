package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/That1Drifter/vyshka/client"
)

// resolveServer turns a SERVER argument into a record: the server with that
// id when the hub knows one, else a server whose name matches it exactly
// (any case), else the one server whose name contains it. Ids are opaque
// (spec section 2.1), so every argument is tried as an id first, whatever
// its shape. Only an answer saying the argument is not a server this token
// can address falls through to the names: any other refusal is the hub's
// answer about this id and is reported as such, rather than hidden behind
// a name search that could land on a different server.
func (e *env) resolveServer(c *client.Client, arg string) (client.Server, error) {
	server, err := c.GetServer(e.ctx, arg)
	if err == nil {
		return server, nil
	}
	if !notAServerForThisToken(err) {
		return client.Server{}, err
	}
	return e.resolveServerByName(c, arg)
}

// notAServerForThisToken reports whether a refusal says the argument used as
// an id names no server this token can address: not_found, or forbidden,
// which a token bound to particular servers answers for any id outside its
// binding before the id is even looked up (spec section 10.2), a name
// included. Neither is evidence about a server of that name.
func notAServerForThisToken(err error) bool {
	var refusal *client.Error
	return errors.As(err, &refusal) && (refusal.Code == "not_found" || refusal.Status == 403)
}

// resolveServerByName finds the server whose name matches arg exactly (any
// case), else the one whose name contains it. Anything short of exactly one
// match is a usage error listing the candidates.
func (e *env) resolveServerByName(c *client.Client, arg string) (client.Server, error) {
	list, err := c.ListServers(e.ctx)
	if err != nil {
		return client.Server{}, err
	}
	servers := list.Servers
	want := strings.ToLower(arg)
	var exact, partial []client.Server
	for _, candidate := range servers {
		name := strings.ToLower(candidate.Name)
		switch {
		case name == want:
			exact = append(exact, candidate)
		case strings.Contains(name, want):
			partial = append(partial, candidate)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = partial
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return client.Server{}, usagef("no server has the id or a name matching %q%s", arg, serverList(servers))
	}
	return client.Server{}, usagef("%q matches more than one server; name one by id%s", arg, serverList(matches))
}

// byIDThenName runs do with arg as the server id and, when the hub answers
// not_found for it and no server has that id, once more with the id of the
// server named arg. A command that needs only the id thus reads nothing but
// its own endpoint on the common path, so a token granted that endpoint
// alone (an events:read token reading a feed) is enough, and an id of any
// shape is taken as given, ids being opaque (spec section 2.1). ambiguous
// says the command's not_found can be about something other than the
// server (a snapshot never accepted, a context not declared): the record is
// then looked up to tell the two apart before the names are tried.
func (e *env) byIDThenName(c *client.Client, arg string, ambiguous bool, do func(serverID string) error) error {
	err := do(arg)
	if !notAServerForThisToken(err) {
		return err
	}
	if ambiguous && client.IsNotFound(err) {
		// A forbidden is about the binding, never about a snapshot or a
		// context, so only a not_found needs telling apart.
		_, lookup := c.GetServer(e.ctx, arg)
		var refusal *client.Error
		switch {
		case lookup == nil:
			// The server exists: the not_found was about something else.
			return err
		case !errors.As(lookup, &refusal):
			// The hub could not be reached: say that, not "no such server".
			return lookup
		case !client.IsNotFound(lookup):
			// The record cannot be read, so the id's own answer stands.
			return err
		}
	}
	server, byName := e.resolveServerByName(c, arg)
	if byName != nil {
		var refusal *client.Error
		if errors.As(byName, &refusal) {
			// The list is refused, so the id's own answer stands.
			return err
		}
		// A usage error (no server of that name either, with the
		// candidates) or a transport failure: both are the truer answer.
		return byName
	}
	return do(server.ID)
}

func serverList(servers []client.Server) string {
	if len(servers) == 0 {
		return "; the hub has no servers"
	}
	var list strings.Builder
	list.WriteString(":")
	for _, server := range servers {
		fmt.Fprintf(&list, "\n  %s  %s", clean(server.ID), clean(server.Name))
	}
	return list.String()
}

// serverLabel names a server for messages.
func serverLabel(server client.Server) string {
	return fmt.Sprintf("%s (%s)", clean(server.Name), clean(server.ID))
}

// resolvePlayer finds a player in the server's latest players snapshot: an
// exact player id, else an exact name (any case), else the one name
// containing the fragment. The snapshot is whatever the plugin last sent, so
// the match is reported with its capture time beside it.
func (e *env) resolvePlayer(c *client.Client, server client.Server, fragment string) (client.PlayerEntry, error) {
	snapshot, err := c.GetState(e.ctx, server.ID, "players")
	if client.IsNotFound(err) {
		return client.PlayerEntry{}, usagef("%s has no players snapshot to resolve %q against; pass --target with the player id",
			serverLabel(server), fragment)
	}
	if err != nil {
		return client.PlayerEntry{}, err
	}
	players, err := snapshot.Players()
	if err != nil {
		return client.PlayerEntry{}, usagef("the players snapshot of %s does not decode: %v", serverLabel(server), err)
	}

	want := strings.ToLower(fragment)
	var byID, byName, partial []client.PlayerEntry
	for _, player := range players {
		name := strings.ToLower(player.Name)
		switch {
		case player.Player.ID == fragment:
			byID = append(byID, player)
		case name == want:
			byName = append(byName, player)
		case want != "" && strings.Contains(name, want):
			partial = append(partial, player)
		}
	}
	matches := byID
	if len(matches) == 0 {
		matches = byName
	}
	if len(matches) == 0 {
		matches = partial
	}
	captured := fmt.Sprintf("the players snapshot captured %s (%s ago)", formatTime(snapshot.CapturedAt), age(snapshot.CapturedAt))

	switch len(matches) {
	case 1:
		player := matches[0]
		fmt.Fprintf(e.stderr, "resolved %q to %s from %s\n", fragment, playerLabel(player), captured)
		return player, nil
	case 0:
		return client.PlayerEntry{}, usagef("no player in %s matches %q (%d online then)", captured, fragment, len(players))
	}
	var list strings.Builder
	for _, player := range matches {
		list.WriteString("\n  " + playerLabel(player))
	}
	return client.PlayerEntry{}, usagef("%q matches more than one player in %s; use a longer fragment or the id:%s",
		fragment, captured, list.String())
}

func playerLabel(player client.PlayerEntry) string {
	name := player.Name
	if name == "" {
		name = "(no name)"
	}
	return fmt.Sprintf("%s (%s:%s)", clean(name), clean(player.Player.Platform), clean(player.Player.ID))
}
