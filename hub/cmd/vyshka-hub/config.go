package main

// The config file (-config, VYSHKA_HUB_CONFIG): one TOML file as a third way to
// set what the serve flags set, below the command line and the environment.
//
// Precedence, highest first: command-line flag, environment variable, config
// file, built-in default. A hub with no file behaves exactly as it did before
// the file existed, and a unit or container can override one value of a file
// shared by several hubs.
//
// The mapping is generic over the serve flag set. A key is a flag's own name,
// at the top level, and its TOML type follows the flag's (string, integer,
// boolean; a duration is a string such as "30s"). A flag added later is
// accepted in the file with no further code: the environment variable is
// derived from the flag name (envName), and every serve flag has to name that
// variable in its usage text, which a test checks.
//
// Nothing is read unless the operator names a file: no search path, no
// default location. And no value is ever printed on a refusal; an error names
// the file and the key.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
)

// secretFlags are the settings that carry a credential. They take the
// `file:/path` indirection from every source (flag, environment, config
// file), carry no printable default (the flag package prints defaults in
// usage text), and are redacted wherever the hub shows its configuration.
var secretFlags = map[string]bool{
	"db":          true, // a Postgres URL carries a password
	"admin-token": true,
}

// listFlags are the string flags whose value is a comma-separated list. A
// config file may give them as a TOML array of strings as well as the comma
// string; every other flag takes its own type only.
var listFlags = map[string]bool{
	"auto-tls-hosts": true,
}

