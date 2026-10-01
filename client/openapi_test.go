package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// This file holds the client to spec/openapi-admin.yaml, which is what keeps
// a hand-written client honest about the document it was not generated
// from. Three checks:
//
//   - Coverage: every operation in the document has a method here, and every
//     exported method is either one operation's or named in clientExtras.
//   - The wire: each method, called with every option set, sends a request
//     the document declares for its operation. The path and method resolve
//     to that operation and no other, every query parameter and body member
//     it sends is declared, and every one the document declares is sent, so
//     an option the client cannot express fails here.
//   - The records: the type each method decodes an answer into, and the type
//     it encodes a body from, have the members the schema has, of matching
//     JSON types, with nullable members held where null cannot be lost.
//
// What the hub answers is graded against the same document elsewhere: by the
// TypeScript client's live test (client/typescript/test/live.test.ts).

// operationCase calls one method with every option set. answer names the
// member of the 2xx body the method returns, when it returns one member of a
// wrapper rather than the whole body.
type operationCase struct {
	operation string
	method    string
	answer    string
	call      func(ctx context.Context, c *Client) error
}

// clientExtras are the exported methods that are no operation of the
// document, and why.
var clientExtras = map[string]string{
	"Health":     "GET /healthz is the reference hub's health check, not part of the protocol",
	"WaitAction": "reads getAction until a terminal state",
}

// recordExceptions are member differences between a record type and its
// schema that are deliberate, keyed by operation, Go type, and JSON member
// name, so an exception excuses the one answer it was written for and no
// other that happens to use the same type.
var recordExceptions = map[string]string{
	"listEvents:Event.roles": "one Event type serves both feeds; only the player feed's schema (an allOf) declares roles",
	"enumerateContext:ContextEntries.reason": "a null reason and an absent one mean the same thing, so it decodes " +
		"to the empty string as documented on the field",
}

func excepted(operation, typeName, member string) bool {
	return recordExceptions[operation+":"+typeName+"."+member] != ""
}

