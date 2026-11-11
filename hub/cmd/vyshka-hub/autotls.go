package main

// Embedded ACME (--auto-tls): the hub obtains and renews its own certificate
// and terminates TLS itself, so a reverse proxy becomes optional.
//
// Challenges. TLS-ALPN-01 is answered on the TLS listener itself, so a hub
// reachable on 443 needs nothing else. A plain listener (default :80, "off"
// disables it) adds HTTP-01 as a second way to validate and redirects
// browsers to https; it serves the ACME challenge path and the redirect and
// nothing else, never the Plugin or Admin API.
//
// Host policy. The hub asks for certificates only for the names in
// -auto-tls-hosts. A handshake for any other name (or none) fails before an
// order is placed, whatever SNI or Host a client sends.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/net/netutil"

	"github.com/That1Drifter/vyshka/hub"
	"github.com/That1Drifter/vyshka/hub/store"
)

// autoTLSFlags is what the -auto-tls* flags and their environment variables
// said, before any validation.
type autoTLSFlags struct {
	enabled   bool
	hosts     string
	cacheDir  string
	email     string
	directory string
	caRoots   string
	httpAddr  string
}

// autoTLSPlan is a validated, coherent auto-TLS configuration.
type autoTLSPlan struct {
	hosts     []string
	cacheDir  string
	email     string
	directory string
	caRoots   string
	// httpAddr is the plain listener for HTTP-01 and the redirect; empty
	// means none.
	httpAddr string
	// tlsPort is the TLS listener's port, which the redirect points at.
	tlsPort string
}

const (
	defaultTLSAddr      = ":443"
	defaultAutoHTTPAddr = ":80"
	// autoTLSCacheName is the cache directory's name beside a SQLite file.
	autoTLSCacheName = "acme-cache"
)

// planAutoTLS checks the flag combination and settles the defaults. It
// returns a nil plan when auto-TLS is off, and the address the hub's main
// listener should use either way: with auto-TLS on and no -addr given (flag
// or VYSHKA_ADDR), that is :443 instead of the plain default.
func planAutoTLS(f autoTLSFlags, addr string, addrGiven bool, dsn string) (*autoTLSPlan, string, error) {
	if !f.enabled {
		var stray []string
		for _, set := range []struct{ name, value string }{
			{"-auto-tls-hosts", f.hosts},
			{"-auto-tls-cache", f.cacheDir},
			{"-auto-tls-email", f.email},
			{"-auto-tls-directory", f.directory},
			{"-auto-tls-ca-roots", f.caRoots},
			{"-auto-tls-http-addr", f.httpAddr},
		} {
			if strings.TrimSpace(set.value) != "" {
				stray = append(stray, set.name)
			}
		}
		if len(stray) > 0 {
			return nil, "", fmt.Errorf("%s set without -auto-tls (env VYSHKA_AUTO_TLS): "+
				"turn it on, or remove them; the hub would otherwise serve plain HTTP", strings.Join(stray, ", "))
		}
		return nil, addr, nil
	}

	hosts, err := parseTLSHosts(f.hosts)
	if err != nil {
		return nil, "", err
	}
	plan := &autoTLSPlan{hosts: hosts, email: strings.TrimSpace(f.email), caRoots: f.caRoots}
	if plan.email != "" && !strings.Contains(plan.email, "@") {
		return nil, "", fmt.Errorf("-auto-tls-email %q is not an email address", plan.email)
	}

	plan.directory = strings.TrimSpace(f.directory)
	if plan.directory == "" {
		plan.directory = acme.LetsEncryptURL
	}
	if !strings.HasPrefix(plan.directory, "https://") {
		return nil, "", fmt.Errorf("-auto-tls-directory %q must be an https:// URL", plan.directory)
	}

	if !addrGiven {
		addr = defaultTLSAddr
	}
	tlsHost, tlsPort, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("-addr %q: %w", addr, err)
	}
	plan.tlsPort = tlsPort

	switch httpAddr := strings.TrimSpace(f.httpAddr); httpAddr {
	case "off":
		plan.httpAddr = ""
	case "":
		plan.httpAddr = defaultAutoHTTPAddr
	default:
		plan.httpAddr = httpAddr
	}
	if plan.httpAddr != "" {
		httpHost, httpPort, err := net.SplitHostPort(plan.httpAddr)
		if err != nil {
			return nil, "", fmt.Errorf("-auto-tls-http-addr %q: %w (\"off\" disables the plain listener)", plan.httpAddr, err)
		}
		if httpPort == tlsPort && (httpHost == tlsHost || httpHost == "" || tlsHost == "") {
			return nil, "", fmt.Errorf("-auto-tls-http-addr %s and -addr %s are the same port", plan.httpAddr, addr)
		}
	}

	plan.cacheDir = strings.TrimSpace(f.cacheDir)
	if plan.cacheDir == "" {
		dir, ok := sqliteDir(dsn)
		if !ok {
			return nil, "", errors.New("-auto-tls with a Postgres database needs -auto-tls-cache " +
				"(env VYSHKA_AUTO_TLS_CACHE): there is no local database file to keep the certificates beside")
		}
		plan.cacheDir = filepath.Join(dir, autoTLSCacheName)
	}
	return plan, addr, nil
}

