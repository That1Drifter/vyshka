package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/That1Drifter/vyshka/client"
)

// resolveServer turns a SERVER argument into a record: the id itself when the
// hub knows it, else a server whose name matches it exactly (any case), else
// the one server whose name contains it. Anything short of exactly one match
// is a usage error listing the candidates.
func (e *env) resolveServer(c *client.Client, arg string) (client.Server, error) {
	server, err := c.GetServer(e.ctx, arg)
	if err == nil {
		return server, nil
	}
	// A name is not an id, and the hub may answer a name that does not fit
	// an id's shape with something other than not_found; any refusal short
	// of an authentication failure or a hub fault falls through to the list.
	var refusal *client.Error
	if !errors.As(err, &refusal) || refusal.Status == 401 || refusal.Status == 403 || refusal.Status >= 500 {
		return client.Server{}, err
	}

	servers, err := c.ListServers(e.ctx)
	if err != nil {
		return client.Server{}, err
	}
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