func operationCases() []operationCase {
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	delta := int64(5)
	revision := int64(3)
	text := "x"
	paused := true
	list := []string{"core.*"}
	events := EventQuery{Types: []string{"core.*", "mod.raid"}, Since: &since, Until: &until, Limit: 10, Cursor: "c"}
	page := PageQuery{Limit: 10, Cursor: "c"}

	return []operationCase{
		{"listServers", "ListServers", "", func(ctx context.Context, c *Client) error {
			_, err := c.ListServers(ctx)
			return err
		}},
		{"createServer", "CreateServer", "", func(ctx context.Context, c *Client) error {
			_, err := c.CreateServer(ctx, CreateServerRequest{Name: "n", Game: "dayz", EnrollmentTokenTTLSeconds: 60})
			return err
		}},
		{"getServer", "GetServer", "", func(ctx context.Context, c *Client) error {
			_, err := c.GetServer(ctx, "s1")
			return err
		}},
		{"issueEnrollmentToken", "IssueEnrollmentToken", "", func(ctx context.Context, c *Client) error {
			_, err := c.IssueEnrollmentToken(ctx, "s1", 60)
			return err
		}},
		{"revokeServerCredentials", "RevokeServerCredentials", "", func(ctx context.Context, c *Client) error {
			return c.RevokeServerCredentials(ctx, "s1")
		}},
		{"queueEnvelope", "QueueEnvelope", "envelope", func(ctx context.Context, c *Client) error {
			_, err := c.QueueEnvelope(ctx, "s1", EnvelopeRequest{Type: "mod.reload", Body: map[string]any{"a": 1}})
			return err
		}},
		{"getServerManifest", "GetManifest", "", func(ctx context.Context, c *Client) error {
			_, err := c.GetManifest(ctx, "s1")
			return err
		}},
		{"enumerateContext", "EnumerateContext", "", func(ctx context.Context, c *Client) error {
			_, err := c.EnumerateContext(ctx, "s1", "zone", true)
			return err
		}},
		{"dispatchAction", "DispatchAction", "", func(ctx context.Context, c *Client) error {
			_, err := c.DispatchAction(ctx, "s1", DispatchRequest{
				Code: "mod.heal", Context: "player", ReferenceKey: "p1", Params: map[string]any{"a": 1},
				TTLSeconds: 60, IdempotencyKey: "k",
			})
			return err
		}},
		{"getAction", "GetAction", "", func(ctx context.Context, c *Client) error {
			_, err := c.GetAction(ctx, "a1")
			return err
		}},
		{"listEvents", "ListEvents", "", func(ctx context.Context, c *Client) error {
			_, err := c.ListEvents(ctx, "s1", events)
			return err
		}},
		{"getState", "GetState", "", func(ctx context.Context, c *Client) error {
			_, err := c.GetState(ctx, "s1", "players")
			return err
		}},
		{"getStateHistory", "StateHistory", "", func(ctx context.Context, c *Client) error {
			_, err := c.StateHistory(ctx, "s1", "players", 5)
			return err
		}},
		{"listTokens", "ListTokens", "", func(ctx context.Context, c *Client) error {
			_, err := c.ListTokens(ctx)
			return err
		}},
		{"createToken", "CreateToken", "", func(ctx context.Context, c *Client) error {
			_, err := c.CreateToken(ctx, CreateTokenRequest{
				Name: "n", Scopes: []string{"servers:read"}, Servers: []string{"s1"}, ExpiresInSeconds: 60,
			})
			return err
		}},
		{"revokeToken", "RevokeToken", "", func(ctx context.Context, c *Client) error {
			return c.RevokeToken(ctx, "t1")
		}},
		{"listAudit", "ListAudit", "", func(ctx context.Context, c *Client) error {
			_, err := c.ListAudit(ctx, AuditQuery{TokenID: "t1", ServerID: "s1", Since: &since, Until: &until, Limit: 10, Cursor: "c"})
			return err
		}},
		{"listPlayerEvents", "PlayerEvents", "", func(ctx context.Context, c *Client) error {
			_, err := c.PlayerEvents(ctx, "steam", "p1", events)
			return err
		}},
		{"listPlayerActions", "PlayerActions", "", func(ctx context.Context, c *Client) error {
			_, err := c.PlayerActions(ctx, "steam", "p1", page)
			return err
		}},
		{"listPlayerNotes", "PlayerNotes", "", func(ctx context.Context, c *Client) error {
			_, err := c.PlayerNotes(ctx, "steam", "p1", page)
			return err
		}},
		{"createPlayerNote", "CreatePlayerNote", "note", func(ctx context.Context, c *Client) error {
			_, err := c.CreatePlayerNote(ctx, "steam", "p1", text)
			return err
		}},
		{"deletePlayerNote", "DeletePlayerNote", "", func(ctx context.Context, c *Client) error {
			return c.DeletePlayerNote(ctx, "steam", "p1", "n1")
		}},
		{"listBans", "ListBans", "", func(ctx context.Context, c *Client) error {
			_, err := c.ListBans(ctx, BanQuery{State: "all", Player: &Identity{Platform: "steam", ID: "p1"}, Limit: 10, Cursor: "c"})
			return err
		}},
		{"createBan", "CreateBan", "", func(ctx context.Context, c *Client) error {
			_, err := c.CreateBan(ctx, CreateBanRequest{
				Player: Identity{Platform: "steam", ID: "p1"}, Reason: "r", DurationSeconds: 60, Name: "n", ServerID: "s1",
			})
			return err
		}},
		{"getBan", "GetBan", "ban", func(ctx context.Context, c *Client) error {
			_, err := c.GetBan(ctx, "b1")
			return err
		}},
		{"liftBan", "LiftBan", "", func(ctx context.Context, c *Client) error {
			_, err := c.LiftBan(ctx, "b1")
			return err
		}},
		{"listWebhooks", "ListWebhooks", "", func(ctx context.Context, c *Client) error {
			_, err := c.ListWebhooks(ctx)
			return err
		}},
		{"createWebhook", "CreateWebhook", "", func(ctx context.Context, c *Client) error {
			_, err := c.CreateWebhook(ctx, CreateWebhookRequest{
				URL: "https://example.net/h", Events: list, ServerIDs: []string{"s1"}, Template: "discord", Redact: []string{"position"},
			})
			return err
		}},
		{"updateWebhook", "UpdateWebhook", "webhook", func(ctx context.Context, c *Client) error {
			url, template := "https://example.net/h", "discord"
			_, err := c.UpdateWebhook(ctx, "w1", UpdateWebhookRequest{
				URL: &url, Events: &list, ServerIDs: &list, Template: &template, Redact: &list, Paused: &paused,
			})
			return err
		}},
		{"deleteWebhook", "DeleteWebhook", "", func(ctx context.Context, c *Client) error {
			return c.DeleteWebhook(ctx, "w1")
		}},
		{"listWebhookDeliveries", "WebhookDeliveries", "", func(ctx context.Context, c *Client) error {
			_, err := c.WebhookDeliveries(ctx, "w1", 10)
			return err
		}},
		{"replayWebhookDelivery", "ReplayDelivery", "delivery", func(ctx context.Context, c *Client) error {
			_, err := c.ReplayDelivery(ctx, "w1", "d1")
			return err
		}},
		{"listKVNamespaces", "KVListNamespaces", "", func(ctx context.Context, c *Client) error {
			_, err := c.KVListNamespaces(ctx)
			return err
		}},
		{"listKVKeys", "KVListKeys", "", func(ctx context.Context, c *Client) error {
			_, err := c.KVListKeys(ctx, "mod", KVListQuery{Prefix: "a", Limit: 10, Cursor: "c"})
			return err
		}},
		{"kvGet", "KVGet", "", func(ctx context.Context, c *Client) error {
			_, err := c.KVGet(ctx, "mod", "k")
			return err
		}},
		{"kvSet", "KVSet", "", func(ctx context.Context, c *Client) error {
			_, err := c.KVSet(ctx, "mod", "k", KVSetRequest{Value: 1, IfRevision: &revision, TTLSeconds: 60})
			return err
		}},
		{"kvDelete", "KVDelete", "", func(ctx context.Context, c *Client) error {
			return c.KVDelete(ctx, "mod", "k")
		}},
		{"kvIncr", "KVIncr", "", func(ctx context.Context, c *Client) error {
			_, err := c.KVIncr(ctx, "mod", "k", &delta)
			return err
		}},
	}
}