// envName is the environment variable that backs a flag: VYSHKA_ and the flag
// name upper-cased with hyphens as underscores, except the database URL, which
// keeps the name every Postgres tool uses, and the config file, whose natural
// name VYSHKA_CONFIG belongs to the vyshka command-line client (a JSON profile
// file). The two programs share no variable; a test holds every hub flag's
// name apart from the ones the client reads.
func envName(flagName string) string {
	switch flagName {
	case "db":
		return "DATABASE_URL"
	case "config":
		return "VYSHKA_HUB_CONFIG"
	}
	return "VYSHKA_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// configFlag adds -config to a flag set.
func configFlag(flags *flag.FlagSet) *string {
	return flags.String("config", "",
		"TOML config file whose keys are these flags' names; a flag or environment variable overrides it, "+
			"and no file is read unless named here (env "+envName("config")+")")
}

// configFile is a parsed, not yet validated, config file.
type configFile struct {
	path   string
	dir    string
	values map[string]any
}

// loadConfigFile reads and parses a TOML file. The parser's own messages can
// quote the text it choked on, which for a secret written without quotes is
// the secret, so a syntax error is reported by line and nothing else.
func loadConfigFile(path string) (*configFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	values := map[string]any{}
	if _, err := toml.Decode(string(data), &values); err != nil {
		var parseErr toml.ParseError
		if errors.As(err, &parseErr) {
			if key, ok := duplicateKey(parseErr.Message); ok {
				return nil, fmt.Errorf("config file %s: line %d: key %q is set twice", path, parseErr.Position.Line, key)
			}
			return nil, fmt.Errorf("config file %s: line %d is not valid TOML%s", path, parseErr.Position.Line, syntaxHint(data, parseErr.Position.Line))
		}
		return nil, fmt.Errorf("config file %s is not valid TOML", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return &configFile{path: path, dir: filepath.Dir(abs), values: values}, nil
}

// syntaxHint names a likely cause of a syntax error only where it can tell
// from the bytes without echoing any of them: a file that is not UTF-8
// (a NUL byte, or a UTF-16 byte-order mark, as PowerShell 5.1 redirection
// writes), or a backslash inside a double-quoted string on the failing line,
// which TOML reads as an escape (a Windows path wants single quotes).
func syntaxHint(data []byte, line int) string {
	if strings.IndexByte(string(data), 0) >= 0 || strings.HasPrefix(string(data), "\xff\xfe") || strings.HasPrefix(string(data), "\xfe\xff") {
		return " (the file is not UTF-8: save it as UTF-8)"
	}
	lines := strings.Split(string(data), "\n")
	if line >= 1 && line <= len(lines) {
		text := lines[line-1]
		if quote := strings.IndexByte(text, '"'); quote >= 0 && strings.IndexByte(text[quote:], '\\') >= 0 {
			return " (a backslash inside a double-quoted string starts an escape: write a Windows path in single quotes, or double each backslash)"
		}
	}
	return ""
}

// duplicateKey pulls the key out of the parser's duplicate-key message
// ("Key 'a.b' has already been defined."), which carries no value.
func duplicateKey(message string) (string, bool) {
	rest, ok := strings.CutPrefix(message, "Key '")
	if !ok {
		return "", false
	}
	key, ok := strings.CutSuffix(rest, "' has already been defined.")
	return key, ok
}

// tomlType names a decoded TOML value's type for an error message.
func tomlType(value any) string {
	switch value.(type) {
	case string:
		return "string"
	case int64:
		return "integer"
	case float64:
		return "float"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "table"
	default:
		return "date or time"
	}
}

// flagValueText turns a TOML value into the text a flag's Set expects, or
// says what type the key wanted. The wanted type comes from the flag's own
// value, so a new flag of an ordinary type needs nothing here.
func flagValueText(f *flag.Flag, value any) (string, error) {
	wrong := func(want string) error {
		return fmt.Errorf("is %s, want %s", withArticle(tomlType(value)), want)
	}
	var wanted any
	if getter, ok := f.Value.(flag.Getter); ok {
		wanted = getter.Get()
	}
	switch wanted.(type) {
	case bool:
		b, ok := value.(bool)
		if !ok {
			return "", wrong("a boolean")
		}
		return strconv.FormatBool(b), nil
	case int, int64, uint, uint64:
		n, ok := value.(int64)
		if !ok {
			return "", wrong("an integer")
		}
		return strconv.FormatInt(n, 10), nil
	case float64:
		switch n := value.(type) {
		case int64:
			return strconv.FormatInt(n, 10), nil
		case float64:
			return strconv.FormatFloat(n, 'g', -1, 64), nil
		}
		return "", wrong("a number")
	case time.Duration:
		s, ok := value.(string)
		if !ok {
			return "", wrong(`a string such as "30s"`)
		}
		return s, nil
	}
	if listFlags[f.Name] {
		if items, ok := value.([]any); ok {
			names := make([]string, 0, len(items))
			for _, item := range items {
				s, ok := item.(string)
				if !ok {
					return "", fmt.Errorf("is an array holding %s, want strings", withArticle(tomlType(item)))
				}
				if strings.Contains(s, ",") {
					return "", errors.New("has an array element with a comma in it; give one name per element")
				}
				names = append(names, s)
			}
			return strings.Join(names, ","), nil
		}
		s, ok := value.(string)
		if !ok {
			return "", wrong("a string (comma-separated) or an array of strings")
		}
		return s, nil
	}
	s, ok := value.(string)
	if !ok {
		return "", wrong("a string")
	}
	return s, nil
}

func withArticle(noun string) string {
	if strings.ContainsAny(noun[:1], "aeiou") {
		return "an " + noun
	}
	return "a " + noun
}

// inlineSecrets is the one warning about a config file that holds secrets
// written into it and can be read by others: the keys, never the values.
type inlineSecrets struct {
	file string
	keys []string
	mode fs.FileMode
}

// settings is how a flag set was resolved: where each value came from, and
// what to warn about once a logger exists.
type settings struct {
	// configPath and configFrom say which file was loaded (empty for none)
	// and how it was named ("flag" or "env VYSHKA_HUB_CONFIG").
	configPath string
	configFrom string
	// sources maps a flag name to "flag", "env NAME", "file", or "default".
	sources map[string]string
	// warning is set when inline secrets sit in a file readable by others.
	warning *inlineSecrets
}

// looseMode reports whether a file mode lets others (the world bit) read the
// file. Group access is a normal layout: a file shared with the service's own
// group, or a systemd credential, whose group bits are an ACL mask.
func looseMode(mode fs.FileMode) bool { return mode.Perm()&0o004 != 0 }

// dbCarriesPassword reports whether a database setting holds a credential:
// a URL with a password (or any password-like query parameter, or a URL that
// does not parse, treated as carrying one), or a libpq keyword string. A plain
// SQLite path is not a secret.
func dbCarriesPassword(text string) bool {
	scheme, rest, isURL := strings.Cut(text, "://")
	if !isURL {
		// A path holds no credential. Anything with an @ or a key=value pair
		// is a DSN written without its scheme, or a libpq keyword string.
		return strings.ContainsAny(text, "@=") || strings.Contains(strings.ToLower(text), "password")
	}
	if scheme == "sqlite" {
		return false
	}
	// An @ counts wherever a URL parser would put it: a password with an
	// unescaped / moves the userinfo into what Go calls the host and path.
	// The one shape with an @ and no password is a single @ whose left side
	// is a bare user name.
	if userinfo, _, found := strings.Cut(rest, "@"); found {
		if strings.Count(rest, "@") != 1 || strings.ContainsAny(userinfo, ":/?#") {
			return true
		}
	}
	parsed, err := url.Parse(text)
	if err != nil {
		return true
	}
	for key := range parsed.Query() {
		if strings.Contains(strings.ToLower(key), "pass") {
			return true
		}
	}
	return false
}

// inlineSecretText reports whether a value written into the config file is a
// secret held inline, as opposed to a file: reference or a non-secret.
func inlineSecretText(name, text string) bool {
	if text == "" || strings.HasPrefix(text, "file:") {
		return false
	}
	if name == "db" {
		return dbCarriesPassword(text)
	}
	return true
}

// resolveSettings settles every flag in flags after Parse: the command line
// wins, then a non-empty environment variable, then the config file, then the
// built-in default. known is the flag set whose names are legal keys in the
// file; it is flags itself for serve, and serve's set for the maintenance
// commands, which apply only the keys they have a flag for. It is called only
// when a file is named, so the maintenance commands do not depend on serve's
// environment otherwise.
func resolveSettings(flags *flag.FlagSet, known func() (*flag.FlagSet, error)) (*settings, error) {
	onCommandLine := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { onCommandLine[f.Name] = true })

	result := &settings{sources: map[string]string{}}
	if configFlag := flags.Lookup("config"); configFlag != nil {
		switch {
		case onCommandLine["config"]:
			result.configPath, result.configFrom = configFlag.Value.String(), "flag"
		case os.Getenv(envName("config")) != "":
			result.configPath, result.configFrom = os.Getenv(envName("config")), "env "+envName("config")
		}
	}

	var file *configFile
	texts := map[string]string{}
	var inline []string
	if result.configPath != "" {
		var err error
		if file, err = loadConfigFile(result.configPath); err != nil {
			return nil, err
		}
		keys, err := known()
		if err != nil {
			return nil, err
		}
		if err := checkKeys(file, keys); err != nil {
			return nil, err
		}
		// Every key is held to its flag's type, whether or not a flag or the
		// environment overrides it: a wrong file is wrong whatever wins.
		names := make([]string, 0, len(file.values))
		for name := range file.values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			text, err := flagValueText(keys.Lookup(name), file.values[name])
			if err != nil {
				return nil, fmt.Errorf("config file %s: key %q %w", file.path, name, err)
			}
			if secretFlags[name] {
				text = file.anchorSecretPath(text)
				if inlineSecretText(name, text) {
					inline = append(inline, name)
				}
			}
			texts[name] = text
		}
		result.warn(file, inline)
	}

	var failure error
	flags.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" || failure != nil {
			return
		}
		switch envKey := envName(f.Name); {
		case onCommandLine[f.Name]:
			result.sources[f.Name] = "flag"
		case os.Getenv(envKey) != "":
			result.sources[f.Name] = "env " + envKey
			if secretFlags[f.Name] {
				// Secrets do not carry their environment value as the flag
				// default (usage text prints defaults), so it is set here.
				f.Value.Set(os.Getenv(envKey))
			}
		case file != nil && file.has(f.Name):
			if err := f.Value.Set(texts[f.Name]); err != nil {
				failure = fmt.Errorf("config file %s: key %q is not a valid value for this setting", file.path, f.Name)
				return
			}
			result.sources[f.Name] = "file"
		default:
			result.sources[f.Name] = "default"
		}
	})
	if failure != nil {
		return nil, failure
	}
	return result, nil
}

