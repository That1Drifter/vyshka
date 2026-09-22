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
		for _, member := range enum {
			if !excluded(schema, member) {
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
			property, _ := properties[key].(map[string]any)
			out[key] = synthesizeValue(property)
		}
		return out
	case "array":
		return []any{}
	case "string":
		// A value the schema's `not` excludes (section 6.1) is stepped past.
		value := "conformance"
		for i := 1; excluded(schema, value) && i <= 100; i++ {
			value = "conformance-" + strconv.Itoa(i)
		}
		return value
	case "boolean":
		return !excluded(schema, true)
	case "null":
		return nil
	case "integer", "number":
		value := synthesizeNumber(schema, schemaType)
		for i := 0; excluded(schema, value) && i < 100; i++ {
			switch typed := value.(type) {
			case int64:
				value = typed + 1
			case float64:
				value = typed + 1
			}
		}
		return value
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

func asFloat(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok
}

// satisfies is the small validator behind synthesis: does value meet this
// schema's own constraints? It checks what the subset can express (type, enum,
// exclusions, bounds, required properties) and nothing more.
func satisfies(schema map[string]any, value any) bool {
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
	schemaType, _ := schema["type"].(string)
	switch schemaType {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		required, _ := schema["required"].([]any)
		properties, _ := schema["properties"].(map[string]any)
		for _, name := range required {
			key, isString := name.(string)
			if !isString {
				continue
			}
			member, present := object[key]
			if !present {
				return false
			}
			if property, isSchema := properties[key].(map[string]any); isSchema && !satisfies(property, member) {
				return false
			}
		}
		return true
	case "integer", "number":
		number, ok := value.(float64)
		if !ok {
			return false
		}
		if schemaType == "integer" && number != math.Trunc(number) {
			return false
		}
		if minimum, has := asFloat(schema["minimum"]); has && number < minimum {
			return false
		}
		if maximum, has := asFloat(schema["maximum"]); has && number > maximum {
			return false
		}
		if exclusive, has := asFloat(schema["exclusiveMinimum"]); has && number <= exclusive {
			return false
		}
		if exclusive, has := asFloat(schema["exclusiveMaximum"]); has && number >= exclusive {
			return false
		}
		return true
	default:
		return true
	}
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
	if members, ok := object["enum"].([]any); !ok || len(members) == 0 {
		return fmt.Errorf("%s.not.enum: must be a non-empty array of the values excluded (section 6.1)", path)
	}
	return nil
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
