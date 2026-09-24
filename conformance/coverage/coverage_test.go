// Package coverage holds one test: that conformance/COVERAGE.md still
// accounts for every normative clause in spec/protocol.md, clause for clause,
// and that every check it names is still declared in one of the two suites.
//
// A protocol edit that adds or removes a MUST, MUST NOT, or REQUIRED clause,
// or rewords the words a row quotes, fails this test until the table is
// brought along, which is the point: the table is only worth reading if it
// cannot quietly go stale. Like everything under conformance/, this reads the
// spec and the suites as text and imports nothing from the hub or a plugin.
package coverage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	specPath  = "../../spec/protocol.md"
	tablePath = "../COVERAGE.md"
)

// keyword matches one normative clause. MUST NOT is one clause, counted at
// its MUST.
var keyword = regexp.MustCompile(`\b(MUST|REQUIRED)\b`)

// atxHeading matches any ATX heading, and numbered one that carries a section
// number ("### 3.1.2 The poll exchange", "## 5. Enrollment and sessions"),
// capturing the number without its trailing dot and the title after it.
var (
	atxHeading = regexp.MustCompile(`^ {0,3}#{1,6}(?:[ \t]|$)`)
	numbered   = regexp.MustCompile(`^ {0,3}#{2,6}[ \t]+(\d+(?:\.\d+)*)\.?(?:[ \t]+(.*))?$`)
	appendix   = regexp.MustCompile(`^ {0,3}#{2}[ \t]+Appendix\b`)
)

// excluded sections restate the RFC 2119 keywords rather than use them.
var excluded = map[string]bool{"1.1": true}

// statuses a row may carry, and whether the status requires check IDs.
var statuses = map[string]bool{
	"graded":   true,
	"partial":  true,
	"ungraded": false,
	"n/a":      false,
}

var binds = map[string]bool{
	"hub":          true,
	"plugin":       true,
	"either":       true,
	"sender":       true,
	"admin client": true,
	"receiver":     true,
}

var spaces = regexp.MustCompile(`\s+`)

func normalize(s string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// lines splits a file into lines, CRLF or LF.
func lines(raw []byte) []string {
	return strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
}

// fences tracks CommonMark fenced code blocks: a run of at least three
// backticks or tildes, indented at most three spaces, closed only by a run of
// the same character at least as long with nothing after it.
type fences struct {
	char byte
	n    int
}

// inside reports whether line is part of a fenced block, the fence lines
// themselves included, and advances the state.
func (f *fences) inside(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || trimmed == "" || (trimmed[0] != '`' && trimmed[0] != '~') {
		return f.n > 0
	}
	char := trimmed[0]
	run := len(trimmed) - len(strings.TrimLeft(trimmed, string(char)))
	if run < 3 {
		return f.n > 0
	}
	if f.n == 0 {
		if char == '`' && strings.Contains(trimmed[run:], "`") {
			// A backtick fence's info string may not hold a backtick; this is
			// inline code, not a fence.
			return false
		}
		f.char, f.n = char, run
		return true
	}
	if char == f.char && run >= f.n && strings.TrimSpace(trimmed[run:]) == "" {
		f.n = 0
	}
	return true
}

// section is one numbered, normative section of the spec: its text with
// whitespace normalized, and where each clause keyword in it sits.
type section struct {
	text    string
	clauses map[int]bool
}

// specSections returns every numbered, normative section, keyed by section
// number. Fenced code blocks are left out, headings count as part of the
// section they open, and everything from the first appendix on is left out
// (informative). Past the first numbered heading, every heading must be
// numbered: an unnumbered one would take its clauses out of every section.
func specSections(t *testing.T) map[string]section {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	sections := map[string]section{}
	var current string
	var started bool
	var buf []string
	var f fences
	flush := func() {
		if current != "" && !excluded[current] {
			text := normalize(strings.Join(buf, " "))
			clauses := map[int]bool{}
			for _, loc := range keyword.FindAllStringIndex(text, -1) {
				clauses[loc[0]] = true
			}
			sections[current] = section{text: text, clauses: clauses}
		}
		buf = nil
	}
	for i, line := range lines(raw) {
		if f.inside(line) {
			continue
		}
		if appendix.MatchString(line) {
			break
		}
		if m := numbered.FindStringSubmatch(line); m != nil {
			flush()
			if _, dup := sections[m[1]]; dup || m[1] == current {
				t.Errorf("protocol.md:%d: section %s is numbered twice; its clauses cannot be told apart", i+1, m[1])
			}
			current, started = m[1], true
			buf = append(buf, m[2])
			continue
		}
		if atxHeading.MatchString(line) {
			if started {
				t.Errorf("protocol.md:%d: an unnumbered heading inside the normative sections would take its clauses out of every section; number it: %q", i+1, line)
			}
			flush()
			current = ""
			continue
		}
		buf = append(buf, line)
	}
	flush()
	if f.n > 0 {
		t.Errorf("protocol.md: a fenced code block is never closed, so everything after it would go unread")
	}
	return sections
}

type row struct {
	line    int
	section string
	excerpt string
	binds   string
	checks  []string
	status  string
	notes   string
}

// splitCells splits a Markdown table row on unescaped pipes.
func splitCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' && i+1 < len(line) && line[i+1] == '|' {
			cell.WriteByte('|')
			i++
			continue
		}
		if line[i] == '|' {
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
			continue
		}
		cell.WriteByte(line[i])
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