// openAPI is the parsed document with the lookups the checks need.
type openAPI struct {
	doc        map[string]any
	operations map[string]operationInfo
}

type operationInfo struct {
	id     string
	method string
	path   string
	node   map[string]any
	// params are the operation's parameters with those of its path item.
	params []map[string]any
}

func loadOpenAPI(t *testing.T) *openAPI {
	t.Helper()
	raw, err := os.ReadFile("../spec/openapi-admin.yaml")
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	api := &openAPI{doc: doc, operations: map[string]operationInfo{}}
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("the document declares no paths")
	}
	for path, item := range paths {
		pathItem := item.(map[string]any)
		var shared []map[string]any
		for _, p := range asList(pathItem["parameters"]) {
			shared = append(shared, api.resolve(p))
		}
		for _, method := range []string{"get", "put", "post", "delete", "patch"} {
			node, ok := pathItem[method].(map[string]any)
			if !ok {
				continue
			}
			id, _ := node["operationId"].(string)
			params := slices.Clone(shared)
			for _, p := range asList(node["parameters"]) {
				params = append(params, api.resolve(p))
			}
			api.operations[id] = operationInfo{id: id, method: strings.ToUpper(method), path: path, node: node, params: params}
		}
	}
	return api
}

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

// resolve follows a local $ref, repeatedly.
func (api *openAPI) resolve(node any) map[string]any {
	m, _ := node.(map[string]any)
	for m != nil {
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		var target any = api.doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
			target = target.(map[string]any)[part]
		}
		m, _ = target.(map[string]any)
	}
	return m
}

