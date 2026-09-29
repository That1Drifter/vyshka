// Package params turns the key=value arguments of `vyshka run` into an
// action's params object, typed by the params schema the action's manifest
// declares (spec section 6.1), and checks the result against that schema
// before anything is sent.
//
// The hub validates every dispatch itself, so this is a convenience, not the
// gate: it answers a typo or an out-of-range number at the terminal, naming
// the key, instead of after a params_invalid round trip. Its checks mirror
// the hub's (type, enum, not.enum, numeric bounds, required), plus one the hub
// does not make: a key the schema does not declare is refused, because on the
// command line an undeclared key is almost always a misspelled one.
package params

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/That1Drifter/vyshka/client"
)

// Coerce builds a params object from args, each `key=value` (typed by the
// schema) or `key:=<json>` (raw JSON, for what key=value cannot express).
// Integers come out as int64 and other numbers as float64, so the wire JSON
// carries 5 rather than 5.0. A nil schema, or one declaring no properties,
// accepts any key and reads each value as JSON when it parses and as a string
// otherwise.
func Coerce(schema *client.ParamsSchema, args []string) (map[string]any, error) {
	result := make(map[string]any, len(args))
	order := make([]string, 0, len(args))
	declared := schema != nil && len(schema.Properties) > 0

	for _, arg := range args {
		key, text, raw, err := split(arg)
		if err != nil {
			return nil, err
		}
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("param %q is given twice", key)
		}

		var property *client.ParamsSchema
		if declared {
			found, ok := schema.Properties[key]
			if !ok {
				return nil, fmt.Errorf("unknown param %q; declared params: %s",
					key, strings.Join(sortedKeys(schema.Properties), ", "))
			}
			property = found
		}

		var value any
		switch {
		case raw:
			value, err = parseJSON(text)
			if err != nil {
				return nil, fmt.Errorf("%s: %s is not valid JSON: %v", key, strconv.Quote(text), err)
			}
		case property != nil:
			value, err = coerce(key, property, text)
		default:
			value, err = jsonOrString(key, text)
		}
		if err != nil {
			return nil, err
		}
		result[key] = value
		order = append(order, key)
	}

	if schema == nil {
		return result, nil
	}
	for _, key := range order {
		if err := validate(key, schema.Properties[key], result[key]); err != nil {
			return nil, err
		}
	}
	var missing []string
	for _, name := range schema.Required {
		if _, present := result[name]; !present {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required params: %s", strings.Join(missing, ", "))
	}
	return result, nil
}

// split reads one argument. The first `=` ends the key, and a `:` right
// before it marks the raw JSON form, so a value may itself contain `=`.
func split(arg string) (key, text string, raw bool, err error) {
	at := strings.IndexByte(arg, '=')
	if at < 0 {
		return "", "", false, fmt.Errorf("param %s is neither key=value nor key:=<json>", strconv.Quote(arg))
	}
	key, text = arg[:at], arg[at+1:]
	if strings.HasSuffix(key, ":") {
		key, raw = strings.TrimSuffix(key, ":"), true
	}
	if key == "" {
		return "", "", false, fmt.Errorf("param %s has no key", strconv.Quote(arg))
	}
	return key, text, raw, nil
}

// coerce types one key=value text by its property schema.
func coerce(path string, schema *client.ParamsSchema, text string) (any, error) {
	switch schema.Type {
	case "integer":
		number, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %s is not an integer", path, strconv.Quote(text))
		}
		return number, nil
	case "number":
		return parseNumber(path, text)
	case "boolean":
		switch text {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, fmt.Errorf("%s: %s is not a boolean; use true or false", path, strconv.Quote(text))
	case "string":
		return text, nil
	case "null":
		if text != "null" {
			return nil, fmt.Errorf("%s: %s is not null; this param takes only the word null", path, strconv.Quote(text))
		}
		return nil, nil
	case "array":
		// An empty value is the empty list, so there is a way to write one.
		items := []any{}
		if text == "" {
			return items, nil
		}
		if schema.Items != nil && (schema.Items.Type == "array" || schema.Items.Type == "object") {
			return nil, fmt.Errorf("%s holds %ss, which key=value cannot express; pass it as %s:=<json>",
				path, schema.Items.Type, path)
		}
		for i, part := range strings.Split(text, ",") {
			itemPath := path + "[" + strconv.Itoa(i) + "]"
			var item any
			var err error
			if schema.Items == nil {
				// Untyped items are strings: splitting on commas already
				// decided what the user meant by each one.
				item = part
			} else {
				item, err = coerce(itemPath, schema.Items, part)
			}
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, nil
	case "object":
		return nil, fmt.Errorf("%s is an object, which key=value cannot express; pass it as %s:=<json>", path, path)
	default:
		// No type, or one a later draft added: the value speaks for itself.
		return jsonOrString(path, text)
	}
}

func parseNumber(path, text string) (float64, error) {
	number, err := strconv.ParseFloat(text, 64)
	// ParseFloat also reads Go's hexadecimal floats, which no one means on
	// a command line and JSON cannot carry.
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || strings.ContainsAny(text, "xX") {
		return 0, fmt.Errorf("%s: %s is not a finite number", path, strconv.Quote(text))
	}
	return number, nil
}

// jsonOrString reads text as JSON when it parses and as a string otherwise.
func jsonOrString(path, text string) (any, error) {
	value, err := parseJSON(text)
	var rangeErr *outOfRangeError
	switch {
	case errors.As(err, &rangeErr):
		return nil, fmt.Errorf("%s: %v", path, err)
	case err != nil:
		return text, nil
	}
	return value, nil
}

// outOfRangeError is a JSON number too large for a float64. It is valid JSON,
// so it must not quietly become a string instead.
type outOfRangeError struct{ text string }

func (e *outOfRangeError) Error() string { return e.text + " is out of range for a number" }

// parseJSON decodes exactly one JSON value, numbers read exactly: an integer
// that fits int64 stays an int64 rather than passing through a float64.
func parseJSON(text string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("there is more after the first JSON value")
	}
	return normalize(value)
}