var (
	sectionCell = regexp.MustCompile(`^\d+(?:\.\d+)*$`)
	summaryRow  = regexp.MustCompile(`^\| (graded|partial|ungraded|n/a) \|`)
	totalLine   = regexp.MustCompile(`^(\d+) clauses in all\.$`)
)

// table is the coverage table as rendered: its clause rows, its summary rows
// by status (every occurrence, so a duplicate shows), and its total line.
type table struct {
	rows    []row
	summary map[string][]string
	totals  []string
}

// readTable parses the coverage table, skipping anything inside a fenced code
// block, which renders as text rather than as a row.
func readTable(t *testing.T) table {
	t.Helper()
	raw, err := os.ReadFile(tablePath)
	if err != nil {
		t.Fatal(err)
	}
	tbl := table{summary: map[string][]string{}}
	var f fences
	for i, line := range lines(raw) {
		if f.inside(line) {
			continue
		}
		if m := totalLine.FindStringSubmatch(line); m != nil {
			tbl.totals = append(tbl.totals, m[1])
			continue
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := splitCells(line)
		if m := summaryRow.FindStringSubmatch(line); m != nil && len(cells) >= 2 {
			tbl.summary[m[1]] = append(tbl.summary[m[1]], cells[1])
			continue
		}
		if !sectionCell.MatchString(cells[0]) {
			continue
		}
		if len(cells) != 6 {
			t.Errorf("COVERAGE.md:%d: a clause row has 6 cells (section, clause, binds, graded by, status, notes), got %d", i+1, len(cells))
			continue
		}
		excerpt := strings.TrimSpace(cells[1])
		if len(excerpt) >= 2 && strings.HasPrefix(excerpt, `"`) && strings.HasSuffix(excerpt, `"`) {
			excerpt = excerpt[1 : len(excerpt)-1]
		}
		r := row{
			line:    i + 1,
			section: cells[0],
			excerpt: normalize(excerpt),
			binds:   cells[2],
			status:  cells[4],
			notes:   cells[5],
		}
		if cells[3] != "-" {
			for _, id := range strings.Split(cells[3], ",") {
				r.checks = append(r.checks, strings.Trim(strings.TrimSpace(id), "`"))
			}
		}
		tbl.rows = append(tbl.rows, r)
	}
	return tbl
}

// suiteIDs returns every check ID the two suites declare, as "hub:<id>" and
// "plugin:<id>": the ID field of every Check literal in the hub suite and
// every Stage literal in the plugin suite, read from the syntax tree so that
// a comment, a result, or a fixture that happens to carry an ID field is not
// mistaken for a check. An ID that is not a string literal is refused rather
// than skipped, so a change in how checks are declared fails loudly.
func suiteIDs(t *testing.T) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for suite, typeName := range map[string]string{"hub": "Check", "plugin": "Stage"} {
		files, err := filepath.Glob(filepath.Join("..", suite, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			record := func(lit *ast.CompositeLit) {
				for _, element := range lit.Elts {
					field, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := field.Key.(*ast.Ident); !ok || key.Name != "ID" {
						continue
					}
					value, ok := field.Value.(*ast.BasicLit)
					if !ok || value.Kind != token.STRING {
						t.Errorf("%s: a %s ID that is not a string literal cannot be read", fset.Position(field.Pos()), typeName)
						continue
					}
					id, err := strconv.Unquote(value.Value)
					if err != nil {
						t.Errorf("%s: %v", fset.Position(value.Pos()), err)
						continue
					}
					ids[suite+":"+id] = true
				}
			}
			names := func(expr ast.Expr) bool {
				ident, ok := expr.(*ast.Ident)
				return ok && ident.Name == typeName
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				lit, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if names(lit.Type) {
					record(lit)
				}
				// Elements of a []Check or []Stage literal elide their type.
				if array, ok := lit.Type.(*ast.ArrayType); ok && names(array.Elt) {
					for _, element := range lit.Elts {
						if inner, ok := element.(*ast.CompositeLit); ok && inner.Type == nil {
							record(inner)
						}
					}
				}
				return true
			})
		}
	}
	if len(ids) == 0 {
		t.Fatal("found no Check or Stage literals in conformance/hub or conformance/plugin; the suites no longer declare checks the way this test reads them")
	}
	return ids
}