// parseTLSHosts reads the comma-separated host list into the names a CA can
// validate over HTTP-01 or TLS-ALPN-01: fully qualified DNS names, no
// wildcards (those need DNS-01), no IP addresses, no ports.
func parseTLSHosts(raw string) ([]string, error) {
	var hosts []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(part)), ".")
		if host == "" {
			continue
		}
		if err := checkTLSHost(host); err != nil {
			return nil, fmt.Errorf("-auto-tls-hosts: %q %w", host, err)
		}
		if !seen[host] {
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	if len(hosts) == 0 {
		return nil, errors.New("-auto-tls needs -auto-tls-hosts (env VYSHKA_AUTO_TLS_HOSTS): " +
			"the hub requests certificates for those names and no others")
	}
	return hosts, nil
}

func checkTLSHost(host string) error {
	switch {
	case strings.Contains(host, "*"):
		return errors.New("is a wildcard; the hub validates over HTTP-01 and TLS-ALPN-01, which cannot issue one")
	case net.ParseIP(host) != nil:
		return errors.New("is an IP address; give the DNS name clients connect to")
	case strings.Contains(host, ":"):
		return errors.New("carries a port; the port is -addr's")
	case !strings.Contains(host, "."):
		return errors.New("is not a fully qualified name")
	case len(host) > 253:
		return errors.New("is longer than a DNS name may be")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return errors.New("has an empty or overlong label")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return errors.New("has a character a DNS name may not (give an internationalized name in its xn-- form)")
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("has a label that starts or ends with a hyphen")
		}
	}
	return nil
}

// sqliteDir is the directory a SQLite DSN keeps its file in, mirroring the
// store's DSN forms; ok is false for anything else (Postgres).
func sqliteDir(dsn string) (string, bool) {
	switch {
	case dsn == "":
		return filepath.Dir(store.DefaultSQLitePath), true
	case strings.HasPrefix(dsn, "sqlite://"):
		return filepath.Dir(strings.TrimPrefix(dsn, "sqlite://")), true
	case strings.Contains(dsn, "://"):
		return "", false
	default:
		return filepath.Dir(dsn), true
	}
}

// autoTLS is the running side of a plan: the ACME manager and the plain
// listener.
type autoTLS struct {
	plan    *autoTLSPlan
	manager *autocert.Manager
	log     *slog.Logger
	allowed map[string]bool
	// certs holds each configured name's current certificate, as the
	// watcher last fetched it. Handshakes answer from it alone.
	certs sync.Map // string -> *tls.Certificate
}

// plainListenerMaxConns caps the plain listener's open connections. It
// serves challenge tokens and redirects, so a handful is plenty; the cap
// keeps idle connections there from eating the file descriptors and memory
// the TLS listener needs.
const plainListenerMaxConns = 64

