package main

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"unicode/utf8"
)

// The harness dispatches whatever action the candidate's manifest declares, so
// it has to invent params from the action's own schema. The schema language is
// the section 6.1 subset: object/array/scalar types, enum, not in its one
// {"enum": [...]} form, required, numeric bounds, default. Apart from the
// subset check below, nothing here validates a schema; it builds one instance that
// satisfies it and, for the crash check, one that does not.

// synthesizeParams builds a params object satisfying the schema. A nil schema
// constrains nothing, so an empty object satisfies it.
func synthesizeParams(schema map[string]any) map[string]any {
	if object, ok := synthesizeValue(schema).(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

func synthesizeValue(schema map[string]any) any {
	if schema == nil {
		return map[string]any{}
	}
	// A default is annotation-only (the companion schema is explicit: never
	// enforced), so a declared default may violate its own constraints. Use it
	// only when it actually satisfies them.
	if value, ok := schema["default"]; ok && satisfies(schema, value) {
		return value
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		// The first member the whole schema admits: an exclusion or a
		// property schema can refuse a member the enum lists.
		for _, member := range enum {
			if satisfies(schema, member) {
				return member
			}
		}
		return enum[0]
	}
	schemaType, _ := schema["type"].(string)
	if schemaType == "" {
		if _, ok := schema["properties"]; ok {
			schemaType = "object"
		}
	}
	switch schemaType {
	case "object", "":
		out := map[string]any{}
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, name := range required {
			key, ok := name.(string)
			if !ok {
				continue
			}
			// A name required twice is synthesized once: nested, the
			// repeats would multiply the work level by level.
			if _, done := out[key]; done {
				continue
			}
			property, _ := properties[key].(map[string]any)
			out[key] = synthesizeValue(property)
		}
		// An object the schema excludes gets an undeclared member more at a
		// time (the subset has no additionalProperties, so any is allowed);
		// a name the schema declares is passed over.
		// The bound is fixed before the loop: names skipped as declared or
		// present, plus one per excluded object, plus one.
		attempts := exclusionCount(schema) + len(properties) + len(out) + 1
		for extra := 1; !satisfies(schema, out) && extra <= attempts; extra++ {
			key := "conformance-" + strconv.Itoa(extra)
			if _, declared := properties[key]; declared {
				continue
			}
			if _, present := out[key]; present {
				continue
			}
			grown := map[string]any{key: true}
			for existing, member := range out {
				grown[existing] = member
			}
			out = grown
		}
		// A schema with no type admits any kind of value its keywords do
		// not refuse, so when no object will do, another kind may.
		// Each other kind is synthesized as if the schema declared it, so
		// its own search steps past whatever the exclusion lists.
		if _, typed := schema["type"]; !typed && !satisfies(schema, out) {
			for _, kind := range []string{"number", "string", "boolean", "null", "array"} {
				declared := map[string]any{"type": kind}
				for keyword, value := range schema {
					if keyword != "type" {
						declared[keyword] = value
					}
				}
				if other := synthesizeValue(declared); satisfies(schema, other) {
					return other
				}
			}
		}
		return out
	case "array":
		// The shortest array the schema admits: empty, then one item more at
		// a time, past however many arrays the schema excludes.
		// The item is synthesized once, and only when the empty array will
		// not do; one its own schema refuses cannot make a longer array
		// valid, so the search stops there rather than growing.
		items, _ := schema["items"].(map[string]any)
		candidate := []any{}
		if satisfies(schema, candidate) {
			return candidate
		}
		item := synthesizeValue(items)
		if items != nil && !satisfies(items, item) {
			return candidate
		}
		for length := 1; length <= exclusionCount(schema)+1; length++ {
			candidate = append(append([]any{}, candidate...), item)
			if satisfies(schema, candidate) {
				return candidate
			}
		}
		return []any{}
	case "string":
		// A value the schema's `not` excludes (section 6.1) is stepped past;
		// the list is finite, so one more candidate than it has members
		// always finds a free one.
		value := "conformance"
		for i := 1; excluded(schema, value) && i <= exclusionCount(schema)+1; i++ {
			value = "conformance-" + strconv.Itoa(i)
		}
		return value
	case "boolean":
		return !excluded(schema, true)
	case "null":
		return nil
	case "integer", "number":
		return steppedNumber(schema, synthesizeNumber(schema, schemaType))
	default:
		return map[string]any{}
	}
}

func synthesizeNumber(schema map[string]any, schemaType string) any {
	minimum, hasMinimum := asFloat(schema["minimum"])
	if exclusive, ok := asFloat(schema["exclusiveMinimum"]); ok && (!hasMinimum || exclusive+1 > minimum) {
		minimum, hasMinimum = exclusive+1, true
	}
	maximum, hasMaximum := asFloat(schema["maximum"])
	if exclusive, ok := asFloat(schema["exclusiveMaximum"]); ok && (!hasMaximum || exclusive-1 < maximum) {
		maximum, hasMaximum = exclusive-1, true
	}
	value := 1.0
	if hasMinimum {
		value = minimum
	} else if hasMaximum && maximum < value {
		value = maximum
	}
	if schemaType == "integer" {
		integer := int64(math.Ceil(value))
		if hasMaximum && float64(integer) > maximum {
			integer = int64(math.Floor(maximum))
		}
		return integer
	}
	return value
}

// steppedNumber finds a number the whole schema admits, starting from the
// synthesized one: the start, the bounds, ever finer points between them,
// then whole steps up and down. The first candidate is returned when nothing
// in reach is admitted.
func steppedNumber(schema map[string]any, first any) any {
	_, integer := first.(int64)
	start, _ := normalize(first).(float64)
	found := first
	// try reports whether a candidate satisfies the schema, keeping it.
	try := func(candidate float64) bool {
		if integer {
			candidate = math.Ceil(candidate)
		}
		if !satisfies(schema, candidate) {
			return false
		}
		if integer {
			found = int64(candidate)
		} else {
			found = candidate
		}
		return true
	}
	if try(start) {
		return found
	}
	// The bounds and points between them, so a narrow or fractional range
	// that excludes its only whole step still yields a value inside it.
	low, hasLow := asFloat(schema["minimum"])
	if exclusive, ok := asFloat(schema["exclusiveMinimum"]); ok && (!hasLow || exclusive >= low) {
		low, hasLow = exclusive, true
	}
	high, hasHigh := asFloat(schema["maximum"])
	if exclusive, ok := asFloat(schema["exclusiveMaximum"]); ok && (!hasHigh || exclusive <= high) {
		high, hasHigh = exclusive, true
	}
	excludedCount := exclusionCount(schema)
	if hasLow && hasHigh {
		if try(low) || try(high) {
			return found
		}
		// Ever finer points between the bounds: a denominator of 2^k adds
		// 2^(k-1) points no earlier one had, so once that passes the length
		// of the exclusion list, one of them is free.
		for denominator := 2; denominator <= 4*(excludedCount+2); denominator *= 2 {
			for numerator := 1; numerator < denominator; numerator += 2 {
				if try(low + (high-low)*float64(numerator)/float64(denominator)) {
					return found
				}
			}
		}
	}
	for step := 1; step <= excludedCount+1; step++ {
		if try(start+float64(step)) || try(start-float64(step)) {
			return found
		}
	}
	return first
}

// exclusionCount is how many values the schema's `not` excludes.
func exclusionCount(schema map[string]any) int {
	not, _ := schema["not"].(map[string]any)
	members, _ := not["enum"].([]any)
	return len(members)
}

func asFloat(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok
}

// satisfies is the small validator behind synthesis: does value meet this
// schema's own constraints? Each keyword of the subset applies to the values
// it constrains whether or not the schema declares a type, as a hub applies
// them (section 6.1): type, enum, the exclusion, numeric bounds, required,
// properties, and items.
func satisfies(schema map[string]any, value any) bool {
	value = normalizeDeep(value)
	if excluded(schema, value) {
		return false
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		found := false
		for _, member := range enum {
			if reflect.DeepEqual(member, value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if schemaType, ok := schema["type"].(string); ok && !typeMatches(schemaType, value) {
		return false
	}
	switch typed := value.(type) {
	case map[string]any:
		required, _ := schema["required"].([]any)
		for _, name := range required {
			if key, isString := name.(string); isString {
				if _, present := typed[key]; !present {
					return false
				}
			}
		}
		// Every property present is held to its schema, required or not.
		properties, _ := schema["properties"].(map[string]any)
		for key, member := range typed {
			if property, isSchema := properties[key].(map[string]any); isSchema && !satisfies(property, member) {
				return false
			}
		}
	case []any:
		if items, isSchema := schema["items"].(map[string]any); isSchema {
			for _, element := range typed {
				if !satisfies(items, element) {
					return false
				}
			}
		}
	case float64:
		if minimum, has := asFloat(schema["minimum"]); has && typed < minimum {
			return false
		}
		if maximum, has := asFloat(schema["maximum"]); has && typed > maximum {
			return false
		}
		if exclusive, has := asFloat(schema["exclusiveMinimum"]); has && typed <= exclusive {
			return false
		}
		if exclusive, has := asFloat(schema["exclusiveMaximum"]); has && typed >= exclusive {
			return false
		}
	}
	return true
}

// typeMatches says whether a decoded JSON value is of a subset type,
// "integer" being a number with no fraction.
func typeMatches(name string, value any) bool {
	switch name {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && number == math.Trunc(number)
	}
	return true
}

// normalizeDeep puts a synthesized value, at every depth, in the shape JSON
// decoding gives it (numbers as float64), so it compares with schema
// constants as a hub compares the dispatched JSON.
func normalizeDeep(value any) any {
	switch typed := value.(type) {
	case int64:
		return float64(typed)
	case []any:
		copied := make([]any, len(typed))
		for i, element := range typed {
			copied[i] = normalizeDeep(element)
		}
		return copied
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, member := range typed {
			copied[key] = normalizeDeep(member)
		}
		return copied
	}
	return value
}

// subsetKeywords is the closed keyword set of spec section 6.1, as the
// companion schema pins it down. A hub rejects a manifest whose params schema
// carries anything else, so the harness fails a manifest that would never be
// accepted by a conformant hub.
var subsetKeywords = map[string]bool{
	"type": true, "enum": true, "required": true, "properties": true,
	"items": true, "minimum": true, "maximum": true,
	"exclusiveMinimum": true, "exclusiveMaximum": true,
	"default": true, "context": true, "x-vyshka-widget": true,
	"not": true,
}

var subsetTypes = map[string]bool{
	"object": true, "array": true, "string": true, "integer": true,
	"number": true, "boolean": true, "null": true,
}

// The bounds of the `context` annotation (section 6.1): a context id is at
// most 64 code points (section 6.2), and one field names at most 16.
const (
	maxAnnotationContextID = 64
	maxAnnotationContexts  = 16
)

// validateSubset walks a params schema and reports the first keyword outside
// the section 6.1 subset, with the path a plugin author needs to find it.
// declared is the set of custom context ids the manifest declares, which a
// `context` annotation must name (section 6.4).
func validateSubset(schema map[string]any, path string, declared map[string]bool) error {
	for keyword, value := range schema {
		if !subsetKeywords[keyword] {
			return fmt.Errorf("%s.%s: keyword %q is outside the schema subset this protocol enforces (section 6.1)", path, keyword, keyword)
		}
		switch keyword {
		case "enum", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
			if !exactConstant(value) {
				return fmt.Errorf("%s.%s: a number beyond 2^53 in magnitude cannot be compared exactly, and a hub rejects the manifest over it (section 6.1)", path, keyword)
			}
		case "type":
			name, ok := value.(string)
			if !ok || !subsetTypes[name] {
				return fmt.Errorf("%s.type: %v is not a type the subset defines (section 6.1)", path, value)
			}
		case "properties":
			properties, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.properties: not an object of schemas (section 6.1)", path)
			}
			for name, property := range properties {
				sub, ok := property.(map[string]any)
				if !ok {
					return fmt.Errorf("%s.properties.%s: not a schema object (section 6.1)", path, name)
				}
				if err := validateSubset(sub, path+".properties."+name, declared); err != nil {
					return err
				}
			}
		case "items":
			sub, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.items: not a schema object (section 6.1)", path)
			}
			if err := validateSubset(sub, path+".items", declared); err != nil {
				return err
			}
		case "not":
			if value == nil {
				// A JSON null reads as no exclusion (section 6.4).
				continue
			}
			if err := validateExclusion(value, path); err != nil {
				return err
			}
		case "context":
			if value == nil {
				// A JSON null reads as no annotation (section 6.4).
				continue
			}
			if err := validateContextAnnotation(value, path, declared); err != nil {
				return err
			}
			// The annotation belongs on a string schema, and the node's
			// type is checked here rather than in its own case because the
			// keywords come in map order.
			if name, _ := schema["type"].(string); name != "string" {
				return fmt.Errorf("%s.context: the context annotation belongs on a schema with \"type\": \"string\", and a hub rejects it elsewhere (section 6.1)", path)
			}
		}
	}
	return nil
}

// validateExclusion checks `not`, which the subset admits in one form only:
// an object whose single keyword is a non-empty `enum` (section 6.1).
func validateExclusion(value any, path string) error {
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf(`%s.not: must be an object of the form {"enum": [...]}; a hub rejects any other form (section 6.1)`, path)
	}
	for keyword := range object {
		if keyword != "enum" {
			return fmt.Errorf(`%s.not.%s: not admits only {"enum": [...]}, and a hub rejects the manifest over %q (section 6.4)`, path, keyword, keyword)
		}
	}
	members, ok := object["enum"].([]any)
	if !ok || len(members) == 0 {
		return fmt.Errorf("%s.not.enum: must be a non-empty array of the values excluded (section 6.1)", path)
	}
	if !exactConstant(members) {
		return fmt.Errorf("%s.not.enum: a number beyond 2^53 in magnitude cannot be compared exactly, and a hub rejects the manifest over it (section 6.1)", path)
	}
	return nil
}

// exactConstant reports whether every number inside a schema constant (an
// enum or exclusion member, or a bound) survives float64 exactly: a fraction,
// or an integer strictly within ±2^53, the rule a hub applies (section 6.1).
func exactConstant(value any) bool {
	switch typed := value.(type) {
	case float64:
		return typed != math.Trunc(typed) || math.Abs(typed) < 1<<53
	case []any:
		for _, element := range typed {
			if !exactConstant(element) {
				return false
			}
		}
	case map[string]any:
		for _, member := range typed {
			if !exactConstant(member) {
				return false
			}
		}
	}
	return true
}

// excluded reports whether value is one the schema's `not` excludes.
func excluded(schema map[string]any, value any) bool {
	not, _ := schema["not"].(map[string]any)
	members, _ := not["enum"].([]any)
	for _, member := range members {
		if reflect.DeepEqual(member, normalize(value)) {
			return true
		}
	}
	return false
}

// normalize puts a synthesized number in the shape JSON decoding gives a
// schema constant, so the int64 synthesizeNumber builds compares equal to
// the float64 an excluded member decodes as.
func normalize(value any) any {
	if integer, ok := value.(int64); ok {
		return float64(integer)
	}
	return value
}

// validateContextAnnotation checks one `context` value: a declared context
// id, or a non-empty array of at most 16 of them, none repeated.
func validateContextAnnotation(value any, path string, declared map[string]bool) error {
	var members []any
	switch typed := value.(type) {
	case string:
		members = []any{typed}
	case []any:
		members = typed
	default:
		return fmt.Errorf("%s.context: must be a context id or an array of them (section 6.1)", path)
	}
	if len(members) == 0 || len(members) > maxAnnotationContexts {
		return fmt.Errorf("%s.context: names %d contexts, want 1 to %d (section 6.1)", path, len(members), maxAnnotationContexts)
	}
	seen := map[string]bool{}
	for i, member := range members {
		id, ok := member.(string)
		if !ok || id == "" || utf8.RuneCountInString(id) > maxAnnotationContextID {
			return fmt.Errorf("%s.context[%d]: must be a non-empty context id of at most %d code points (section 6.1)", path, i, maxAnnotationContextID)
		}
		if seen[id] {
			return fmt.Errorf("%s.context: names %q twice (section 6.1)", path, id)
		}
		seen[id] = true
		if !declared[id] {
			return fmt.Errorf("%s.context: names %q, which the manifest's contexts do not declare; a hub rejects the manifest over it (section 6.4)", path, id)
		}
	}
	return nil
}

// synthesizeInvalidParams builds a params value the schema forbids, plus a
// description of how it violates. When the schema constrains nothing at all,
// no object can violate it, so the fallback is params that are not an object:
// something a conformant hub could never send, which is exactly what the
// crash check wants to hand the plugin.
func synthesizeInvalidParams(schema map[string]any) (json.RawMessage, string) {
	properties, _ := schema["properties"].(map[string]any)

	if required, ok := schema["required"].([]any); ok && len(required) > 0 {
		if first, ok := required[0].(string); ok {
			out := map[string]any{}
			for _, name := range required[1:] {
				key, ok := name.(string)
				if !ok {
					continue
				}
				property, _ := properties[key].(map[string]any)
				out[key] = synthesizeValue(property)
			}
			encoded, err := json.Marshal(out)
			if err == nil {
				return encoded, "omits the required property " + first
			}
		}
	}

	// No required properties: violate a property's declared type instead. The
	// keys are walked in sorted order so the choice is deterministic.
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		property, _ := properties[key].(map[string]any)
		propertyType, _ := property["type"].(string)
		var wrong any
		switch propertyType {
		case "string":
			wrong = 12345
		case "integer", "number", "boolean", "object", "array":
			wrong = "conformance-wrong-type"
		default:
			continue
		}
		encoded, err := json.Marshal(map[string]any{key: wrong})
		if err == nil {
			return encoded, "gives property " + key + " the wrong type"
		}
	}

	return json.RawMessage(`"schema-invalid"`), "is not a JSON object at all"
}
