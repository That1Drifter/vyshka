package cli

import (
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/client"
)

func cmdVersion(e *env, args []string) error {
	fs, apply := e.flagSet("version")
	positionals, err := e.parse(fs, apply, args, "version")
	if err != nil {
		return err
	}
	if len(positionals) > 0 {
		return usagef("version takes no arguments")
	}
	if e.g.json {
		return e.emitJSON(map[string]string{"version": client.Version, "protocolDraft": client.ProtocolDraft})
	}
	fmt.Fprintln(e.stdout, client.Version)
	fmt.Fprintln(e.stdout, "protocol draft "+client.ProtocolDraft)
	return nil
}

func cmdHealth(e *env, args []string) error {
	fs, apply := e.flagSet("health")
	positionals, err := e.parse(fs, apply, args, "health")
	if err != nil {
		return err
	}
	if len(positionals) > 0 {
		return usagef("health takes no arguments")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	health, err := c.Health(e.ctx)
	// A degraded hub still sent its body, which is the most useful thing to
	// show; the refusal behind it sets the exit code.
	if health.Status != "" {
		if e.g.json {
			if jsonErr := e.emitJSON(rawOr(health.Raw, health)); jsonErr != nil {
				return jsonErr
			}
		} else {
			database := health.Database.Driver
			if health.Database.OK {
				database += fmt.Sprintf(" (ok, schema %d)", health.Database.SchemaVersion)
			} else {
				database += " (not ok: " + clean(health.Database.Error) + ")"
			}
			if printErr := e.keyValues(
				[2]string{"status", cell(health.Status)},
				[2]string{"version", cell(health.Version)},
				[2]string{"uptime", (time.Duration(health.UptimeSeconds) * time.Second).String()},
				[2]string{"database", database},
			); printErr != nil {
				return printErr
			}
		}
	}
	return err
}

func cmdServers(e *env, args []string) error {
	// Global flags may sit between the command and its subcommand; they are
	// handed on to the subcommand's own parse.
	flags, rest := leadingGlobals(args)
	if len(rest) > 0 {
		subArgs := append(flags, rest[1:]...)
		switch rest[0] {
		case "show":
			return cmdServersShow(e, subArgs)
		case "create":
			return cmdServersCreate(e, subArgs)
		case "token":
			return cmdServersToken(e, subArgs)
		}
	}
	fs, apply := e.flagSet("servers")
	positionals, err := e.parse(fs, apply, args, "servers")
	if err != nil {
		return err
	}
	if len(positionals) > 0 {
		return usagef("unknown servers subcommand %q; want show, create, or token", positionals[0])
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	list, err := c.ListServers(e.ctx)
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(list.Raw, list))
	}
	servers := list.Servers

	tw := e.table()
	fmt.Fprintln(tw, "ID\tNAME\tGAME\tLINK\tCREDENTIALS\tPENDING\tLAST SEEN\tPLUGIN")
	for _, server := range servers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			cell(server.ID), cell(server.Name), cell(server.Game), cell(server.LinkState),
			cell(server.CredentialState), server.PendingEnvelopeCount,
			formatTimePtr(server.LastSeenAt, "never"), pluginLabel(server.Plugin))
	}
	return tw.Flush()
}

func pluginLabel(plugin *client.PluginDescriptor) string {
	if plugin == nil || plugin.Name == "" && plugin.Version == "" {
		return "-"
	}
	return clean(strings.TrimSpace(plugin.Name + " " + plugin.Version))
}

func cmdServersShow(e *env, args []string) error {
	fs, apply := e.flagSet("servers show")
	positionals, err := e.parse(fs, apply, args, "servers")
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usagef("servers show takes one SERVER")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	server, err := e.resolveServer(c, positionals[0])
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(server.Raw, server))
	}

	pairs := [][2]string{
		{"id", cell(server.ID)},
		{"name", cell(server.Name)},
		{"game", cell(server.Game)},
		{"created", formatTime(server.CreatedAt)},
		{"enrolled", formatTimePtr(server.EnrolledAt, "never")},
		{"revoked", formatTimePtr(server.RevokedAt, "-")},
		{"credentials", cell(server.CredentialState)},
		{"link", cell(server.LinkState)},
		{"last seen", formatTimePtr(server.LastSeenAt, "never")},
		{"pending envelopes", strconv.Itoa(server.PendingEnvelopeCount)},
		{"plugin", pluginLabel(server.Plugin)},
	}
	if server.Plugin != nil && len(server.Plugin.Transports) > 0 {
		pairs = append(pairs, [2]string{"transports", clean(strings.Join(server.Plugin.Transports, ", "))})
	}
	if server.Session != nil {
		pairs = append(pairs, [2]string{"session", fmt.Sprintf("%s, expires %s, poll timeout %ds",
			clean(server.Session.ID), formatTime(server.Session.ExpiresAt), server.Session.PollTimeoutSeconds)})
	} else {
		pairs = append(pairs, [2]string{"session", "none"})
	}
	if server.Bans != nil {
		bans := "not supported"
		if server.Bans.Supported {
			bans = "supported"
			if server.Bans.AppliedRevision != nil {
				bans += fmt.Sprintf(", applied revision %d at %s",
					*server.Bans.AppliedRevision, formatTimePtr(server.Bans.AppliedAt, "-"))
			} else {
				bans += ", nothing applied yet"
			}
		}
		pairs = append(pairs, [2]string{"bans", bans})
	}
	return e.keyValues(pairs...)
}