// prepareAutoTLS readies the cache directory and the ACME client, failing
// the boot on anything an operator must fix (an unwritable cache, an
// unreadable root file). It contacts nobody: the CA is first reached when
// the certificate is first wanted.
func prepareAutoTLS(plan *autoTLSPlan, logger *slog.Logger) (*autoTLS, *tls.Config, error) {
	if err := os.MkdirAll(plan.cacheDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("auto-tls cache %s: %w", plan.cacheDir, err)
	}
	probe, err := os.CreateTemp(plan.cacheDir, ".write-probe-*")
	if err != nil {
		return nil, nil, fmt.Errorf("auto-tls cache %s is not writable: %w", plan.cacheDir, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	if info, err := os.Stat(plan.cacheDir); err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		logger.Warn("auto-tls cache directory is open to other users; it holds the ACME account key and certificate keys",
			"dir", plan.cacheDir, "mode", info.Mode().Perm().String())
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if plan.caRoots != "" {
		pem, err := os.ReadFile(plan.caRoots)
		if err != nil {
			return nil, nil, fmt.Errorf("auto-tls CA roots: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("auto-tls CA roots: %s holds no PEM certificate", plan.caRoots)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	manager := &autocert.Manager{
		// Turning -auto-tls on is the operator accepting the CA's terms.
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(plan.cacheDir),
		HostPolicy: autocert.HostWhitelist(plan.hosts...),
		Email:      plan.email,
		Client: &acme.Client{
			DirectoryURL: plan.directory,
			UserAgent:    "vyshka-hub/" + hub.Version,
			HTTPClient:   &http.Client{Transport: transport},
		},
	}
	a := &autoTLS{plan: plan, manager: manager, log: logger, allowed: make(map[string]bool, len(plan.hosts))}
	for _, host := range plan.hosts {
		a.allowed[host] = true
	}
	tlsConfig := &tls.Config{
		GetCertificate: a.getCertificate,
		// h2 is left out on purpose; hub.Serve strips it in any case.
		NextProtos: []string{"http/1.1", acme.ALPNProto},
		MinVersion: tls.VersionTLS12,
	}
	httpAddr := plan.httpAddr
	if httpAddr == "" {
		httpAddr = "off"
	}
	logger.Info("auto-tls enabled", "hosts", plan.hosts, "directory", plan.directory,
		"cache", plan.cacheDir, "http_addr", httpAddr)
	return a, tlsConfig, nil
}

// getCertificate answers a handshake without ever ordering or waiting on an
// order. A TLS-ALPN-01 validation hello (acme-tls/1 alone) goes to the
// manager, which answers it from its challenge tokens and never orders;
// every other hello gets the configured name's certificate from memory, so
// it is the same ECDSA certificate whatever the client offers, and a
// handshake cannot park a connection behind a slow CA. Ordering is the
// watcher's alone (at boot, and on its backoff after a failure).
func (a *autoTLS) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == acme.ALPNProto {
		return a.manager.GetCertificate(hello)
	}
	name := strings.TrimSuffix(strings.ToLower(hello.ServerName), ".")
	if !a.allowed[name] {
		return nil, fmt.Errorf("auto-tls: host %q is not in -auto-tls-hosts", hello.ServerName)
	}
	cert, ok := a.certs.Load(name)
	if !ok {
		return nil, fmt.Errorf("auto-tls: no certificate for %q yet", name)
	}
	return cert.(*tls.Certificate), nil
}

// start opens the plain listener, if any, and the certificate watch. The
// listener is bound before start returns so a port conflict fails the boot;
// both stop when ctx is cancelled. The returned function closes the plain
// listener at once, for a boot that fails after start.
func (a *autoTLS) start(ctx context.Context) (func(), error) {
	go a.watch(ctx)
	if a.plan.httpAddr == "" {
		return func() {}, nil
	}
	listener, err := net.Listen("tcp", a.plan.httpAddr)
	if err != nil {
		return nil, fmt.Errorf("auto-tls plain listener on %s: %w", a.plan.httpAddr, err)
	}
	listener = netutil.LimitListener(listener, plainListenerMaxConns)
	challenge := a.manager.HTTPHandler(redirectToTLS(a.plan.hosts, a.plan.tlsPort))
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// An ACME token is a short base64url string. Anything else never
			// reaches the cache, whose file errors would name its path.
			if token, ok := strings.CutPrefix(r.URL.Path, "/.well-known/acme-challenge/"); ok &&
				(token == "" || len(token) > 128 || strings.Trim(token, base64URLAlphabet) != "") {
				http.NotFound(w, r)
				return
			}
			challenge.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}
	go func() {
		a.log.Info("auto-tls plain listener (ACME HTTP-01 and the https redirect only)", "addr", listener.Addr().String())
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("auto-tls plain listener stopped", "error", err.Error())
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	return func() { server.Close() }, nil
}

const base64URLAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// redirectToTLS answers everything on the plain listener that is not an
// ACME challenge: a GET or HEAD with an origin-form target (a path from /)
// for a configured name is sent to https on the TLS port, anything else is
// refused. The Host header picks the redirect's name only from the
// configured list, and a target that is not a path (opaque, or not starting
// with /) is refused rather than appended, so the listener cannot be used
// to bounce a client to an arbitrary site.
func redirectToTLS(hosts []string, tlsPort string) http.Handler {
	allowed := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		allowed[host] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		if !allowed[host] {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "this hub serves its APIs over https only", http.StatusBadRequest)
			return
		}
		if r.URL.Opaque != "" || !strings.HasPrefix(r.URL.Path, "/") {
			http.Error(w, "the request target must be a path", http.StatusBadRequest)
			return
		}
		target := "https://" + host
		if tlsPort != "443" {
			target += ":" + tlsPort
		}
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusFound)
	})
}