func (c *configFile) has(name string) bool { _, ok := c.values[name]; return ok }

// anchorSecretPath makes a relative `file:` reference from the config file
// relative to the file's own directory, so a file and its secrets move
// together and the working directory does not matter. Flags and the
// environment keep meaning the working directory, as they always did.
func (c *configFile) anchorSecretPath(text string) string {
	path, isFile := strings.CutPrefix(text, "file:")
	if !isFile || path == "" || filepath.IsAbs(path) {
		return text
	}
	return "file:" + filepath.Join(c.dir, path)
}

// hasPermissionBits is false where a file mode says nothing about who can read
// the file (Windows); tests turn it on to exercise the warning there.
var hasPermissionBits = runtime.GOOS != "windows"

// warn records the one warning for inline secrets (keys) in a file others can
// read. Windows has no permission bits to read.
func (r *settings) warn(file *configFile, keys []string) {
	if len(keys) == 0 || !hasPermissionBits {
		return
	}
	info, err := os.Stat(file.path)
	if err != nil || !looseMode(info.Mode()) {
		return
	}
	sort.Strings(keys)
	r.warning = &inlineSecrets{file: file.path, keys: keys, mode: info.Mode().Perm()}
}

// warningText is the warning in words; it names the file and the keys.
func (w *inlineSecrets) text() string {
	return fmt.Sprintf("config file %s holds inline secrets (%s) and is readable by others (mode %04o); "+
		"use file: references, or remove the world read bit (chmod o-r)", w.file, strings.Join(w.keys, ", "), w.mode)
}

