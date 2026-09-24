package webhook

import (
	"strings"

	"github.com/That1Drifter/vyshka/hub/internal/pattern"
	"github.com/That1Drifter/vyshka/hub/store"
)

// ReservedNamespace reports whether a type, or a pattern, lies in a namespace
// the hub's own notifications own (spec section 8.1): action, server, and
// audit. Telemetry may not use them, which is what keeps a plugin from
// speaking in the hub's voice to every webhook receiver.
func ReservedNamespace(value string) bool {
	return strings.HasPrefix(value, "action.") || strings.HasPrefix(value, "server.") ||
		strings.HasPrefix(value, "audit.")
}

// OptIn reports whether a notification type is matched only by a pattern that
// names its namespace, never by the catch-all (spec section 11.1). The audit
// log is admin-only, and a subscription to everything registered before the
// notification existed must not start exporting it.
func OptIn(notificationType string) bool {
	return strings.HasPrefix(notificationType, "audit.")
}

// PatternAdmits reports whether one webhook filter pattern matches one
// notification type, with the opt-in rule applied.
func PatternAdmits(filter, notificationType string) bool {
	if filter == "*" && OptIn(notificationType) {
		return false
	}
	return pattern.Match(filter, notificationType)
}

// FilterAdmitsAudit reports whether a webhook filter matches the opt-in audit
// notification. An empty filter is the catch-all, which never does.
func FilterAdmitsAudit(events []string) bool {
	for _, filter := range events {
		if PatternAdmits(filter, AuditRecorded) {
			return true
		}
	}
	return false
}

// SubscribesTelemetry reports whether a pattern can match any telemetry type.
// The section 8.1 reservation is what makes this decidable: a pattern
// confined to the reserved namespaces can only ever match the hub's own
// notifications.
func SubscribesTelemetry(filter string) bool {
	return filter == "*" || !ReservedNamespace(filter)
}

// webhookMatches decides whether one notification concerns one webhook: the
// server filter is exact membership, the event filter the section 10.1 pattern
// grammar, and empty filters mean everything (spec section 11.2).
func webhookMatches(webhook store.Webhook, notificationType, serverID string) bool {
	if len(webhook.ServerIDs) > 0 {
		observed := false
		for _, candidate := range webhook.ServerIDs {
			if candidate == serverID {
				observed = true
				break
			}
		}
		if !observed {
			return false
		}
	}
	if OptIn(notificationType) && !webhook.AuditGranted {
		// A filter naming the audit namespace that no token able to read the
		// audit log ever authorized, one registered while the namespace was
		// ordinary telemetry above all, is not a request for the access
		// record (spec section 11.1).
		return false
	}
	if len(webhook.Events) == 0 {
		// An empty filter is the catch-all, and the catch-all never admits an
		// opt-in notification (spec section 11.1).
		return !OptIn(notificationType)
	}
	for _, filter := range webhook.Events {
		if PatternAdmits(filter, notificationType) {
			return true
		}
	}
	return false
}
