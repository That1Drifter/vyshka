package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
)

// errWriter remembers the first write failure, which is how events --follow
// learns that whatever it was piping into has gone away.
type errWriter struct {
	w   io.Writer
	err error
}

func (w *errWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.w.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

// table returns a writer for human-readable columns. Cells are separated by
// tabs and must be passed through clean first.
func (e *env) table() *tabwriter.Writer {
	return tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
}

// emitJSON prints one value as one line of JSON. json.RawMessage values are
// compacted, which is what turns the hub's indented answers into one object
// per line. HTML escaping is off so the hub's strings come out as it sent
// them.
func (e *env) emitJSON(value any) error {
	encoder := json.NewEncoder(e.stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

// rawOr returns a record's raw JSON, or the record itself re-encoded when it
// carries none (it was built locally rather than read from the hub).
func rawOr(raw json.RawMessage, record any) any {
	if len(raw) > 0 {
		return raw
	}
	return record
}

// clean makes a string from the hub safe to print as one table cell or one
// line: tabs and newlines would break the layout, and control characters
// (an escape sequence in a player name, say) would reach the terminal.
func clean(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r):
			return unicode.ReplacementChar
		}
		return r
	}, s)
}

// cell cleans a value for a table and shows an empty one as "-".
func cell(s string) string {
	if s == "" {
		return "-"
	}
	return clean(s)
}

// formatTime prints a time as RFC 3339 in UTC.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func formatTimePtr(t *time.Time, none string) string {
	if t == nil {
		return none
	}
	return formatTime(*t)
}

// age prints how long ago t was, coarsely: 12s, 3m, 2h, 5d. A time ahead of
// the local clock, which clock skew produces, reads as 0s.
func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// formatPosition prints a position the way run takes a vector param back:
// x,y[,z].
func formatPosition(position []float64) string {
	if len(position) == 0 {
		return "-"
	}
	parts := make([]string, len(position))
	for i, value := range position {
		parts[i] = strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strings.Join(parts, ",")
}

// prettyJSON indents raw JSON for reading; what does not parse is shown
// cleaned, as it came.
func prettyJSON(raw json.RawMessage) string {
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return clean(string(raw))
	}
	return out.String()
}

// compactJSON renders raw JSON on one line.
func compactJSON(raw json.RawMessage) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return clean(string(raw))
	}
	return out.String()
}

// rawMember extracts one member of a raw JSON object, verbatim.
func rawMember(raw json.RawMessage, name string) json.RawMessage {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return nil
	}
	return members[name]
}

// truncate shortens s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-3]) + "..."
}

// keyValues prints aligned `key: value` lines.
func (e *env) keyValues(pairs ...[2]string) error {
	tw := e.table()
	for _, pair := range pairs {
		fmt.Fprintf(tw, "%s:\t%s\n", pair[0], pair[1])
	}
	return tw.Flush()
}
