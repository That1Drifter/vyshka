package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A base URL whose last segment ends in an encoded slash keeps that segment:
// only a literal trailing slash is a separator to drop.
func TestEncodedSlashInThePrefixIsKept(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"servers":[]}`))
	}))
	defer server.Close()
	for base, want := range map[string]string{
		server.URL + "/tenant%2F": "/tenant%2F/api/v1/servers",
		server.URL + "/tenant/":   "/tenant/api/v1/servers",
		server.URL + "/tenant":    "/tenant/api/v1/servers",
		server.URL:                "/api/v1/servers",
	} {
		c, err := New(base, "vya_test")
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if _, err := c.ListServers(context.Background()); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if got != want {
			t.Errorf("base %s sent %s, want %s", base, got, want)
		}
	}
}

// The list answers keep the hub's whole object, members this client does not
// model included, so a caller can print what the hub sent.
func TestListAnswersKeepTheHubsObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/servers"):
			w.Write([]byte(`{"servers":[{"id":"s1","name":"one","extra":true}],"total":1}`))
		case strings.HasSuffix(r.URL.Path, "/kv"):
			w.Write([]byte(`{"namespaces":[{"namespace":"n","keys":1,"bytes":42}],"total":1}`))
		default:
			w.Write([]byte(`{"snapshots":[{"type":"players","capturedAt":"2026-09-29T00:00:00Z",` +
				`"receivedAt":"2026-09-29T00:00:00Z","snapshot":{"players":[]}}],"more":false}`))
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "vya_test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	servers, err := c.ListServers(ctx)
	if err != nil || len(servers.Servers) != 1 || servers.Servers[0].ID != "s1" {
		t.Fatalf("ListServers = %+v, %v", servers, err)
	}
	if !strings.Contains(string(servers.Raw), `"total":1`) || !strings.Contains(string(servers.Servers[0].Raw), `"extra":true`) {
		t.Errorf("ListServers dropped members: %s", servers.Raw)
	}

	namespaces, err := c.KVListNamespaces(ctx)
	if err != nil || len(namespaces.Namespaces) != 1 || namespaces.Namespaces[0].Keys != 1 {
		t.Fatalf("KVListNamespaces = %+v, %v", namespaces, err)
	}
	if !strings.Contains(string(namespaces.Raw), `"total":1`) || !strings.Contains(string(namespaces.Namespaces[0].Raw), `"bytes":42`) {
		t.Errorf("KVListNamespaces dropped members: %s", namespaces.Raw)
	}

	history, err := c.StateHistory(ctx, "s1", "players", 0)
	if err != nil || len(history.Snapshots) != 1 || history.Snapshots[0].Type != "players" {
		t.Fatalf("StateHistory = %+v, %v", history, err)
	}
	if !strings.Contains(string(history.Raw), `"more":false`) {
		t.Errorf("StateHistory dropped members: %s", history.Raw)
	}
}
