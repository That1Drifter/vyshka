package hub

import "testing"

func TestParseScopeAcceptsTheGrammar(t *testing.T) {
	t.Parallel()

	for _, text := range []string{
		"admin",
		"servers:read",
		"events:read",
		"events:read:*",
		"events:read:core.player.*",
		"events:read:core.player.death",
		"actions:read:example-mod.*",
		"actions:dispatch",
		"actions:dispatch:example-mod.heal",
		"kv:rw:example-mod",
		"webhooks:manage",
	} {
		scope, err := ParseScope(text)
		if err != nil {
			t.Errorf("ParseScope(%q) = error %v, want it accepted", text, err)
			continue
		}
		if scope.String() != text {
			t.Errorf("ParseScope(%q).String() = %q; the canonical form must round-trip",
				text, scope.String())
		}
	}
}

func TestParseScopeRefusesWhatItDoesNotDefine(t *testing.T) {
	t.Parallel()

	for _, one := range []struct {
		text string
		why  string
	}{
		{"", "an empty scope"},
		{"  ", "whitespace"},
		{"event:read", "a misspelled resource, which must not become a grant that matches nothing"},
		{"servers:write", "a verb this hub does not define"},
		{"servers:read:s-1234", "a pattern on a pair that takes none"},
		{"admin:read", "a verb on the superuser scope"},
		{"events:read:", "a trailing colon, which is a narrowing the operator meant to write"},
		{"events:read:a:b", "a fourth field"},
		{"events:read:core.*.death", "a wildcard in the middle, which is a glob this hub does not implement"},
		{"events:read:*.death", "a leading wildcard"},
		{"events:read:core..death", "an empty segment"},
		{"events:read:core player", "a space in a pattern"},
		{"events:read:core.player%", "a character outside the identifier alphabet"},
	} {
		if scope, err := ParseScope(one.text); err == nil {
			t.Errorf("ParseScope(%q) = %v, want an error: %s", one.text, scope, one.why)
		}
	}
}