// watch obtains each configured name's certificate at boot and then keeps
// an eye on it, so the log says when a certificate is ready, renewed,
// overdue for renewal, or unobtainable. Apart from the manager's own
// renewal timer it is the only place an order starts: handshakes answer
// from what it stores. The manager renews on its own (from two thirds of
// the lifetime on) and keeps the cached certificate while renewal fails; a
// renewed one reaches handshakes at the watcher's next check, at most an
// hour later and long before the old one expires.
func (a *autoTLS) watch(ctx context.Context) {
	for _, host := range a.plan.hosts {
		go a.watchHost(ctx, host)
	}
}

// After a failure the watcher waits certRetryFirst, doubling up to
// certRetryMax, and starts over on success: each attempt can be a real
// order, and a CA counts failed validations (and, when the clock is wrong,
// issued certificates) against per-name limits.
const (
	certRetryFirst = 5 * time.Minute
	certRetryMax   = time.Hour
	certCheckMin   = 10 * time.Second
	certCheckMax   = time.Hour
)

func (a *autoTLS) watchHost(ctx context.Context, host string) {
	var current *x509.Certificate
	failWait := certRetryFirst
	for {
		var wait time.Duration
		cert, err := a.manager.GetCertificate(ecdsaHello(host))
		if err == nil && cert.Leaf == nil {
			err = errors.New("the certificate has no parsed leaf")
		}
		switch {
		case err != nil:
			wait = failWait
			failWait = min(failWait*2, certRetryMax)
			a.log.Error("tls certificate unavailable; handshakes for this name fail until one is obtained",
				"host", host, "error", err.Error(), "retry_in", wait.String())
		default:
			failWait = certRetryFirst
			a.certs.Store(host, cert)
			leaf := cert.Leaf
			if current == nil || leaf.SerialNumber.Cmp(current.SerialNumber) != 0 {
				msg := "tls certificate ready"
				if current != nil {
					msg = "tls certificate renewed"
				}
				a.log.Info(msg, "host", host, "serial", serialHex(leaf.SerialNumber),
					"issuer", leaf.Issuer.CommonName, "not_after", leaf.NotAfter.UTC().Format(time.RFC3339))
				current = leaf
			}
			lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
			threshold := min(lifetime/3, 30*24*time.Hour)
			remaining := time.Until(leaf.NotAfter)
			// The manager starts renewing at the threshold, with up to a
			// tenth of jitter; past four fifths of it, renewal has failed.
			if remaining < threshold*4/5 {
				a.log.Warn("tls certificate renewal overdue; still serving the current certificate until it expires",
					"host", host, "not_after", leaf.NotAfter.UTC().Format(time.RFC3339),
					"expires_in", remaining.Round(time.Second).String())
			}
			wait = min(max(lifetime/24, certCheckMin), certCheckMax)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ecdsaHello is the ClientHello the watch asks the manager with. It must
// advertise ECDSA, or the manager would order an RSA certificate that real
// clients (which all offer ECDSA) never use.
func ecdsaHello(host string) *tls.ClientHelloInfo {
	return &tls.ClientHelloInfo{
		ServerName:       host,
		SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
		SupportedCurves:  []tls.CurveID{tls.CurveP256},
		CipherSuites:     []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
	}
}

func serialHex(n *big.Int) string {
	return fmt.Sprintf("%x", n)
}
