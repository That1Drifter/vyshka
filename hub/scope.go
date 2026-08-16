package hub

import (
	"fmt"
	"strings"
)

// The scope grammar of spec section 10. A scope is `resource:verb`, optionally
// narrowed by a third `:pattern` field:
//
//	servers:read
//	events:read                      every event type
//	events:read:example-mod.*        one namespace
//	actions:read:example-mod.heal    one action code
//	actions:dispatch                 every action (dangerous)
//	actions:dispatch:core.player.*   one namespace of action codes
//	kv:rw:example-mod
//	webhooks:manage
//	admin                            everything, including token management
//
// Resources and verbs are a closed set. Refusing an unknown scope at mint time
// is what stops a typo from becoming a token that silently grants nothing: an
// operator who writes `event:read` gets an error, not a token that fails every
// request it is later used for.
const (
	resourceServers  = "servers"
	resourceEvents   = "events"
	resourceActions  = "actions"
	resourceKV       = "kv"
	resourceWebhooks = "webhooks"
	resourceAdmin    = "admin"

	verbRead     = "read"
	verbDispatch = "dispatch"
	verbRW       = "rw"
	verbManage   = "manage"
)

// Scope limits. A token carrying hundreds of grants would make every request
// pay for them, and the pattern length matches the identifiers scopes name.
const (
	maxScopesPerToken = 32
	maxScopePattern   = 128
)

// Scope is one grant on an admin token. Pattern is empty when the grant is not
// narrowed, which is equivalent to "*" for matching and is kept distinct only
// so that a token round-trips through the API as the operator wrote it.
type Scope struct {
	Resource string
	Verb     string
	Pattern  string
}

// scopeKind describes one legal resource:verb pair.
type scopeKind struct {
	resource string
	verb     string
	// patterned reports whether this pair accepts a narrowing third field.
	patterned bool
}

// scopeKinds is the closed set of grants this draft defines.
//
// `actions:read` is not in the section 10 example list of earlier drafts; it
// arrived with this slice. Without it, reading an action's lifecycle would have
// to be gated on `actions:dispatch`, which would mean a monitoring token could
// only watch jobs it was also allowed to start. Least privilege has to work in
// both directions or it is decoration.
var scopeKinds = []scopeKind{
	{resource: resourceServers, verb: verbRead},
	{resource: resourceEvents, verb: verbRead, patterned: true},
	{resource: resourceActions, verb: verbRead, patterned: true},
	{resource: resourceActions, verb: verbDispatch, patterned: true},
	{resource: resourceKV, verb: verbRW, patterned: true},
	{resource: resourceWebhooks, verb: verbManage},
	{resource: resourceAdmin, verb: ""},
}

// ParseScope reads one scope string. It is strict: an unrecognized resource,
// verb, or pattern is an error rather than a grant that matches nothing.
func ParseScope(value string) (Scope, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return Scope{}, fmt.Errorf("a scope cannot be empty")
	}

	parts := strings.Split(text, ":")
	if len(parts) > 3 {
		return Scope{}, fmt.Errorf("scope %q has more than the three resource:verb:pattern fields", text)
	}

	scope := Scope{Resource: parts[0]}
	if len(parts) > 1 {
		scope.Verb = parts[1]
	}
	if len(parts) > 2 {
		scope.Pattern = parts[2]
	}

	kind, ok := lookupScopeKind(scope.Resource, scope.Verb)
	if !ok {
		return Scope{}, fmt.Errorf("scope %q is not one this hub defines; see spec section 10", text)
	}
	if scope.Pattern == "" {
		if len(parts) > 2 {
			// `events:read:` is a narrowing the operator meant to write and did
			// not. Reading it as the unnarrowed grant would hand out more than
			// they asked for, which is the one direction an authorization bug
			// must never fail in.
			return Scope{}, fmt.Errorf("scope %q ends in an empty pattern; drop the trailing colon to grant every value", text)
		}
		return scope, nil
	}
	if !kind.patterned {
		return Scope{}, fmt.Errorf("scope %s:%s does not take a :pattern", scope.Resource, scope.Verb)
	}
	if !validScopePattern(scope.Pattern) {
		return Scope{}, fmt.Errorf("scope pattern %q must be *, a {namespace}.* prefix, or an exact identifier of at most %d characters drawn from letters, digits, _, -, and .",
			truncateUTF8(scope.Pattern, 64), maxScopePattern)
	}
	return scope, nil
}

func lookupScopeKind(resource, verb string) (scopeKind, bool) {
	for _, kind := range scopeKinds {
		if kind.resource == resource && kind.verb == verb {
			return kind, true
		}
	}
	return scopeKind{}, false
}

// String renders the canonical form, which is what is stored and returned.
func (s Scope) String() string {
	text := s.Resource
	if s.Verb != "" {
		text += ":" + s.Verb
	}
	if s.Pattern != "" {
		text += ":" + s.Pattern
	}
	return text
}