// checkKeys refuses a file whose keys are not all flags of known, naming the
// file and the keys and never a value. `config` is not a key: a file cannot
// name another file.
func checkKeys(file *configFile, known *flag.FlagSet) error {
	var unknown []string
	for name := range file.values {
		if name == "config" || known.Lookup(name) == nil {
			unknown = append(unknown, strconv.Quote(name))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		noun := "key"
		if len(unknown) > 1 {
			noun = "keys"
		}
		return fmt.Errorf("config file %s: unknown %s %s (a key is a serve flag's name, without the dash)",
			file.path, noun, strings.Join(unknown, ", "))
	}
	return nil
}

// redactSecrets is the switch the redaction test turns off to prove its
// assertion can fail. Nothing else sets it.
var redactSecrets = true

// displayValue is what the hub may show for a setting. A secret appears only
// as the path of the file it will be read from, or redacted. The database is
// a secret only when it carries a credential: a plain SQLite path is shown,
// and a URL is shown as scheme, host, and database name with the user, the
// password, and the query (which libpq lets carry a password) redacted, so an
// operator can see which database won.
func displayValue(f *flag.Flag) string {
	value := f.Value.String()
	if !secretFlags[f.Name] {
		return value
	}
	switch {
	case value == "":
		return "(unset)"
	case !redactSecrets:
		return value
	case strings.HasPrefix(value, "file:"):
		return value
	case f.Name == "db":
		return displayDB(value)
	default:
		return "<redacted>"
	}
}

// displayDB is the redacted form of a database setting that is not a file:
// reference. Only a value that is plainly a path (no URL, no whitespace, no
// key=value pairs) is shown whole; anything else that does not parse as a URL
// is withheld.
//
// A URL is cut by its own text, not by a URL parser: parsers disagree about
// where the userinfo ends when a password holds an unescaped /, ?, #, or @
// (Go takes the authority up to the first /, the Postgres driver the userinfo
// up to the first @), and a redaction built on one of them prints what the
// other calls the password. So everything up to the last @ is withheld, the
// query is withheld, and a value whose last @ comes after a ? or # (a
// password holding one, or an @ inside the query: the two cannot be told
// apart) is withheld whole.
func displayDB(value string) string {
	scheme, rest, isURL := strings.Cut(value, "://")
	if !isURL {
		if strings.ContainsAny(value, " \t=@") {
			return "<redacted>"
		}
		return value
	}
	if scheme == "sqlite" {
		return value
	}
	if !plainScheme(scheme) {
		return "<redacted>"
	}
	if _, err := url.Parse(value); err != nil {
		return "<redacted>"
	}
	shown := scheme + "://"
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		if strings.ContainsAny(rest[:at], "?#") {
			return shown + "<redacted>"
		}
		shown += "<redacted>@"
		rest = rest[at+1:]
	}
	location, _, hasQuery := strings.Cut(rest, "?")
	location, _, _ = strings.Cut(location, "#")
	shown += location
	if hasQuery {
		shown += "?<redacted>"
	}
	return shown
}

