// Package coverage holds one test: that conformance/COVERAGE.md still
// accounts for every normative clause in spec/protocol.md, clause for clause,
// and that every check it names still exists in one of the two suites.
//
// A protocol edit that adds, removes, or rewords a MUST, MUST NOT, or
// REQUIRED clause fails this test until the table is brought along, which is
// the point: the table is only worth reading if it cannot quietly go stale.
// Like everything under conformance/, this reads the spec and the suites as
// text and imports nothing from the hub or a plugin.
package coverage

import (
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

// heading matches a numbered section heading ("### 3.1.2 The poll exchange",
// "## 5. Enrollment and sessions") and captures the number without a
// trailing dot.
var heading = regexp.MustCompile(`^#{2,6} (\d+(?:\.\d+)*)\.? `)

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

// checkID matches the ID of a hub check or a plugin stage as the suites
// declare it. Every ID in both suites is dotted, which keeps envelope and
// ban IDs in fixtures out of the set.
var checkID = regexp.MustCompile(`\bID:\s+"([a-z][A-Za-z0-9_-]*(?:\.[A-Za-z0-9_-]+)+)"`)

var spaces = regexp.MustCompile(`\s+`)

func normalize(s string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// specSections returns the whitespace-normalized text of every numbered,
// normative section, keyed by section number. Fenced code blocks are left
// out, and so is everything from the first appendix on (informative).
func specSections(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	sections := map[string]string{}
	var current string
	var fenced bool
	var buf []string
	flush := func() {
		if current != "" && !excluded[current] {
			sections[current] = normalize(strings.Join(buf, " "))
		}
		buf = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if strings.HasPrefix(line, "## Appendix") {
			flush()
			current = ""
			break
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			flush()
			current = m[1]
			continue
		}
		if strings.HasPrefix(line, "#") {
			// An unnumbered heading (the document title) ends the section
			// before it and starts front matter, which is not normative.
			flush()
			current = ""
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return sections
}

type row struct {
	line     int
	section  string
	excerpt  string
	binds    string
	checks   []string
	status   string
	notes    string
	clauseAt int // offset of the pinned keyword in the section text
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

var sectionCell = regexp.MustCompile(`^\d+(?:\.\d+)*$`)

// tableRows parses every clause row of the coverage table: a row whose first
// cell is a section number.
func tableRows(t *testing.T) []row {
	t.Helper()
	raw, err := os.ReadFile(tablePath)
	if err != nil {
		t.Fatal(err)
	}
	var rows []row
	for i, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := splitCells(line)
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
		rows = append(rows, r)
	}
	return rows
}

// suiteIDs returns every check ID the two suites declare, as "hub:<id>" and
// "plugin:<id>".
func suiteIDs(t *testing.T) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, suite := range []string{"hub", "plugin"} {
		files, err := filepath.Glob(filepath.Join("..", suite, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range checkID.FindAllStringSubmatch(string(raw), -1) {
				ids[suite+":"+m[1]] = true
			}
		}
	}
	if len(ids) == 0 {
		t.Fatal("found no check IDs in conformance/hub or conformance/plugin; the ID pattern no longer matches how the suites declare checks")
	}
	return ids
}

func TestCoverageTableMatchesSpec(t *testing.T) {
	sections := specSections(t)
	rows := tableRows(t)
	ids := suiteIDs(t)

	// Pin every row to the one clause it accounts for.
	pinned := map[string]map[int]int{} // section -> clause offset -> row line
	for i := range rows {
		r := &rows[i]
		text, ok := sections[r.section]
		if !ok {
			t.Errorf("COVERAGE.md:%d: section %s is not a normative section of the spec", r.line, r.section)
			continue
		}
		if n := len(keyword.FindAllStringIndex(r.excerpt, -1)); n != 1 {
			t.Errorf("COVERAGE.md:%d: the excerpt must contain exactly one MUST or REQUIRED, it has %d: %q", r.line, n, r.excerpt)
			continue
		}
		at := strings.Index(text, r.excerpt)
		if at < 0 {
			t.Errorf("COVERAGE.md:%d: the excerpt no longer appears in section %s: %q", r.line, r.section, r.excerpt)
			continue
		}
		if strings.Contains(text[at+1:], r.excerpt) {
			t.Errorf("COVERAGE.md:%d: the excerpt appears more than once in section %s; quote enough to make it unique: %q", r.line, r.section, r.excerpt)
			continue
		}
		r.clauseAt = at + keyword.FindStringIndex(r.excerpt)[0]
		if pinned[r.section] == nil {
			pinned[r.section] = map[int]int{}
		}
		if other, dup := pinned[r.section][r.clauseAt]; dup {
			t.Errorf("COVERAGE.md:%d: accounts for the same clause as line %d", r.line, other)
			continue
		}
		pinned[r.section][r.clauseAt] = r.line

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
		text := sections[name]
		for _, loc := range keyword.FindAllStringIndex(text, -1) {
			if _, ok := pinned[name][loc[0]]; ok {
				continue
			}
			from, to := max(0, loc[0]-80), min(len(text), loc[1]+80)
			t.Errorf("section %s: no row accounts for the clause %q", name, "..."+text[from:to]+"...")
		}
	}

	// The summary at the top of the table states what the rows add up to.
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.status]++
	}
	raw, err := os.ReadFile(tablePath)
	if err != nil {
		t.Fatal(err)
	}
	for status := range statuses {
		want := "| " + status + " | "
		line := ""
		for _, l := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
			if strings.HasPrefix(l, want) {
				line = l
				break
			}
		}
		cells := splitCells(line)
		if line == "" || len(cells) < 2 || cells[1] != strconv.Itoa(counts[status]) {
			t.Errorf("COVERAGE.md summary: the %s row must read %d", status, counts[status])
		}
	}
	t.Logf("%d clauses: %d graded, %d partial, %d ungraded, %d n/a",
		len(rows), counts["graded"], counts["partial"], counts["ungraded"], counts["n/a"])
}
