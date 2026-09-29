package cli

import (
	"fmt"
	"strings"

	"github.com/That1Drifter/vyshka/client"
)

// idAlphabet is Crockford base32, the encoding of every id the hub mints.
const idAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// looksLikeID reports whether arg has the shape of a hub-assigned id: 26
// characters of Crockford base32 (a ULID, spec section 4). A name can be
// given that shape on purpose, so the test only decides which lookup to try
// first.
func looksLikeID(arg string) bool {
	if len(arg) != 26 {
		return false
	}
	for _, r := range strings.ToUpper(arg) {
		if !strings.ContainsRune(idAlphabet, r) {
			return false
		}
	}
	return true
}

// resolveServer turns a SERVER argument into a record: the id itself when it
// has an id's shape and the hub knows it, else a server whose name matches it
// exactly (any case), else the one server whose name contains it. Anything
// short of exactly one match is a usage error listing the candidates. Only
// not_found falls through from the id lookup to the names: any other refusal
// is the hub's answer about this id and is reported as such, rather than
// hidden behind a name search that could land on a different server.
func (e *env) resolveServer(c *client.Client, arg string) (client.Server, error) {
	if looksLikeID(arg) {
		server, err := c.GetServer(e.ctx, arg)
		if err == nil {
			return server, nil
		}
		if !client.IsNotFound(err) {
			return client.Server{}, err
		}
	}

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

// resolveServerID is resolveServer for a command that needs only the id. An
// argument with an id's shape is taken as the id with no lookup at all, so a
// token that may read a server's events but not its record can still read
// them by id (the command's own request answers not_found for an unknown
// one); a name still goes through the list.
func (e *env) resolveServerID(c *client.Client, arg string) (string, error) {
	if looksLikeID(arg) {
		return arg, nil
	}
	server, err := e.resolveServer(c, arg)
	return server.ID, err
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
