package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/client"
)

func cmdKV(e *env, args []string) error {
	if len(args) == 0 {
		return usagef("kv takes get, set, incr, delete, list, or namespaces; run \"vyshka help kv\" for usage")
	}
	switch args[0] {
	case "get":
		return cmdKVGet(e, args[1:])
	case "set":
		return cmdKVSet(e, args[1:])
	case "incr":
		return cmdKVIncr(e, args[1:])
	case "delete":
		return cmdKVDelete(e, args[1:])
	case "list":
		return cmdKVList(e, args[1:])
	case "namespaces":
		return cmdKVNamespaces(e, args[1:])
	case "-h", "--help", "-help":
		return &helpRequest{command: "kv"}
	}
	return usagef("unknown kv subcommand %q; want get, set, incr, delete, list, or namespaces", args[0])
}

// kvArgs parses a kv subcommand that takes NS and KEY.
func (e *env) kvArgs(name string, positionals []string, extra int) (string, string, error) {
	if len(positionals) != 2+extra {
		if extra == 0 {
			return "", "", usagef("kv %s takes NS and KEY", name)
		}
		return "", "", usagef("kv %s takes NS, KEY, and VALUE", name)
	}
	return positionals[0], positionals[1], nil
}

// printValueLine prints a value as compact JSON on stdout, the whole of
// stdout, so a shell substitution gets exactly the value.
func (e *env) printValueLine(value json.RawMessage) {
	fmt.Fprintln(e.stdout, compactJSON(value))
}

// printKVMeta prints a key's revision and expiry on stderr, beside a value
// printed alone on stdout.
func (e *env) printKVMeta(revision int64, expiresAt *time.Time) {
	fmt.Fprintf(e.stderr, "revision %d\n", revision)
	if expiresAt != nil {
		fmt.Fprintf(e.stderr, "expires %s\n", formatTime(*expiresAt))
	}
}

func cmdKVGet(e *env, args []string) error {
	fs, apply := e.flagSet("kv get")
	positionals, err := e.parse(fs, apply, args, "kv")
	if err != nil {
		return err
	}
	namespace, key, err := e.kvArgs("get", positionals, 0)
	if err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	entry, err := c.KVGet(e.ctx, namespace, key)
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(entry.Raw, entry))
	}
	e.printValueLine(entry.Value)
	e.printKVMeta(entry.Revision, entry.ExpiresAt)
	return nil
}

func cmdKVSet(e *env, args []string) error {
	fs, apply := e.flagSet("kv set")
	ifRevision := fs.Int64("if-revision", 0, "")
	ttl := fs.Duration("ttl", 0, "")
	asString := fs.Bool("string", false, "")
	positionals, err := e.parse(fs, apply, args, "kv")
	if err != nil {
		return err
	}
	namespace, key, err := e.kvArgs("set", positionals, 1)
	if err != nil {
		return err
	}
	text := positionals[2]

	var request client.KVSetRequest
	trimmed := strings.TrimSpace(text)
	switch {
	case *asString:
		request.Value = text
	case json.Valid([]byte(trimmed)):
		// Sent byte for byte, so a large integer is not rounded through a
		// float64 on its way to the store.
		if trimmed == "null" {
			return usagef("a KV value cannot be null; pass --string to store the text null")
		}
		request.Value = json.RawMessage(trimmed)
	default:
		request.Value = text
	}
	if flagGiven(fs, "if-revision") {
		if *ifRevision < 0 {
			return usagef("--if-revision must be 0 (only if the key does not exist) or a revision")
		}
		request.IfRevision = ifRevision
	}
	if flagGiven(fs, "ttl") {
		if request.TTLSeconds, err = seconds("ttl", *ttl); err != nil {
			return err
		}
	}

	c, err := e.client()
	if err != nil {
		return err
	}
	result, err := c.KVSet(e.ctx, namespace, key, request)
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(result.Raw, result))
	}
	fmt.Fprintf(e.stdout, "revision %d\n", result.Revision)
	if result.ExpiresAt != nil {
		fmt.Fprintf(e.stdout, "expires %s\n", formatTimePtr(result.ExpiresAt, "-"))
	}
	return nil
}

