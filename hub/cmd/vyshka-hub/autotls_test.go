package main

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/acme"
)

// Flag-combination validation is the one part of --auto-tls end to end
// cannot reach cheaply: every refusal would be a separate boot. The issuance,
// the listeners, and the failure modes are in scripts/demo-auto-tls.sh.
func TestPlanAutoTLSRefusesIncoherentCombinations(t *testing.T) {
	cases := []struct {
		name    string
		flags   autoTLSFlags
		dsn     string
		wantErr string
	}{
		{"hosts without the switch", autoTLSFlags{hosts: "hub.example.org"}, "", "-auto-tls-hosts set without -auto-tls"},
		{"several stray flags named together", autoTLSFlags{email: "a@b.c", httpAddr: "off"}, "",
			"-auto-tls-email, -auto-tls-http-addr set without -auto-tls"},
		{"switch without hosts", autoTLSFlags{enabled: true}, "", "needs -auto-tls-hosts"},
		{"only commas", autoTLSFlags{enabled: true, hosts: " , ,"}, "", "needs -auto-tls-hosts"},
		{"wildcard", autoTLSFlags{enabled: true, hosts: "*.example.org"}, "", "wildcard"},
		{"IP address", autoTLSFlags{enabled: true, hosts: "203.0.113.7"}, "", "IP address"},
		{"port", autoTLSFlags{enabled: true, hosts: "hub.example.org:443"}, "", "port"},
		{"single label", autoTLSFlags{enabled: true, hosts: "localhost"}, "", "fully qualified"},
		{"bad character", autoTLSFlags{enabled: true, hosts: "hub_1.example.org"}, "", "character"},
		{"hyphen edge", autoTLSFlags{enabled: true, hosts: "-hub.example.org"}, "", "hyphen"},
		{"plain directory", autoTLSFlags{enabled: true, hosts: "hub.example.org", directory: "http://ca.example/dir"}, "", "https://"},
		{"not an email", autoTLSFlags{enabled: true, hosts: "hub.example.org", email: "ops"}, "", "not an email"},
		{"bad http addr", autoTLSFlags{enabled: true, hosts: "hub.example.org", httpAddr: "80"}, "", "-auto-tls-http-addr"},
		{"http addr on the TLS port", autoTLSFlags{enabled: true, hosts: "hub.example.org", httpAddr: ":443"}, "", "same port"},
		{"postgres without a cache", autoTLSFlags{enabled: true, hosts: "hub.example.org"},
			"postgres://vyshka@db/vyshka", "needs -auto-tls-cache"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := planAutoTLS(c.flags, "127.0.0.1:8080", false, c.dsn)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

func TestPlanAutoTLSDefaults(t *testing.T) {
	// Off: nothing changes, -addr stays as given.
	plan, addr, err := planAutoTLS(autoTLSFlags{}, "127.0.0.1:8080", false, "")
	if err != nil || plan != nil || addr != "127.0.0.1:8080" {
		t.Fatalf("off: plan %v addr %q err %v", plan, addr, err)
	}

	// On with only hosts: :443, a plain :80, Let's Encrypt production, the
	// cache beside the SQLite file, and the names normalized and deduplicated.
	plan, addr, err = planAutoTLS(autoTLSFlags{enabled: true, hosts: "Hub.Example.org., hub.example.org,alt.example.org"},
		"127.0.0.1:8080", false, filepath.Join("data", "vyshka.db"))
	if err != nil {
		t.Fatal(err)
	}
	if addr != ":443" || plan.httpAddr != ":80" || plan.tlsPort != "443" || plan.directory != acme.LetsEncryptURL {
		t.Errorf("defaults: addr %q http %q port %q directory %q", addr, plan.httpAddr, plan.tlsPort, plan.directory)
	}
	if want := filepath.Join("data", autoTLSCacheName); plan.cacheDir != want {
		t.Errorf("cache %q, want %q", plan.cacheDir, want)
	}
	if strings.Join(plan.hosts, ",") != "hub.example.org,alt.example.org" {
		t.Errorf("hosts %v", plan.hosts)
	}

	// An -addr the operator gave is kept; "off" drops the plain listener;
	// Postgres works with an explicit cache.
	plan, addr, err = planAutoTLS(autoTLSFlags{enabled: true, hosts: "hub.example.org", httpAddr: "off", cacheDir: "/var/cache/x"},
		"0.0.0.0:8443", true, "postgres://vyshka@db/vyshka")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "0.0.0.0:8443" || plan.httpAddr != "" || plan.tlsPort != "8443" || plan.cacheDir != "/var/cache/x" {
		t.Errorf("explicit: addr %q http %q port %q cache %q", addr, plan.httpAddr, plan.tlsPort, plan.cacheDir)
	}
}