func TestCoverageTableMatchesSpec(t *testing.T) {
	sections := specSections(t)
	tbl := readTable(t)
	ids := suiteIDs(t)

	// Pin every row to the one clause it accounts for: the keyword its
	// excerpt holds must sit, in the spec, exactly where a clause keyword
	// sits, which keeps a word like MUSTARD from standing in for a clause.
	pinned := map[string]map[int]int{} // section -> clause offset -> row line
	for _, r := range tbl.rows {
		sec, ok := sections[r.section]
		if !ok {
			t.Errorf("COVERAGE.md:%d: section %s is not a normative section of the spec", r.line, r.section)
			continue
		}
		inExcerpt := keyword.FindAllStringIndex(r.excerpt, -1)
		if len(inExcerpt) != 1 {
			t.Errorf("COVERAGE.md:%d: the excerpt must contain exactly one MUST or REQUIRED, it has %d: %q", r.line, len(inExcerpt), r.excerpt)
			continue
		}
		at := strings.Index(sec.text, r.excerpt)
		if at < 0 {
			t.Errorf("COVERAGE.md:%d: the excerpt no longer appears in section %s: %q", r.line, r.section, r.excerpt)
			continue
		}
		if strings.Contains(sec.text[at+1:], r.excerpt) {
			t.Errorf("COVERAGE.md:%d: the excerpt appears more than once in section %s; quote enough to make it unique: %q", r.line, r.section, r.excerpt)
			continue
		}
		clauseAt := at + inExcerpt[0][0]
		if !sec.clauses[clauseAt] {
			t.Errorf("COVERAGE.md:%d: the excerpt's keyword is not a clause keyword where it sits in section %s: %q", r.line, r.section, r.excerpt)
			continue
		}
		if pinned[r.section] == nil {
			pinned[r.section] = map[int]int{}
		}
		if other, dup := pinned[r.section][clauseAt]; dup {
			t.Errorf("COVERAGE.md:%d: accounts for the same clause as line %d", r.line, other)
			continue
		}
		pinned[r.section][clauseAt] = r.line

		wantIDs, known := statuses[r.status]
		switch {
		case !known:
			t.Errorf("COVERAGE.md:%d: status %q is not one of graded, partial, ungraded, n/a", r.line, r.status)
		case wantIDs && len(r.checks) == 0:
			t.Errorf("COVERAGE.md:%d: a %s clause names the checks that grade it", r.line, r.status)
		case !wantIDs && len(r.checks) > 0:
			t.Errorf("COVERAGE.md:%d: a %s clause names no checks (\"-\")", r.line, r.status)
		}
		if (r.status == "partial" || r.status == "n/a") && r.notes == "" {
			t.Errorf("COVERAGE.md:%d: a %s clause says why in its notes", r.line, r.status)
		}
		if !binds[r.binds] {
			t.Errorf("COVERAGE.md:%d: binds %q is not one of hub, plugin, either, sender, admin client, receiver", r.line, r.binds)
		}
		for _, id := range r.checks {
			if !ids[id] {
				t.Errorf("COVERAGE.md:%d: %s is not a check in either suite (IDs are written hub:<id> or plugin:<id>)", r.line, id)
			}
		}
	}

	// Every clause in the spec has its row.
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		text := sections[name].text
		for _, loc := range keyword.FindAllStringIndex(text, -1) {
			if _, ok := pinned[name][loc[0]]; ok {
				continue
			}
			from, to := max(0, loc[0]-80), min(len(text), loc[1]+80)
			t.Errorf("section %s: no row accounts for the clause %q", name, "..."+text[from:to]+"...")
		}
	}

	// The summary states what the rows add up to, once per status, and the
	// total line states the sum.
	counts := map[string]int{}
	for _, r := range tbl.rows {
		counts[r.status]++
	}
	for status := range statuses {
		got := tbl.summary[status]
		if len(got) != 1 || got[0] != strconv.Itoa(counts[status]) {
			t.Errorf("COVERAGE.md summary: want exactly one %s row reading %d, got %v", status, counts[status], got)
		}
	}
	if len(tbl.totals) != 1 || tbl.totals[0] != strconv.Itoa(len(tbl.rows)) {
		t.Errorf("COVERAGE.md summary: want exactly one line reading %q, got %v", strconv.Itoa(len(tbl.rows))+" clauses in all.", tbl.totals)
	}
	t.Logf("%d clauses: %d graded, %d partial, %d ungraded, %d n/a",
		len(tbl.rows), counts["graded"], counts["partial"], counts["ungraded"], counts["n/a"])
}