// plainScheme reports whether text is shaped like a URL scheme, so that the
// part of a setting before :// is safe to print.
func plainScheme(text string) bool {
	if text == "" || len(text) > 32 {
		return false
	}
	for _, c := range text {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '+', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// describe says where a setting came from, for an error message.
func (r *settings) describe(name string) string {
	switch source := r.sources[name]; {
	case source == "file":
		return "config file " + r.configPath
	case source == "flag":
		return "-" + name
	case strings.HasPrefix(source, "env "):
		return "environment variable " + strings.TrimPrefix(source, "env ")
	default:
		return "default"
	}
}

// dbMoved is appended to a refusal of a db: reference: before the config file
// existed, a value starting file: was a SQLite URI.
const dbMoved = "; db: file:PATH now names a file that holds the database URL, " +
	"and a SQLite URI is written sqlite://file:... (see deploy/README.md, \"The config file\")"

// secret resolves the file: indirection of a secret-bearing setting, from
// whichever source supplied it. A failure names the setting and its source
// and the secret file's path, never a value. For db the file must hold a
// postgres://, postgresql://, or sqlite:// URL: anything else is withheld, so
// a file that was never meant as a secret (an old SQLite URI named a database
// file) cannot become the database setting or be echoed by a later error.
func (r *settings) secret(name, value string) (string, error) {
	path, isRef := strings.CutPrefix(value, "file:")
	if name == "db" && !isRef && keywordDSN(value) {
		// The hub would take it for a SQLite file name, create that file, and
		// log the name, password and all, as the database it opened.
		return "", fmt.Errorf("db (from %s): a key=value connection string is not supported; write a postgres:// URL",
			r.describe(name))
	}
	if name == "db" && isRef && strings.Contains(path, "?") {
		return "", fmt.Errorf("db (from %s): file:%s names a path with ?, which is a SQLite URI query%s",
			r.describe(name), path, dbMoved)
	}
	resolved, err := resolveSecret(value)
	if err == nil && name == "db" && isRef && !hasDBScheme(resolved) {
		err = fmt.Errorf("%s does not hold a postgres://, postgresql://, or sqlite:// URL", path)
	}
	if err != nil {
		if name == "db" && isRef {
			err = fmt.Errorf("%w%s", err, dbMoved)
		}
		return "", fmt.Errorf("%s (from %s): %w", name, r.describe(name), err)
	}
	return resolved, nil
}

// keywordDSN reports whether a database setting is a libpq keyword string
// (host=... user=... password=...) rather than a URL or a path.
func keywordDSN(text string) bool {
	if strings.Contains(text, "://") {
		return false
	}
	// libpq allows spaces around the = of a pair; close them up first.
	joined := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	for _, spaced := range []string{" = ", " =", "= "} {
		joined = strings.ReplaceAll(joined, spaced, "=")
	}
	for _, field := range strings.Fields(joined) {
		for _, key := range []string{"host=", "hostaddr=", "port=", "user=", "password=", "dbname=", "sslmode="} {
			if strings.HasPrefix(field, key) {
				return true
			}
		}
	}
	return false
}

func hasDBScheme(dsn string) bool {
	for _, scheme := range []string{"postgres://", "postgresql://", "sqlite://"} {
		if strings.HasPrefix(dsn, scheme) {
			return true
		}
	}
	return false
}

// log reports the config file in use and any warning about it, once a logger
// exists. Neither carries a value.
func (r *settings) log(logger *slog.Logger) {
	if r.configPath != "" {
		logger.Info("config file", "path", r.configPath, "via", r.configFrom)
	}
	if w := r.warning; w != nil {
		logger.Warn("config file holds inline secrets and is readable by others; "+
			"use file: references, or remove the world read bit",
			"file", w.file, "keys", strings.Join(w.keys, ","), "mode", fmt.Sprintf("%04o", w.mode))
	}
}

// print writes the effective configuration: each setting, its value (secrets
// redacted), and where the value came from. It reads the flag values, which
// hold a secret only as typed (inline, or as a file: reference), never as
// read from a file.
func (r *settings) print(out, errOut io.Writer, flags *flag.FlagSet, listenAddr string) error {
	if r.warning != nil {
		fmt.Fprintln(errOut, "warning: "+r.warning.text())
	}
	if r.configPath == "" {
		fmt.Fprintln(out, "config file: none (name one with -config or VYSHKA_HUB_CONFIG)")
	} else {
		fmt.Fprintf(out, "config file: %s (from %s)\n", r.configPath, r.configFrom)
	}
	fmt.Fprintln(out, "precedence: flag, then environment variable, then config file, then default")
	fmt.Fprintln(out)
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "SETTING\tVALUE\tSOURCE")
	flags.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" {
			return
		}
		value := displayValue(f)
		if value == "" {
			value = `""`
		}
		fmt.Fprintf(table, "%s\t%s\t%s\n", f.Name, value, r.sources[f.Name])
	})
	if err := table.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nserve would listen on %s\n", listenAddr)
	return nil
}