// success is the operation's 2xx answer: its status and its JSON schema, nil
// when it has no body.
func (api *openAPI) success(op operationInfo) (int, map[string]any) {
	responses := op.node["responses"].(map[string]any)
	var codes []string
	for code := range responses {
		if strings.HasPrefix(code, "2") {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	if len(codes) == 0 {
		return 0, nil
	}
	var status int
	fmt.Sscanf(codes[0], "%d", &status)
	response := api.resolve(responses[codes[0]])
	content, _ := response["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	if media == nil {
		return status, nil
	}
	return status, api.resolve(media["schema"])
}

func (api *openAPI) requestSchema(op operationInfo) (schema map[string]any, required bool) {
	body := api.resolve(op.node["requestBody"])
	if body == nil {
		return nil, false
	}
	required, _ = body["required"].(bool)
	content, _ := body["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	return api.resolve(media["schema"]), required
}

// match returns the operations whose method and path template fit a request.
func (api *openAPI) match(method, escapedPath string) []string {
	segments := strings.Split(strings.Trim(escapedPath, "/"), "/")
	var hits []string
	for id, op := range api.operations {
		if op.method != method {
			continue
		}
		template := strings.Split(strings.Trim(op.path, "/"), "/")
		if len(template) != len(segments) {
			continue
		}
		fits := true
		for i, part := range template {
			if strings.HasPrefix(part, "{") {
				if segments[i] == "" {
					fits = false
				}
				continue
			}
			if part != segments[i] {
				fits = false
			}
		}
		if fits {
			hits = append(hits, id)
		}
	}
	sort.Strings(hits)
	return hits
}

// objectBranches flattens a schema into the object schemas it is made of:
// itself, every allOf branch, and the object branches of a oneOf or anyOf
// (the others are the null of a nullable member).
func (api *openAPI) objectBranches(schema map[string]any) []map[string]any {
	schema = api.resolve(schema)
	if schema == nil {
		return nil
	}
	var out []map[string]any
	if schema["properties"] != nil || schema["additionalProperties"] != nil || schema["type"] == "object" {
		out = append(out, schema)
	}
	for _, branch := range asList(schema["allOf"]) {
		out = append(out, api.objectBranches(api.resolve(branch))...)
	}
	for _, branch := range append(asList(schema["oneOf"]), asList(schema["anyOf"])...) {
		out = append(out, api.objectBranches(api.resolve(branch))...)
	}
	return out
}

// properties is the union of the declared members of an object schema's
// branches, and whether any branch admits members it does not declare.
func (api *openAPI) properties(schema map[string]any) (map[string]map[string]any, bool) {
	props := map[string]map[string]any{}
	open := false
	for _, branch := range api.objectBranches(schema) {
		if extra, ok := branch["additionalProperties"]; ok && extra != false {
			open = true
		}
		members, _ := branch["properties"].(map[string]any)
		for name, member := range members {
			props[name], _ = member.(map[string]any)
		}
	}
	return props, open
}

// nonNull is the schema with the null branch of a nullable member taken
// away, and whether there was one.
func (api *openAPI) nonNull(schema map[string]any) (map[string]any, bool) {
	schema = api.resolve(schema)
	branches := asList(schema["oneOf"])
	if len(branches) == 0 {
		return schema, false
	}
	var rest []map[string]any
	nullable := false
	for _, branch := range branches {
		b := api.resolve(branch)
		if b["type"] == "null" {
			nullable = true
			continue
		}
		rest = append(rest, b)
	}
	if nullable && len(rest) == 1 {
		return rest[0], true
	}
	return schema, nullable
}

// sentRequest is what the fake hub saw.
type sentRequest struct {
	method string
	path   string
	query  map[string][]string
	body   []byte
}

func TestEveryOperationHasAMethodAndEveryMethodAnOperation(t *testing.T) {
	api := loadOpenAPI(t)
	cases := operationCases()

	byOperation := map[string]bool{}
	byMethod := map[string]bool{}
	for _, c := range cases {
		if byOperation[c.operation] {
			t.Errorf("operation %s has two cases", c.operation)
		}
		byOperation[c.operation] = true
		byMethod[c.method] = true
		if _, ok := api.operations[c.operation]; !ok {
			t.Errorf("case %s names no operation in the document", c.operation)
		}
		if _, ok := reflect.TypeOf(&Client{}).MethodByName(c.method); !ok {
			t.Errorf("case %s names no method %s", c.operation, c.method)
		}
	}
	for id, op := range api.operations {
		if !byOperation[id] {
			t.Errorf("operation %s (%s %s) has no method in this client", id, op.method, op.path)
		}
	}
	clientType := reflect.TypeOf(&Client{})
	for i := range clientType.NumMethod() {
		name := clientType.Method(i).Name
		if !byMethod[name] && clientExtras[name] == "" {
			t.Errorf("method %s is no operation of the document; map it in operationCases or explain it in clientExtras", name)
		}
	}
}

func TestEveryRequestIsOneTheDocumentDeclares(t *testing.T) {
	api := loadOpenAPI(t)

	var mu sync.Mutex
	var sent []sentRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = append(sent, sentRequest{r.Method, r.URL.EscapedPath(), r.URL.Query(), body})
		mu.Unlock()
		// Answer with the operation's declared 2xx: the minimal JSON object
		// when it has a body, nothing when it has none.
		hits := api.match(r.Method, r.URL.EscapedPath())
		if len(hits) != 1 {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		status, schema := api.success(api.operations[hits[0]])
		if schema == nil {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	c, err := New(server.URL, "vya_openapi_test")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range operationCases() {
		op, ok := api.operations[tc.operation]
		if !ok {
			continue
		}
		mu.Lock()
		sent = nil
		mu.Unlock()
		if err := tc.call(context.Background(), c); err != nil {
			t.Errorf("%s: %v", tc.operation, err)
			continue
		}
		mu.Lock()
		got := sent
		mu.Unlock()
		if len(got) != 1 {
			t.Errorf("%s: %d requests sent, want 1", tc.operation, len(got))
			continue
		}
		request := got[0]
		if hits := api.match(request.method, request.path); !slices.Equal(hits, []string{tc.operation}) {
			t.Errorf("%s: %s %s resolves to %v in the document", tc.operation, request.method, request.path, hits)
		}

		declared := map[string]map[string]any{}
		for _, p := range op.params {
			if p["in"] == "query" {
				declared[p["name"].(string)] = p
			}
		}
		for name, values := range request.query {
			param, ok := declared[name]
			if !ok {
				t.Errorf("%s: sends query parameter %s, which the document does not declare", tc.operation, name)
				continue
			}
			for _, fault := range api.queryFaults(param, values) {
				t.Errorf("%s: query parameter %s %s", tc.operation, name, fault)
			}
		}
		for name := range declared {
			if _, ok := request.query[name]; !ok {
				t.Errorf("%s: the document declares query parameter %s, which this client cannot send", tc.operation, name)
			}
		}

		schema, required := api.requestSchema(op)
		switch {
		case schema == nil && len(request.body) > 0:
			t.Errorf("%s: sends a body, and the document declares none: %s", tc.operation, request.body)
		case schema != nil && len(request.body) == 0:
			if required {
				t.Errorf("%s: sends no body, and the document requires one", tc.operation)
			}
		case schema != nil:
			var body any
			if err := json.Unmarshal(request.body, &body); err != nil {
				t.Errorf("%s: the body is not JSON: %v", tc.operation, err)
				continue
			}
			for _, fault := range api.bodyFaults(schema, body, "") {
				t.Errorf("%s: %s", tc.operation, fault)
			}
		}
	}
}

// queryFaults checks what the client sent for one query parameter against
// the parameter: an array is sent the one way this client sends lists, one
// repetition of the parameter per item (style form, exploded, the
// defaults), within the array's bounds, and each item, like any other
// value, must satisfy its schema.
func (api *openAPI) queryFaults(param map[string]any, values []string) []string {
	schema := api.resolve(param["schema"])
	// A keyword this check does not grade is a fault rather than a pass: the
	// document could constrain a parameter in a way no test here would see.
	// A list's own keywords are checked here, and every single value's, a
	// list's items included, in queryValueFault.
	if schema["type"] == "array" {
		if fault := ungradedKeyword(schema, arrayKeywords); fault != "" {
			return []string{fault}
		}
	}
	if schema["type"] != "array" {
		if len(values) != 1 {
			return []string{fmt.Sprintf("is sent %d times, and the document declares one value", len(values))}
		}
		if fault := queryValueFault(schema, values[0]); fault != "" {
			return []string{fmt.Sprintf("%q %s", values[0], fault)}
		}
		return nil
	}
	var faults []string
	if style, ok := param["style"]; ok && style != "form" {
		faults = append(faults, fmt.Sprintf("has style %v, and this client sends a list as repeated parameters", style))
	}
	if explode, ok := param["explode"]; ok && explode != true {
		faults = append(faults, "is not exploded, and this client sends a list as repeated parameters")
	}
	for keyword, check := range map[string]func(float64) bool{
		"minItems": func(bound float64) bool { return float64(len(values)) >= bound },
		"maxItems": func(bound float64) bool { return float64(len(values)) <= bound },
	} {
		if raw, ok := schema[keyword]; ok {
			bound, isNumber := schemaNumber(raw)
			if !isNumber {
				faults = append(faults, fmt.Sprintf("has a %s of %v, which is not a number", keyword, raw))
			} else if !check(bound) {
				faults = append(faults, fmt.Sprintf("is sent %d times, outside its %s of %v", len(values), keyword, raw))
			}
		}
	}
	if schema["uniqueItems"] == true {
		seen := map[string]bool{}
		for _, value := range values {
			if seen[value] {
				faults = append(faults, fmt.Sprintf("repeats %q, and the document requires unique items", value))
			}
			seen[value] = true
		}
	}
	items := api.resolve(schema["items"])
	for _, value := range values {
		if fault := queryValueFault(items, value); fault != "" {
			faults = append(faults, fmt.Sprintf("%q %s", value, fault))
		}
	}
	return faults
}

// The schema keywords queryFaults and queryValueFault grade, for a list
// parameter and for a single value (a list's items included); the
// annotations carry no constraint.
var (
	arrayKeywords = map[string]bool{
		"type": true, "items": true, "minItems": true, "maxItems": true, "uniqueItems": true,
		"default": true, "description": true, "example": true, "examples": true,
	}
	scalarKeywords = map[string]bool{
		"type": true, "enum": true, "format": true, "pattern": true, "minLength": true, "maxLength": true,
		"minimum": true, "maximum": true, "default": true, "description": true, "example": true, "examples": true,
	}
)

// schemaNumber reads a numeric schema keyword, which YAML decodes as an int
// or a float64 depending on how the document spells it.
func schemaNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func ungradedKeyword(schema map[string]any, graded map[string]bool) string {
	for keyword := range schema {
		if !graded[keyword] {
			return fmt.Sprintf("has a schema using %s, which this check does not grade; teach it", keyword)
		}
	}
	return ""
}

// exactNumber is schemaNumber without the rounding: the keyword as an exact
// rational, from an integer exactly and from a float64 at its exact value.
func exactNumber(v any) (*big.Rat, bool) {
	switch n := v.(type) {
	case int:
		return new(big.Rat).SetInt64(int64(n)), true
	case int64:
		return new(big.Rat).SetInt64(n), true
	case uint64:
		return new(big.Rat).SetUint64(n), true
	case float64:
		if r := new(big.Rat).SetFloat64(n); r != nil {
			return r, true
		}
	}
	return nil, false
}

// queryValueFault checks one query value against its parameter's schema:
// the type it must parse as, and the enum, format, pattern, and bounds the
// document gives it. Empty means the value is acceptable.
func queryValueFault(schema map[string]any, value string) string {
	if fault := ungradedKeyword(schema, scalarKeywords); fault != "" {
		return fault
	}
	if enum := asList(schema["enum"]); len(enum) > 0 {
		if !slices.ContainsFunc(enum, func(member any) bool { return fmt.Sprint(member) == value }) {
			return fmt.Sprintf("is none of %v", enum)
		}
	}
	switch schema["type"] {
	case "integer":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return "is not an integer"
		}
		// Compared as exact rationals: a float64 of the value would round
		// anything past 2^53 onto a neighbouring bound.
		value := new(big.Rat).SetInt64(n)
		for keyword, inside := range map[string]func(int) bool{
			"minimum": func(cmp int) bool { return cmp >= 0 },
			"maximum": func(cmp int) bool { return cmp <= 0 },
		} {
			raw, ok := schema[keyword]
			if !ok {
				continue
			}
			bound, isNumber := exactNumber(raw)
			if !isNumber {
				return fmt.Sprintf("has a %s of %v, which is not a number", keyword, raw)
			}
			if !inside(value.Cmp(bound)) {
				return fmt.Sprintf("is outside its %s of %v", keyword, raw)
			}
		}
	case "boolean":
		if value != "true" && value != "false" {
			return "is not true or false"
		}
	case "string":
		if schema["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				return "is not an RFC 3339 date-time"
			}
		}
		if pattern, ok := schema["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(value) {
			return "does not match " + pattern
		}
		length := float64(utf8.RuneCountInString(value))
		if raw, ok := schema["minLength"]; ok {
			if bound, isNumber := schemaNumber(raw); !isNumber || length < bound {
				return fmt.Sprintf("is outside its minLength of %v", raw)
			}
		}
		if raw, ok := schema["maxLength"]; ok {
			if bound, isNumber := schemaNumber(raw); !isNumber || length > bound {
				return fmt.Sprintf("is outside its maxLength of %v", raw)
			}
		}
	default:
		return fmt.Sprintf("has a schema of type %v, which this check does not know", schema["type"])
	}
	return ""
}

// bodyFaults compares a request body with its schema both ways: a member the
// schema does not declare is a fault, and so is a declared member the body
// lacks, because every case sets every option.
func (api *openAPI) bodyFaults(schema map[string]any, value any, path string) []string {
	schema, _ = api.nonNull(schema)
	var faults []string
	switch v := value.(type) {
	case map[string]any:
		props, open := api.properties(schema)
		if len(props) == 0 {
			return nil
		}
		for name, member := range v {
			prop, ok := props[name]
			if !ok {
				if !open {
					faults = append(faults, fmt.Sprintf("sends %s%s, which the document does not declare", path, "."+name))
				}
				continue
			}
			faults = append(faults, api.bodyFaults(prop, member, path+"."+name)...)
		}
		for name := range props {
			if _, ok := v[name]; !ok {
				faults = append(faults, fmt.Sprintf("the document declares %s.%s, which this client cannot send", path, name))
			}
		}
	case []any:
		for i, element := range v {
			faults = append(faults, api.bodyFaults(api.resolve(schema["items"]), element, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return faults
}

func TestRecordsMatchTheirSchemas(t *testing.T) {
	api := loadOpenAPI(t)
	clientType := reflect.TypeOf(&Client{})
	for _, tc := range operationCases() {
		op, ok := api.operations[tc.operation]
		if !ok {
			continue
		}
		method, ok := clientType.MethodByName(tc.method)
		if !ok {
			continue
		}
		fn := method.Type

		_, answer := api.success(op)
		if tc.answer != "" && answer != nil {
			props, _ := api.properties(answer)
			answer = props[tc.answer]
			if answer == nil {
				t.Errorf("%s: the 2xx body declares no member %s", tc.operation, tc.answer)
				continue
			}
		}
		switch {
		case fn.NumOut() == 2 && answer == nil:
			t.Errorf("%s: %s returns %s, and the document declares no body", tc.operation, tc.method, fn.Out(0))
		case fn.NumOut() == 1 && answer != nil:
			t.Errorf("%s: %s returns no record, and the document declares a body", tc.operation, tc.method)
		case fn.NumOut() == 2:
			for _, fault := range api.typeFaults(tc.operation, fn.Out(0), answer, fn.Out(0).Name(), "", false) {
				t.Errorf("%s answer: %s", tc.operation, fault)
			}
		}

		// The request record, when the method takes one: a struct argument.
		schema, _ := api.requestSchema(op)
		for i := 1; i < fn.NumIn(); i++ {
			in := fn.In(i)
			if in.Kind() != reflect.Struct || in == reflect.TypeOf((*context.Context)(nil)).Elem() {
				continue
			}
			if strings.HasSuffix(in.Name(), "Query") {
				continue // query options, graded on the wire
			}
			if schema == nil {
				t.Errorf("%s: %s takes %s, and the document declares no body", tc.operation, tc.method, in)
				continue
			}
			for _, fault := range api.typeFaults(tc.operation, in, schema, in.Name(), "", true) {
				t.Errorf("%s request: %s", tc.operation, fault)
			}
		}
	}
}

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage(nil))
)

// typeFaults compares a Go type with a schema: JSON types, members both
// ways, and nullability. operation and owner (the struct a member belongs
// to) are what recordExceptions are keyed by.
func (api *openAPI) typeFaults(operation string, goType reflect.Type, schema map[string]any, owner, path string, request bool) []string {
	schema, nullable := api.nonNull(schema)
	at := path
	if at == "" {
		at = goType.String()
	}
	canHoldNull := false
	for goType.Kind() == reflect.Pointer {
		goType = goType.Elem()
		canHoldNull = true
	}
	if goType == rawType || goType.Kind() == reflect.Interface {
		return nil
	}
	switch goType.Kind() {
	case reflect.Slice, reflect.Map:
		canHoldNull = true
	}
	var faults []string
	if nullable && !canHoldNull && !request {
		name := strings.TrimPrefix(path, ".")
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		if !excepted(operation, owner, name) {
			faults = append(faults, fmt.Sprintf("%s is nullable, and %s cannot hold null", at, goType))
		}
	}

	kind, _ := schema["type"].(string)
	if goType == timeType {
		if kind != "string" || schema["format"] != "date-time" {
			faults = append(faults, fmt.Sprintf("%s is a time, and the schema is %v %v", at, kind, schema["format"]))
		}
		return faults
	}
	switch goType.Kind() {
	case reflect.String:
		if kind != "string" {
			faults = append(faults, fmt.Sprintf("%s is a string, and the schema is %q", at, kind))
		}
	case reflect.Bool:
		if kind != "boolean" {
			faults = append(faults, fmt.Sprintf("%s is a bool, and the schema is %q", at, kind))
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if kind != "integer" {
			faults = append(faults, fmt.Sprintf("%s is an integer, and the schema is %q", at, kind))
		}
	case reflect.Float32, reflect.Float64:
		if kind != "number" {
			faults = append(faults, fmt.Sprintf("%s is a number, and the schema is %q", at, kind))
		}
	case reflect.Slice:
		if kind != "array" {
			faults = append(faults, fmt.Sprintf("%s is a list, and the schema is %q", at, kind))
			break
		}
		faults = append(faults, api.typeFaults(operation, goType.Elem(), api.resolve(schema["items"]), owner, path+"[]", request)...)
	case reflect.Map:
		if extra, ok := schema["additionalProperties"]; !ok || extra == false {
			faults = append(faults, fmt.Sprintf("%s is a map, and the schema is not an open object", at))
		}
	case reflect.Struct:
		faults = append(faults, api.structFaults(operation, goType, schema, path, request)...)
	default:
		faults = append(faults, fmt.Sprintf("%s has kind %s, which this check does not know", at, goType.Kind()))
	}
	return faults
}

func (api *openAPI) structFaults(operation string, goType reflect.Type, schema map[string]any, path string, request bool) []string {
	props, open := api.properties(schema)
	if len(props) == 0 {
		if open {
			return nil // an open object the document leaves to another schema (a manifest body, say)
		}
		return []string{fmt.Sprintf("%s is a struct, and the schema declares no members", goType)}
	}
	var faults []string
	fields := map[string]reflect.StructField{}
	for i := range goType.NumField() {
		field := goType.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field
	}
	for name, field := range fields {
		prop, ok := props[name]
		if !ok {
			if !excepted(operation, goType.Name(), name) {
				faults = append(faults, fmt.Sprintf("%s.%s (%s) is no member of the schema", goType.Name(), field.Name, name))
			}
			continue
		}
		faults = append(faults, api.typeFaults(operation, field.Type, prop, goType.Name(), path+"."+name, request)...)
	}
	for name := range props {
		if _, ok := fields[name]; !ok && !excepted(operation, goType.Name(), name) {
			faults = append(faults, fmt.Sprintf("%s has no field for the schema's member %s", goType.Name(), name))
		}
	}
	sort.Strings(faults)
	return faults
}

// Integer bounds compare exactly, past where a float64 can tell neighbours
// apart, and a bound spelled as a float still bounds.
func TestQueryValueBoundsAreExact(t *testing.T) {
	for _, tc := range []struct {
		schema map[string]any
		value  string
		fault  bool
	}{
		{map[string]any{"type": "integer", "maximum": 9007199254740992}, "9007199254740993", true},
		{map[string]any{"type": "integer", "maximum": 9007199254740992}, "9007199254740992", false},
		{map[string]any{"type": "integer", "minimum": -9007199254740992}, "-9007199254740993", true},
		{map[string]any{"type": "integer", "minimum": 11.0}, "10", true},
		{map[string]any{"type": "integer", "minimum": 10.5}, "11", false},
	} {
		if got := queryValueFault(tc.schema, tc.value) != ""; got != tc.fault {
			t.Errorf("%v against %v: fault %v, want %v", tc.value, tc.schema, got, tc.fault)
		}
	}
}
