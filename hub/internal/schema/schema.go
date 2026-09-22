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
	"unicode/utf8"
)

// maxDepth bounds schema nesting. Real parameter schemas are a few levels deep;
// the cap exists so an adversarial manifest cannot make compilation or
// validation recurse without limit.
const maxDepth = 32

// The bounds of the `context` annotation (spec section 6.1): a context id is
// at most 64 code points (section 6.2), and one field draws on at most 16 of
// them, which is more than a form has any use for and few enough that a
// manifest cannot make a UI fetch hundreds of enumerations per field.
const (
	maxContextIDLength = 64
	maxContextRefs     = 16
)

// maxExactNumber is 2^53, the largest magnitude at which float64, the type
// every JSON number here passes through, still represents integers exactly.
// A bound or enum constant beyond it would be enforced against a rounded
// value: `{"minimum": 9007199254740993}` would admit 9007199254740992, and the
// schema's author would never know. Rejecting the constant at publish keeps
// "the hub enforces what it accepted" true.
const maxExactNumber = float64(1 << 53)

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

	// excluded is `not` in the one form the subset admits, `{"enum": [...]}`
	// (section 6.1): the values an instance must not take, compared the way
	// enum compares. Nil when the schema excludes nothing.
	excluded []any

	// contexts is the `context` annotation: the declared custom contexts
	// whose entries a UI offers for this string field (section 6.1). Kept
	// for ContextRefs, never consulted by Validate.
	contexts []string
}

// ContextRef is one `context` annotation found in a compiled schema: the
// dotted path of the node carrying it and the context id it names.
type ContextRef struct {
	Path string
	ID   string
}

// ContextRefs lists every context id the schema's `context` annotations
// name, in path order, so a manifest validator can check each against the
// contexts the manifest declares (section 6.4).
func (s *Schema) ContextRefs() []ContextRef {
	var refs []ContextRef
	s.contextRefs("", &refs)
	return refs
}

func (s *Schema) contextRefs(path string, refs *[]ContextRef) {
	if s == nil {
		return
	}
	for _, id := range s.contexts {
		*refs = append(*refs, ContextRef{Path: joinPath(path, "context"), ID: id})
	}
	for _, name := range s.propertyOrder {
		s.properties[name].contextRefs(joinPath(path, "properties."+name), refs)
	}
	if s.items != nil {
		s.items.contextRefs(joinPath(path, "items"), refs)
	}
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
			for i, member := range values {
				inexactConstants(member, child+"["+strconv.Itoa(i)+"]", faults)
			}
			compiled.enum = values
		case "not":
			// A JSON null reads as no exclusion (section 6.4), as a null
			// context annotation does.
			if value == nil {
				continue
			}
			compiled.excluded = compileExclusion(value, child, faults)
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
		case "context":
			// Annotation only as well (section 6.1), but a constrained one:
			// it names manifest entities, so a malformed value is a typo the
			// author wants to hear about at publish. Whether each id is
			// declared is the manifest validator's check (ContextRefs); the
			// type requirement is checked once every keyword is read, since
			// "context" sorts before "type". A JSON null reads as no
			// annotation (section 6.4).
			if value == nil {
				continue
			}
			compiled.contexts = compileContextRefs(value, child, faults)
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
	if compiled.contexts != nil && compiled.typeName != "string" {
		*faults = append(*faults, Fault{Path: joinPath(path, "context"),
			Message: "context is an annotation for a string schema; add \"type\": \"string\" beside it"})
	}
	return compiled
}

