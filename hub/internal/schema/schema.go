// Package schema implements the JSON Schema subset of spec/protocol.md
// section 6.1: the language a manifest may use to describe action parameters.
//
// The subset is closed on purpose. The hub validates dispatch payloads against
// these schemas before queueing (section 6.1), so a keyword it accepted but did
// not enforce would let exactly the input a mod author excluded reach the game
// server. Compile therefore rejects any keyword outside the subset instead of
// ignoring it.
package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
)

// maxDepth bounds schema nesting. Real parameter schemas are a few levels deep;
// the cap exists so an adversarial manifest cannot make compilation or
// validation recurse without limit.
const maxDepth = 32

// Fault names one thing wrong with a schema or an instance. Path is a dotted
// JSON path into whichever document the fault is about ("" for its root).
type Fault struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (f Fault) String() string {
	if f.Path == "" {
		return f.Message
	}
	return f.Path + ": " + f.Message
}

// typeNames are the values `type` may take. "integer" is "number" with an
// integral value, per JSON Schema.
var typeNames = map[string]bool{
	"object": true, "array": true, "string": true,
	"integer": true, "number": true, "boolean": true, "null": true,
}

// Schema is one compiled node of the subset.
type Schema struct {
	typeName   string
	enum       []any
	required   []string
	properties map[string]*Schema
	// propertyOrder keeps property iteration deterministic, so repeated
	// validation of the same instance reports the same first fault.
	propertyOrder []string
	items         *Schema

	minimum, maximum                   *float64
	exclusiveMinimum, exclusiveMaximum *float64
}

// Compile parses a raw schema and checks it against the subset. It returns
// every fault it can find rather than stopping at the first, so a manifest
// rejection can name all of what was wrong in one round trip.
func Compile(raw json.RawMessage) (*Schema, []Fault) {
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, []Fault{{Path: "", Message: "schema is not valid JSON: " + err.Error()}}
	}

	var faults []Fault
	compiled := compile(node, "", 0, &faults)
	if len(faults) > 0 {
		return nil, faults
	}
	return compiled, nil
}