func TestParseScopeRefusesAnOverlongPattern(t *testing.T) {
	t.Parallel()

	long := make([]byte, maxScopePattern+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := ParseScope("events:read:" + string(long)); err == nil {
		t.Error("a pattern past the length cap was accepted")
	}
}

func TestScopeMatchesConcreteValues(t *testing.T) {
	t.Parallel()

	for _, one := range []struct {
		pattern string
		value   string
		want    bool
	}{
		{"", "anything.at.all", true},
		{"*", "anything.at.all", true},
		{"example-mod.heal", "example-mod.heal", true},
		{"example-mod.heal", "example-mod.heal2", false},
		{"example-mod.*", "example-mod.heal", true},
		{"example-mod.*", "example-mod.raid.started", true},
		// The separating dot is part of the prefix, so a longer namespace that
		// merely starts with the same letters is not covered.
		{"example-mod.*", "example-modular.heal", false},
		{"core.player.*", "core.player.death", true},
		{"core.player.*", "core.server.fps", false},
		{"core.*", "core.player.death", true},
	} {
		scope := Scope{Resource: resourceEvents, Verb: verbRead, Pattern: one.pattern}
		if got := scope.matches(one.value); got != one.want {
			t.Errorf("Scope{Pattern:%q}.matches(%q) = %v, want %v",
				one.pattern, one.value, got, one.want)
		}
	}
}

// covers is the question an explicit query filter asks, and it is not the same
// question matches asks. A grant on one value cannot cover a request spanning a
// whole namespace, even though it matches a value inside it.
func TestScopeCoversWholePatterns(t *testing.T) {
	t.Parallel()

	for _, one := range []struct {
		grant     string
		requested string
		want      bool
	}{
		{"", "core.player.*", true},
		{"*", "*", true},
		{"core.*", "*", false},
		{"core.*", "core.player.*", true},
		{"core.*", "core.player.death", true},
		{"core.player.*", "core.*", false},
		{"core.player.*", "core.player.*", true},
		{"core.player.death", "core.player.death", true},
		{"core.player.death", "core.player.*", false},
		{"core.*", "coreX.player.*", false},
	} {
		scope := Scope{Resource: resourceEvents, Verb: verbRead, Pattern: one.grant}
		if got := scope.covers(one.requested); got != one.want {
			t.Errorf("Scope{Pattern:%q}.covers(%q) = %v, want %v",
				one.grant, one.requested, got, one.want)
		}
	}
}

func TestPrincipalAdminImpliesEverything(t *testing.T) {
	t.Parallel()

	admin := &principal{Name: "root", Scopes: []Scope{{Resource: resourceAdmin}}}
	if !admin.allowsAny(resourceEvents, verbRead) {
		t.Error("admin does not carry events:read")
	}
	if !admin.allows(resourceActions, verbDispatch, "example-mod.wipe") {
		t.Error("admin cannot dispatch an arbitrary action")
	}
	if !admin.covers(resourceEvents, verbRead, "*") {
		t.Error("admin does not cover the catch-all event filter")
	}
	if patterns := admin.patternsFor(resourceEvents, verbRead); len(patterns) != 0 {
		t.Errorf("admin is narrowed to %v, want no narrowing at all", patterns)
	}
}

func TestPrincipalDispatchImpliesRead(t *testing.T) {
	t.Parallel()

	// A token that can start a job but could not then find out what happened to
	// it would be useless on its own, so dispatch implies read over the same
	// pattern. The implication does not run the other way.
	dispatcher := &principal{Name: "narrow", Scopes: []Scope{
		{Resource: resourceActions, Verb: verbDispatch, Pattern: "example-mod.heal"},
	}}
	if !dispatcher.allows(resourceActions, verbRead, "example-mod.heal") {
		t.Error("a dispatcher cannot read the action it dispatched")
	}
	if dispatcher.allows(resourceActions, verbRead, "example-mod.wipe") {
		t.Error("a dispatcher can read an action outside its pattern")
	}

	reader := &principal{Name: "watcher", Scopes: []Scope{
		{Resource: resourceActions, Verb: verbRead, Pattern: "example-mod.heal"},
	}}
	if reader.allows(resourceActions, verbDispatch, "example-mod.heal") {
		t.Error("a reader can dispatch, which inverts the implication")
	}
	if !reader.allowsAny(resourceActions, verbRead) {
		t.Error("a reader fails its own route gate")
	}
	if reader.allowsAny(resourceActions, verbDispatch) {
		t.Error("a reader passes the dispatch route gate")
	}
}

func TestPrincipalWithoutGrantsFailsClosed(t *testing.T) {
	t.Parallel()

	nobody := &principal{}
	for _, one := range []struct {
		resource string
		verb     string
	}{
		{resourceServers, verbRead},
		{resourceEvents, verbRead},
		{resourceActions, verbRead},
		{resourceActions, verbDispatch},
		{resourceAdmin, ""},
	} {
		if nobody.allowsAny(one.resource, one.verb) {
			t.Errorf("a principal with no scopes passes the %s:%s gate", one.resource, one.verb)
		}
	}
	if nobody.isAdmin() {
		t.Error("a principal with no scopes is admin")
	}
}

func TestPrincipalPatternsForListsNarrowings(t *testing.T) {
	t.Parallel()

	narrowed := &principal{Scopes: []Scope{
		{Resource: resourceEvents, Verb: verbRead, Pattern: "core.player.*"},
		{Resource: resourceEvents, Verb: verbRead, Pattern: "example-mod.raid.started"},
		{Resource: resourceServers, Verb: verbRead},
	}}
	patterns := narrowed.patternsFor(resourceEvents, verbRead)
	if len(patterns) != 2 {
		t.Fatalf("patternsFor = %v, want the two events:read narrowings", patterns)
	}

	// An unnarrowed grant anywhere in the set means the whole resource, which
	// callers read as the empty result.
	wide := &principal{Scopes: []Scope{
		{Resource: resourceEvents, Verb: verbRead, Pattern: "core.player.*"},
		{Resource: resourceEvents, Verb: verbRead},
	}}
	if patterns := wide.patternsFor(resourceEvents, verbRead); len(patterns) != 0 {
		t.Errorf("patternsFor = %v, want no narrowing when one grant is unnarrowed", patterns)
	}
}
