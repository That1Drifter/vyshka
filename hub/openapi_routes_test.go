package hub_test

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Every Admin API route the hub serves is an operation of
// spec/openapi-admin.yaml, and every operation there is a route here. The
// clients are generated from (TypeScript) or held to (Go) that document, so a
// route it lacks is a route no client can reach, which is how four of them
// went undocumented until the clients were built.
//
// The routes are read from the source of routes() rather than from the mux,
// which does not enumerate its patterns: each is one mux.HandleFunc call
// with a method in its pattern, and the method-less registrations beside
// them only turn a mismatch into a 405.
func TestAdminRoutesAreTheOpenAPIOperations(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, match := range regexp.MustCompile(`mux\.HandleFunc\("(GET|PUT|POST|DELETE|PATCH) (/api/v1[^"]*)"`).
		FindAllSubmatch(source, -1) {
		routes = append(routes, string(match[1])+" "+string(match[2]))
	}
	if len(routes) < 30 {
		t.Fatalf("found %d Admin API routes in server.go; the pattern no longer matches how routes are registered", len(routes))
	}

	raw, err := os.ReadFile("../spec/openapi-admin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var operations []string
	for path, item := range doc.Paths {
		for method := range item {
			if slices.Contains([]string{"get", "put", "post", "delete", "patch"}, method) {
				operations = append(operations, strings.ToUpper(method)+" "+path)
			}
		}
	}

	sort.Strings(routes)
	sort.Strings(operations)
	for _, route := range routes {
		if !slices.Contains(operations, route) {
			t.Errorf("the hub serves %s, which spec/openapi-admin.yaml does not declare", route)
		}
	}
	for _, operation := range operations {
		if !slices.Contains(routes, operation) {
			t.Errorf("spec/openapi-admin.yaml declares %s, which the hub does not serve", operation)
		}
	}
}
