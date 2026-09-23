package hub

import (
	"bytes"
	"encoding/json"
	"strings"
)

// redactData applies a webhook's redaction paths (spec section 11.2) to one
// notification's data and returns the data to render. A path is member names
// joined by dots: the first selects a member of data, each further name a
// member of what is selected so far, and the member the last name selects is
// removed. Where a step selects an array, the rest of the path applies to
// every element that is an object. A path that selects nothing strips
// nothing.
//
// Only the levels a path reaches are re-encoded, a level being reached when
// it carries the member the path names there; everything else stays the
// plugin's own bytes, so a number above 2^53 or a member order the receiver
// relies on survives everywhere a path did not reach. Data no path reaches
// is returned as it came.
func redactData(data json.RawMessage, paths []string) json.RawMessage {
	for _, path := range paths {
		if redacted, changed := redactValue(data, strings.Split(path, ".")); changed {
			data = redacted
		}
	}
	return data
}

// redactValue removes the member path names from raw, the value the path so
// far has selected, and reports whether anything was removed. path is never
// empty.
func redactValue(raw json.RawMessage, path []string) (json.RawMessage, bool) {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 {
		return raw, false
	}
	switch trimmed[0] {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return raw, false
		}
		child, present := object[path[0]]
		if !present {
			// The decoded map holds every member name the object carries,
			// duplicates collapsed, so absent here is absent everywhere.
			return raw, false
		}
		// From here the level is re-encoded from the map whether or not the
		// path went on to remove anything. JSON lets an object repeat a
		// member, and the map keeps only the last: returning the original
		// bytes because the last copy held nothing to strip would send the
		// earlier copies out whole. Re-encoding keeps one copy, the one the
		// path was applied to.
		if len(path) == 1 {
			delete(object, path[0])
			return encodeRedacted(object)
		}
		replaced, _ := redactValue(child, path[1:])
		object[path[0]] = replaced
		return encodeRedacted(object)
	case '[':
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return raw, false
		}
		changed := false
		for i, element := range elements {
			// The rest of the path applies to each element that is an object;
			// anything else in the array has no members to strip.
			if first := bytes.TrimLeft(element, " \t\r\n"); len(first) == 0 || first[0] != '{' {
				continue
			}
			if replaced, did := redactValue(element, path); did {
				elements[i] = replaced
				changed = true
			}
		}
		if !changed {
			return raw, false
		}
		return encodeRedacted(elements)
	}
	return raw, false
}

// encodeRedacted re-encodes one changed level without HTML escaping, so the
// bytes that were not redacted read as the plugin wrote them. It fails
// closed: an encoding failure, which members decoded a moment ago cannot
// produce, replaces the level with null rather than let the unredacted
// original leave the hub.
func encodeRedacted(value any) (json.RawMessage, bool) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return json.RawMessage(`null`), true
	}
	return json.RawMessage(bytes.TrimRight(buffer.Bytes(), "\n")), true
}