func cmdKVIncr(e *env, args []string) error {
	fs, apply := e.flagSet("kv incr")
	delta := fs.Int64("delta", 1, "")
	positionals, err := e.parse(fs, apply, args, "kv")
	if err != nil {
		return err
	}
	namespace, key, err := e.kvArgs("incr", positionals, 0)
	if err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var sent *int64
	if flagGiven(fs, "delta") {
		sent = delta
	}
	entry, err := c.KVIncr(e.ctx, namespace, key, sent)
	if err != nil {
		return err
	}
	if e.g.json {
		return e.emitJSON(rawOr(entry.Raw, entry))
	}
	e.printValueLine(entry.Value)
	e.printKVMeta(entry.Revision, entry.ExpiresAt)
	return nil
}

func cmdKVDelete(e *env, args []string) error {
	fs, apply := e.flagSet("kv delete")
	positionals, err := e.parse(fs, apply, args, "kv")
	if err != nil {
		return err
	}
	namespace, key, err := e.kvArgs("delete", positionals, 0)
	if err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	outcome := "deleted"
	if err := c.KVDelete(e.ctx, namespace, key); err != nil {
		// Spec section 12.2: a delete that finds the key gone treats it as
		// success, since the key is gone either way.
		if !client.IsNotFound(err) {
			return err
		}
		outcome = "already absent"
	}
	if e.g.json {
		fmt.Fprintln(e.stderr, outcome)
		return nil
	}
	fmt.Fprintln(e.stdout, outcome)
	return nil
}

func cmdKVList(e *env, args []string) error {
	fs, apply := e.flagSet("kv list")
	prefix := fs.String("prefix", "", "")
	limit := fs.Int("limit", 0, "")
	all := fs.Bool("all", false, "")
	positionals, err := e.parse(fs, apply, args, "kv")
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usagef("kv list takes NS")
	}
	if *limit < 0 {
		return usagef("--limit must not be negative")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	query := client.KVListQuery{Prefix: *prefix, Limit: *limit}
	tw := e.table()
	if !e.g.json {
		fmt.Fprintln(tw, "KEY\tREVISION\tEXPIRES")
	}
	more := false
	for {
		page, err := c.KVListKeys(e.ctx, positionals[0], query)
		if err != nil {
			return err
		}
		if e.g.json {
			if err := e.emitJSON(rawOr(page.Raw, page)); err != nil {
				return err
			}
		} else {
			for _, key := range page.Keys {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", cell(key.Key), strconv.FormatInt(key.Revision, 10),
					formatTimePtr(key.ExpiresAt, "never"))
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
		fmt.Fprintln(e.stderr, "more keys follow; pass --all to walk every page")
	}
	return nil
}

func cmdKVNamespaces(e *env, args []string) error {
	fs, apply := e.flagSet("kv namespaces")
	positionals, err := e.parse(fs, apply, args, "kv")
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return usagef("kv namespaces takes no arguments")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	namespaces, err := c.KVListNamespaces(e.ctx)
	if err != nil {
		return err
	}
	if e.g.json {
		if namespaces == nil {
			namespaces = []client.KVNamespace{}
		}
		return e.emitJSON(map[string]any{"namespaces": namespaces})
	}
	if len(namespaces) == 0 {
		fmt.Fprintln(e.stdout, "no namespaces hold keys this token may read")
		return nil
	}
	tw := e.table()
	fmt.Fprintln(tw, "NAMESPACE\tKEYS")
	for _, namespace := range namespaces {
		fmt.Fprintf(tw, "%s\t%d\n", cell(namespace.Namespace), namespace.Keys)
	}
	return tw.Flush()
}