// validScopePattern accepts `*`, a `{prefix}.*` namespace pattern, or an exact
// identifier. The alphabet is the one the event type grammar of section 8.1
// allows, which is also what action codes and KV namespaces are drawn from, so
// nothing an operator writes in a scope can reach a query as a character the
// values it matches could never contain.
func validScopePattern(pattern string) bool {
	if pattern == "*" {
		return true
	}
	if len(pattern) > maxScopePattern {
		return false
	}
	literal, isPrefix := strings.CutSuffix(pattern, ".*")
	if isPrefix && literal == "" {
		return false
	}
	if !isPrefix && strings.Contains(pattern, "*") {
		// A `*` anywhere but as the whole pattern or a trailing `.*` would be a
		// glob this hub does not implement. Accepting it as a literal would
		// silently grant nothing.
		return false
	}
	for _, segment := range strings.Split(literal, ".") {
		if segment == "" {
			return false
		}
		for i := range len(segment) {
			c := segment[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
				c == '_', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// matches reports whether a grant's pattern covers one concrete value: an event
// type, an action code, or a KV namespace.
func (s Scope) matches(value string) bool {
	switch {
	case s.Pattern == "" || s.Pattern == "*":
		return true
	case strings.HasSuffix(s.Pattern, ".*"):
		// The prefix keeps its separating dot, so `example-mod.*` matches
		// example-mod.heal and not example-modular.heal.
		return strings.HasPrefix(value, strings.TrimSuffix(s.Pattern, "*"))
	default:
		return s.Pattern == value
	}
}

// covers reports whether a grant covers everything a requested pattern could
// match. It is the question an explicit query filter asks, where matches asks
// the question one concrete value asks.
//
// The asymmetry matters: a token granted `example-mod.heal` matches that one
// code, but does not cover a request for `example-mod.*`, because that request
// spans codes the grant says nothing about.
func (s Scope) covers(pattern string) bool {
	if s.Pattern == "" || s.Pattern == "*" {
		return true
	}
	requestedPrefix, isPrefix := strings.CutSuffix(pattern, ".*")
	if !isPrefix {
		if pattern == "*" {
			// Only an unnarrowed grant covers the catch-all, and that was
			// handled above.
			return false
		}
		return s.matches(pattern)
	}
	grantPrefix, grantIsPrefix := strings.CutSuffix(s.Pattern, ".*")
	if !grantIsPrefix {
		// An exact grant covers one value; a `{namespace}.*` request spans an
		// open set, so nothing exact can cover it.
		return false
	}
	// `core.*` covers `core.player.*`, not the other way round.
	return strings.HasPrefix(requestedPrefix+".", grantPrefix+".")
}

// principal is the authenticated caller of an Admin API request.
type principal struct {
	// TokenID is empty for the bootstrap credential, which lives in
	// configuration rather than in the tokens table.
	TokenID string
	Name    string
	Scopes  []Scope
}

// isAdmin reports whether the caller holds the superuser scope. `admin` implies
// every other scope: a hub whose admin token could not list servers would be a
// puzzle, not a safeguard, and section 10 already assigns it the two operations
// (token management, server enrollment) that can grant everything else anyway.
func (p *principal) isAdmin() bool {
	for _, scope := range p.Scopes {
		if scope.Resource == resourceAdmin {
			return true
		}
	}
	return false
}

// allowsAny reports whether the caller holds any grant on a resource and verb,
// whatever its pattern. It is the route-level gate: a request that fails it can
// be refused before its body is read, and one that passes it still has to clear
// the value-level check inside the handler.
func (p *principal) allowsAny(resource, verb string) bool {
	if p.isAdmin() {
		return true
	}
	for _, scope := range p.Scopes {
		if scope.Resource == resource && scope.Verb == verb {
			return true
		}
	}
	// Dispatching an action implies reading it. Without this a narrowly scoped
	// dispatcher could start a job and then be unable to find out what happened
	// to it, which would make every such token useless on its own.
	if resource == resourceActions && verb == verbRead {
		return p.allowsAny(resourceActions, verbDispatch)
	}
	return false
}

// allows reports whether the caller may act on one concrete value.
func (p *principal) allows(resource, verb, value string) bool {
	if p.isAdmin() {
		return true
	}
	for _, scope := range p.Scopes {
		if scope.Resource == resource && scope.Verb == verb && scope.matches(value) {
			return true
		}
	}
	if resource == resourceActions && verb == verbRead {
		return p.allows(resourceActions, verbDispatch, value)
	}
	return false
}

// covers reports whether the caller's grants cover an entire requested pattern.
func (p *principal) covers(resource, verb, pattern string) bool {
	if p.isAdmin() {
		return true
	}
	for _, scope := range p.Scopes {
		if scope.Resource == resource && scope.Verb == verb && scope.covers(pattern) {
			return true
		}
	}
	if resource == resourceActions && verb == verbRead {
		return p.covers(resourceActions, verbDispatch, pattern)
	}
	return false
}

// patternsFor lists the narrowings the caller holds on a resource and verb, for
// the one case where a grant does not gate a request but shapes it: a feed read
// with no explicit filter is narrowed to what the token may see, rather than
// refused. An empty result means "everything", which is why callers must check
// allowsAny first.
func (p *principal) patternsFor(resource, verb string) []string {
	if p.isAdmin() {
		return nil
	}
	patterns := make([]string, 0, len(p.Scopes))
	for _, scope := range p.Scopes {
		if scope.Resource != resource || scope.Verb != verb {
			continue
		}
		if scope.Pattern == "" || scope.Pattern == "*" {
			return nil
		}
		patterns = append(patterns, scope.Pattern)
	}
	return patterns
}