func normalize(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer, nil
		}
		number, err := typed.Float64()
		if err != nil || math.IsInf(number, 0) {
			return nil, &outOfRangeError{text: typed.String()}
		}
		return number, nil
	case []any:
		for i := range typed {
			item, err := normalize(typed[i])
			if err != nil {
				return nil, err
			}
			typed[i] = item
		}
	case map[string]any:
		for key := range typed {
			member, err := normalize(typed[key])
			if err != nil {
				return nil, err
			}
			typed[key] = member
		}
	}
	return value, nil
}

// validate checks one value against its schema the way the hub will: type,
// enum, not.enum, numeric bounds, and, below it, required members and items.
// A nil schema admits anything.
func validate(path string, schema *client.ParamsSchema, value any) error {
	if schema == nil {
		return nil
	}
	if schema.Type != "" && !typeMatches(schema.Type, value) {
		return fmt.Errorf("%s: expected %s, got %s", path, schema.Type, typeName(value))
	}
	if schema.Enum != nil && !containsJSON(schema.Enum, value) {
		return fmt.Errorf("%s: %s is not one of the allowed values: %s", path, encode(value), encodeList(schema.Enum))
	}
	if schema.Not != nil && containsJSON(schema.Not.Enum, value) {
		return fmt.Errorf("%s: %s is excluded by the schema", path, encode(value))
	}

	switch typed := value.(type) {
	case map[string]any:
		var missing []string
		for _, name := range schema.Required {
			if _, present := typed[name]; !present {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%s: missing required members: %s", path, strings.Join(missing, ", "))
		}
		for _, name := range sortedKeys(schema.Properties) {
			if member, present := typed[name]; present {
				if err := validate(path+"."+name, schema.Properties[name], member); err != nil {
					return err
				}
			}
		}
	case []any:
		for i, item := range typed {
			if err := validate(path+"["+strconv.Itoa(i)+"]", schema.Items, item); err != nil {
				return err
			}
		}
	default:
		if number, ok := asNumber(value); ok {
			return checkBounds(path, schema, value, number)
		}
	}
	return nil
}

func checkBounds(path string, schema *client.ParamsSchema, value any, number float64) error {
	shown := encode(value)
	switch {
	case schema.Minimum != nil && number < *schema.Minimum:
		return fmt.Errorf("%s: %s is below the minimum %s", path, shown, formatBound(*schema.Minimum))
	case schema.Maximum != nil && number > *schema.Maximum:
		return fmt.Errorf("%s: %s is above the maximum %s", path, shown, formatBound(*schema.Maximum))
	case schema.ExclusiveMinimum != nil && number <= *schema.ExclusiveMinimum:
		return fmt.Errorf("%s: %s is not above the exclusive minimum %s", path, shown, formatBound(*schema.ExclusiveMinimum))
	case schema.ExclusiveMaximum != nil && number >= *schema.ExclusiveMaximum:
		return fmt.Errorf("%s: %s is not below the exclusive maximum %s", path, shown, formatBound(*schema.ExclusiveMaximum))
	}
	return nil
}

func asNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	}
	return 0, false
}

// typeMatches follows the hub: an integer is any number with no fractional
// part, 5.0 included, and a type this draft does not name admits anything,
// leaving the verdict to the hub that knows it.
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
		_, ok := asNumber(value)
		return ok
	case "integer":
		switch typed := value.(type) {
		case int64:
			return true
		case float64:
			return typed == math.Trunc(typed)
		}
		return false
	}
	return true
}

func typeName(value any) string {
	switch value.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case int64, float64:
		return "number"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", value)
}

// containsJSON compares by JSON encoding, which is how deep equality over
// JSON values reads once numbers are involved: an int64 5 from the command
// line and a float64 5 from the schema both encode as 5.
func containsJSON(members []any, value any) bool {
	want, err := json.Marshal(value)
	if err != nil {
		return false
	}
	for _, member := range members {
		got, err := json.Marshal(member)
		if err == nil && bytes.Equal(got, want) {
			return true
		}
	}
	return false
}

func encode(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func encodeList(values []any) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = encode(value)
	}
	return strings.Join(parts, ", ")
}

func formatBound(bound float64) string {
	return strconv.FormatFloat(bound, 'f', -1, 64)
}

func sortedKeys(properties map[string]*client.ParamsSchema) []string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
