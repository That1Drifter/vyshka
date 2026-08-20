package main

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
)

// The harness dispatches whatever action the candidate's manifest declares, so
// it has to invent params from the action's own schema. The schema language is
// the section 6.1 subset: object/array/scalar types, enum, required, numeric
// bounds, default. Nothing here validates a schema; it builds one instance that
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
		return "conformance"
	case "boolean":
		return true
	case "null":
		return nil
	case "integer", "number":
		return synthesizeNumber(schema, schemaType)
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
// bounds, required properties) and nothing more.
func satisfies(schema map[string]any, value any) bool {
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
	"default": true, "x-vyshka-widget": true,
}

var subsetTypes = map[string]bool{
	"object": true, "array": true, "string": true, "integer": true,
	"number": true, "boolean": true, "null": true,
}

// validateSubset walks a params schema and reports the first keyword outside
// the section 6.1 subset, with the path a plugin author needs to find it.
func validateSubset(schema map[string]any, path string) error {
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
				if err := validateSubset(sub, path+".properties."+name); err != nil {
					return err
				}
			}
		case "items":
			sub, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.items: not a schema object (section 6.1)", path)
			}
			if err := validateSubset(sub, path+".items"); err != nil {
				return err
			}
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