func compile(node any, path string, depth int, faults *[]Fault) *Schema {
	fault := func(format string, args ...any) {
		*faults = append(*faults, Fault{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if depth > maxDepth {
		fault("schema nests deeper than %d levels", maxDepth)
		return nil
	}

	// The subset has no boolean schemas: a node is a JSON object or an error.
	object, ok := node.(map[string]any)
	if !ok {
		fault("a schema must be a JSON object")
		return nil
	}

	compiled := &Schema{}
	// Keywords are taken in sorted order so a rejected manifest reports the
	// same faults in the same order on every publish.
	for _, keyword := range sortedKeys(object) {
		value := object[keyword]
		child := joinPath(path, keyword)
		switch keyword {
		case "type":
			name, ok := value.(string)
			if !ok || !typeNames[name] {
				*faults = append(*faults, Fault{Path: child,
					Message: "type must be one of object, array, string, integer, number, boolean, null"})
				continue
			}
			compiled.typeName = name
		case "enum":
			values, ok := value.([]any)
			if !ok || len(values) == 0 {
				*faults = append(*faults, Fault{Path: child, Message: "enum must be a non-empty array"})
				continue
			}
			compiled.enum = values
		case "required":
			names, ok := value.([]any)
			if !ok {
				*faults = append(*faults, Fault{Path: child, Message: "required must be an array of property names"})
				continue
			}
			for i, name := range names {
				property, ok := name.(string)
				if !ok {
					*faults = append(*faults, Fault{Path: child + "[" + strconv.Itoa(i) + "]",
						Message: "required entries must be strings"})
					continue
				}
				compiled.required = append(compiled.required, property)
			}
		case "properties":
			properties, ok := value.(map[string]any)
			if !ok {
				*faults = append(*faults, Fault{Path: child, Message: "properties must be an object of schemas"})
				continue
			}
			compiled.properties = make(map[string]*Schema, len(properties))
			for _, name := range sortedKeys(properties) {
				compiled.propertyOrder = append(compiled.propertyOrder, name)
				compiled.properties[name] = compile(properties[name], joinPath(child, name), depth+1, faults)
			}
		case "items":
			compiled.items = compile(value, child, depth+1, faults)
		case "minimum":
			compiled.minimum = compileBound(value, child, faults)
		case "maximum":
			compiled.maximum = compileBound(value, child, faults)
		case "exclusiveMinimum":
			compiled.exclusiveMinimum = compileBound(value, child, faults)
		case "exclusiveMaximum":
			compiled.exclusiveMaximum = compileBound(value, child, faults)
		case "default":
			// Annotation only: surfaced to UIs, never enforced.
		case "x-vyshka-widget":
			// A UI hint, deliberately unconstrained (section 6.1): a widget
			// name this hub has not heard of must not reject the manifest,
			// because the hint never constrains the data model.
			if _, ok := value.(string); !ok {
				*faults = append(*faults, Fault{Path: child, Message: "x-vyshka-widget must be a string"})
			}
		default:
			*faults = append(*faults, Fault{Path: child,
				Message: fmt.Sprintf("keyword %q is outside the schema subset this protocol enforces", keyword)})
		}
	}
	return compiled
}

func compileBound(value any, path string, faults *[]Fault) *float64 {
	number, ok := value.(float64)
	if !ok {
		*faults = append(*faults, Fault{Path: path, Message: "numeric bounds must be numbers"})
		return nil
	}
	return &number
}

// Validate checks an instance against the compiled schema and returns every
// fault found. A nil fault slice means the instance conforms.
func (s *Schema) Validate(instance json.RawMessage) []Fault {
	var value any
	if err := json.Unmarshal(instance, &value); err != nil {
		return []Fault{{Path: "", Message: "instance is not valid JSON: " + err.Error()}}
	}
	var faults []Fault
	s.validate(value, "", &faults)
	return faults
}

func (s *Schema) validate(value any, path string, faults *[]Fault) {
	fault := func(format string, args ...any) {
		*faults = append(*faults, Fault{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if s == nil {
		return
	}

	if s.typeName != "" && !typeMatches(s.typeName, value) {
		fault("expected %s, got %s", s.typeName, jsonTypeName(value))
		return
	}

	if s.enum != nil {
		found := false
		for _, allowed := range s.enum {
			if reflect.DeepEqual(allowed, value) {
				found = true
				break
			}
		}
		if !found {
			fault("value is not one of the enum's %d allowed values", len(s.enum))
		}
	}

	switch typed := value.(type) {
	case map[string]any:
		for _, name := range s.required {
			if _, present := typed[name]; !present {
				fault("required property %q is missing", name)
			}
		}
		for _, name := range s.propertyOrder {
			if property, present := typed[name]; present {
				s.properties[name].validate(property, joinPath(path, name), faults)
			}
		}
	case []any:
		if s.items != nil {
			for i, element := range typed {
				s.items.validate(element, path+"["+strconv.Itoa(i)+"]", faults)
			}
		}
	case float64:
		if s.minimum != nil && typed < *s.minimum {
			fault("%v is below the minimum %v", typed, *s.minimum)
		}
		if s.maximum != nil && typed > *s.maximum {
			fault("%v is above the maximum %v", typed, *s.maximum)
		}
		if s.exclusiveMinimum != nil && typed <= *s.exclusiveMinimum {
			fault("%v is not above the exclusive minimum %v", typed, *s.exclusiveMinimum)
		}
		if s.exclusiveMaximum != nil && typed >= *s.exclusiveMaximum {
			fault("%v is not below the exclusive maximum %v", typed, *s.exclusiveMaximum)
		}
	}
}

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
		return ok && number == math.Trunc(number) && !math.IsInf(number, 0)
	}
	return false
}

func jsonTypeName(value any) string {
	switch value.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case nil:
		return "null"
	}
	return "unknown"
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