// seconds converts a duration flag to whole seconds, refusing anything under
// one second, which the hub would clamp to something the user did not write.
func seconds(flagName string, d time.Duration) (int, error) {
	if d < time.Second {
		return 0, usagef("--%s must be at least 1s", flagName)
	}
	return int(d / time.Second), nil
}

func cmdServersCreate(e *env, args []string) error {
	fs, apply := e.flagSet("servers create")
	game := fs.String("game", "", "")
	ttl := fs.Duration("enrollment-ttl", 0, "")
	positionals, err := e.parse(fs, apply, args, "servers")
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usagef("servers create takes one NAME (quote a name with spaces)")
	}
	request := client.CreateServerRequest{Name: positionals[0], Game: *game}
	if flagGiven(fs, "enrollment-ttl") {
		if request.EnrollmentTokenTTLSeconds, err = seconds("enrollment-ttl", *ttl); err != nil {
			return err
		}
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	created, err := c.CreateServer(e.ctx, request)
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(created.Raw, created))
	}
	// The token is the point of this command: it is a one-time credential,
	// shown here and nowhere else, on a line of its own so it copies cleanly.
	fmt.Fprintf(e.stdout, "server %s\n", serverLabel(created.Server))
	fmt.Fprintln(e.stdout, "enrollment token, valid once and not shown again:")
	fmt.Fprintln(e.stdout, clean(created.Enrollment.Token))
	fmt.Fprintf(e.stdout, "expires %s\n", formatTime(created.Enrollment.ExpiresAt))
	return nil
}

func cmdServersToken(e *env, args []string) error {
	fs, apply := e.flagSet("servers token")
	ttl := fs.Duration("ttl", 0, "")
	positionals, err := e.parse(fs, apply, args, "servers")
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usagef("servers token takes one SERVER")
	}
	ttlSeconds := 0
	if flagGiven(fs, "ttl") {
		if ttlSeconds, err = seconds("ttl", *ttl); err != nil {
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
	token, err := c.IssueEnrollmentToken(e.ctx, server.ID, ttlSeconds)
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(token.Raw, token))
	}
	fmt.Fprintf(e.stdout, "enrollment token for %s, valid once and not shown again:\n", serverLabel(server))
	fmt.Fprintln(e.stdout, clean(token.Token))
	fmt.Fprintf(e.stdout, "expires %s\n", formatTime(token.ExpiresAt))
	return nil
}

