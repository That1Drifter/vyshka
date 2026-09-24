// Package pattern holds the matching half of the spec section 10.1 pattern
// grammar, shared by the scopes that grant access and the webhook filters that
// subscribe to notifications, so the two can never drift into different
// readings of the same text. Validation stays with the callers, which answer a
// refused pattern in their own words.
package pattern

import "strings"

// Match reports whether a pattern covers one concrete value: an event type, an
// action code, a KV namespace, or a notification type. An empty pattern and
// `*` match everything, `{prefix}.*` matches the namespace, and anything else
// matches only itself.
func Match(pattern, value string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, ".*"):
		// The prefix keeps its separating dot, so `example-mod.*` matches
		// example-mod.heal and not example-modular.heal.
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == value
	}
}