// compileContextRefs reads a `context` annotation: one context id, or an
// array of them. It returns nil with faults recorded when the value is
// unusable, and a non-nil (possibly single-element) slice otherwise.
func compileContextRefs(value any, path string, faults *[]Fault) []string {
	fault := func(format string, args ...any) []string {
		*faults = append(*faults, Fault{Path: path, Message: fmt.Sprintf(format, args...)})
		return nil
	}
	var members []any
	switch typed := value.(type) {
	case string:
		members = []any{typed}
	case []any:
		members = typed
	default:
		return fault("context must be a context id or an array of them")
	}
	if len(members) == 0 {
		return fault("context must name at least one context")
	}
	if len(members) > maxContextRefs {
		return fault("context may name at most %d contexts, got %d", maxContextRefs, len(members))
	}
	ids := make([]string, 0, len(members))
	seen := make(map[string]bool, len(members))
	for i, member := range members {
		id, ok := member.(string)
		if !ok || id == "" {
			return fault("context[%d] must be a non-empty context id", i)
		}
		if utf8.RuneCountInString(id) > maxContextIDLength {
			return fault("context[%d] is longer than %d characters", i, maxContextIDLength)
		}
		if seen[id] {
			return fault("context names %q more than once", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// compileExclusion reads `not`, which the subset admits in one form only: an
// object whose single keyword is a non-empty `enum` (section 6.1). That form
// is the one a UI can render, by showing the listed values as unavailable; a
// general `not` over any subschema would be enforceable but not displayable,
// and a keyword a UI cannot show is a promise to the operator nobody keeps.
// Returns nil with faults recorded when the value is not that form.
func compileExclusion(value any, path string, faults *[]Fault) []any {
	fault := func(at, message string) []any {
		*faults = append(*faults, Fault{Path: at, Message: message})
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fault(path, `not must be an object of the form {"enum": [...]}`)
	}
	for _, keyword := range sortedKeys(object) {
		if keyword != "enum" {
			return fault(joinPath(path, keyword),
				fmt.Sprintf(`keyword %q inside not is outside the subset: not admits only {"enum": [...]}`, keyword))
		}
	}
	members, ok := object["enum"].([]any)
	if !ok || len(members) == 0 {
		return fault(joinPath(path, "enum"), "not must carry a non-empty enum of the values it excludes")
	}
	allExact := true
	for i, member := range members {
		if !inexactConstants(member, joinPath(path, "enum")+"["+strconv.Itoa(i)+"]", faults) {
			allExact = false
		}
	}
	if !allExact {
		return nil
	}
	return members
}

// inexactConstants records a fault for every number inside a constant (an
// enum or exclusion member, at any depth of an object or array member) that a
// float64 does not hold exactly, and reports whether there was none: such a
// constant would be compared as a rounded value its author never wrote.
func inexactConstants(value any, path string, faults *[]Fault) bool {
	switch typed := value.(type) {
	case float64:
		if !exact(typed) {
			*faults = append(*faults, Fault{Path: path,
				Message: "numbers beyond 2^53 in magnitude cannot be compared exactly"})
			return false
		}
	case []any:
		clean := true
		for i, element := range typed {
			if !inexactConstants(element, path+"["+strconv.Itoa(i)+"]", faults) {
				clean = false
			}
		}
		return clean
	case map[string]any:
		clean := true
		for _, key := range sortedKeys(typed) {
			if !inexactConstants(typed[key], joinPath(path, key), faults) {
				clean = false
			}
		}
		return clean
	}
	return true
}

func compileBound(value any, path string, faults *[]Fault) *float64 {
	number, ok := value.(float64)
	if !ok {
		*faults = append(*faults, Fault{Path: path, Message: "numeric bounds must be numbers"})
		return nil
	}
	if !exact(number) {
		*faults = append(*faults, Fault{Path: path,
			Message: "numbers beyond 2^53 in magnitude cannot be enforced exactly"})
		return nil
	}
	return &number
}

// exact reports whether a number survived the trip through float64 exactly:
// either it is fractional by intent, or it is an integer strictly within
// ±2^53. The bound itself is excluded because 2^53 and 2^53+1 parse to the
// same float, so a value of exactly 2^53 cannot prove what was written.
func exact(number float64) bool {
	if number != math.Trunc(number) {
		return true
	}
	return number > -maxExactNumber && number < maxExactNumber
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

	for _, refused := range s.excluded {
		if reflect.DeepEqual(refused, value) {
			fault("value is one of the %d values the schema excludes", len(s.excluded))
			break
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