func cmdActions(e *env, args []string) error {
	fs, apply := e.flagSet("actions")
	code := fs.String("code", "", "")
	positionals, err := e.parse(fs, apply, args, "actions")
	if err != nil {
		return err
	}
	if len(positionals) > 1 {
		return usagef("actions takes at most one SERVER")
	}
	c, err := e.client()
	if err != nil {
		return err
	}

	if len(positionals) == 1 {
		server, err := e.resolveServer(c, positionals[0])
		if err != nil {
			return err
		}
		record, err := c.GetManifest(e.ctx, server.ID)
		if client.IsNotFound(err) {
			return usagef("%s has no manifest: its plugin has not published one", serverLabel(server))
		}
		if err != nil {
			return err
		}
		if *code != "" {
			action, err := findAction(server, record, *code)
			if err != nil {
				return err
			}
			if e.g.json {
				return e.emitJSON(rawOr(action.Raw, action))
			}
			return e.printAction(action)
		}
		if e.g.json {
			return e.emitJSON(rawOr(record.Raw, record))
		}
		return e.printActionTable(record.Manifest.Actions)
	}

	list, err := c.ListServers(e.ctx)
	if err != nil {
		return err
	}
	servers := list.Servers
	type listed struct {
		Server   any `json:"server"`
		Manifest any `json:"manifest"`
	}
	var all []listed
	for i, server := range servers {
		record, err := c.GetManifest(e.ctx, server.ID)
		missing := client.IsNotFound(err)
		if err != nil && !missing {
			return err
		}
		if e.g.json {
			entry := listed{Server: rawOr(server.Raw, server)}
			if !missing {
				entry.Manifest = rawOr(record.Raw, record)
			}
			all = append(all, entry)
			continue
		}

		if i > 0 {
			fmt.Fprintln(e.stdout)
		}
		fmt.Fprintln(e.stdout, serverLabel(server))
		switch {
		case missing:
			fmt.Fprintln(e.stdout, "no manifest")
		case *code != "":
			action, found := lookupAction(record, *code)
			if !found {
				fmt.Fprintf(e.stdout, "no action %s\n", clean(*code))
				continue
			}
			if err := e.printAction(action); err != nil {
				return err
			}
		default:
			if err := e.printActionTable(record.Manifest.Actions); err != nil {
				return err
			}
		}
	}
	if e.g.json {
		if all == nil {
			all = []listed{}
		}
		return e.emitJSON(all)
	}
	if len(servers) == 0 {
		fmt.Fprintln(e.stdout, "no servers")
	}
	return nil
}

func lookupAction(record client.ManifestRecord, code string) (client.ManifestAction, bool) {
	for _, action := range record.Manifest.Actions {
		if action.Code == code {
			return action, true
		}
	}
	return client.ManifestAction{}, false
}

// findAction looks a code up in a manifest, answering an unknown one with
// the codes that do exist.
func findAction(server client.Server, record client.ManifestRecord, code string) (client.ManifestAction, error) {
	if action, found := lookupAction(record, code); found {
		return action, nil
	}
	codes := make([]string, 0, len(record.Manifest.Actions))
	for _, action := range record.Manifest.Actions {
		codes = append(codes, clean(action.Code))
	}
	slices.Sort(codes)
	if len(codes) == 0 {
		return client.ManifestAction{}, usagef("%s declares no actions", serverLabel(server))
	}
	return client.ManifestAction{}, usagef("%s does not declare %q; its actions: %s",
		serverLabel(server), code, strings.Join(codes, ", "))
}

func (e *env) printActionTable(actions []client.ManifestAction) error {
	if len(actions) == 0 {
		fmt.Fprintln(e.stdout, "no actions")
		return nil
	}
	tw := e.table()
	fmt.Fprintln(tw, "CODE\tCONTEXT\tDANGER\tNAME\tPARAMS")
	for _, action := range actions {
		danger := action.Danger
		if danger == "" {
			danger = "none"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", cell(action.Code), cell(action.Context),
			clean(danger), cell(action.Name), paramsSummary(action.Params))
	}
	return tw.Flush()
}

// paramsSummary renders a params schema as name:type pairs, a * after each
// required one, so the table says what run takes without the whole schema.
func paramsSummary(schema *client.ParamsSchema) string {
	if schema == nil || len(schema.Properties) == 0 {
		return "-"
	}
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	slices.Sort(names)
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = clean(name) + ":" + typeLabel(schema.Properties[name])
		if required[name] {
			parts[i] += "*"
		}
	}
	return strings.Join(parts, " ")
}

func typeLabel(schema *client.ParamsSchema) string {
	switch {
	case schema == nil || schema.Type == "":
		return "any"
	case schema.Type == "array":
		return typeLabel(schema.Items) + "[]"
	}
	return clean(schema.Type)
}

func (e *env) printAction(action client.ManifestAction) error {
	danger := action.Danger
	if danger == "" {
		danger = "none"
	}
	if err := e.keyValues(
		[2]string{"code", cell(action.Code)},
		[2]string{"name", cell(action.Name)},
		[2]string{"context", cell(action.Context)},
		[2]string{"namespace", cell(action.Namespace)},
		[2]string{"danger", clean(danger)},
	); err != nil {
		return err
	}
	params := rawMember(action.Raw, "params")
	if len(params) == 0 || string(params) == "null" {
		fmt.Fprintln(e.stdout, "params: none declared")
		return nil
	}
	fmt.Fprintln(e.stdout, "params:")
	fmt.Fprintln(e.stdout, prettyJSON(params))
	return nil
}

// flagGiven reports whether a flag was on the command line, for flags whose
// zero value is itself meaningful.
func flagGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}
