package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var out bytes.Buffer
	r.Write(context.Background(), &out)
	return out.String()
}

// The exposition format is the contract with Prometheus, and the end-to-end
// demo cannot reach its corners (escaping, bucket edges, special values), so
// this pins the whole output of a small registry byte for byte.
func TestExpositionFormat(t *testing.T) {
	r := New()
	counter := r.NewCounter("t_requests_total", "Requests.\nSecond line with a \\ backslash.", "path", "code")
	gauge := r.NewGauge("t_temperature", "A gauge.")
	histogram := r.NewHistogram("t_latency_seconds", "Latency.", []float64{0.1, 0.5, 1}, "route")

	counter.Inc("/a", "200")
	counter.Add(2, "/a", "200")
	counter.Inc("/b", "500")
	counter.Inc(`say "hi"\`+"\n", "200") // label value needing every escape
	counter.Add(-5, "/a", "200")         // ignored: counters do not go down

	gauge.Set(-1.5)

	histogram.Observe(0.1, "r")  // exactly on a bound: inclusive, lands in le="0.1"
	histogram.Observe(0.3, "r")  // between bounds
	histogram.Observe(1, "r")    // exactly on the last bound
	histogram.Observe(7.25, "r") // above every bound: only +Inf counts it
	histogram.Observe(0.2, "q")  // a second series sorts before "r"

	got := render(t, r)
	expect := `# HELP t_latency_seconds Latency.
# TYPE t_latency_seconds histogram
t_latency_seconds_bucket{route="q",le="0.1"} 0
t_latency_seconds_bucket{route="q",le="0.5"} 1
t_latency_seconds_bucket{route="q",le="1"} 1
t_latency_seconds_bucket{route="q",le="+Inf"} 1
t_latency_seconds_sum{route="q"} 0.2
t_latency_seconds_count{route="q"} 1
t_latency_seconds_bucket{route="r",le="0.1"} 1
t_latency_seconds_bucket{route="r",le="0.5"} 2
t_latency_seconds_bucket{route="r",le="1"} 3
t_latency_seconds_bucket{route="r",le="+Inf"} 4
t_latency_seconds_sum{route="r"} 8.65
t_latency_seconds_count{route="r"} 4
# HELP t_requests_total Requests.\nSecond line with a \\ backslash.
# TYPE t_requests_total counter
t_requests_total{path="/a",code="200"} 3
t_requests_total{path="/b",code="500"} 1
t_requests_total{path="say \"hi\"\\\n",code="200"} 1
# HELP t_temperature A gauge.
# TYPE t_temperature gauge
t_temperature -1.5
`
	// The registry's own error counter sorts last (v after t) and carries no
	// series yet, so only its header lines follow.
	suffix := "# HELP vyshka_collect_errors_total Scrape-time collectors that failed; their gauges are omitted from that scrape.\n" +
		"# TYPE vyshka_collect_errors_total counter\n"
	if got != expect+suffix {
		t.Fatalf("exposition mismatch\n--- got\n%s\n--- want\n%s", got, expect+suffix)
	}
}

func TestHistogramWithoutLabelsAndLargeValues(t *testing.T) {
	r := New()
	h := r.NewHistogram("t_size_bytes", "Sizes.", []float64{1024, 1 << 20})
	h.Observe(2048)
	h.Observe(3e15) // beyond 1e15 falls back to exponent form in the sum

	got := render(t, r)
	for _, line := range []string{
		`t_size_bytes_bucket{le="1024"} 0`,
		`t_size_bytes_bucket{le="1048576"} 1`,
		`t_size_bytes_bucket{le="+Inf"} 2`,
		`t_size_bytes_sum 3.000000000002048e+15`,
		`t_size_bytes_count 2`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing line %q in:\n%s", line, got)
		}
	}
}

func TestSpecialValues(t *testing.T) {
	r := New()
	g := r.NewGauge("t_special", "Special values.", "which")
	g.Set(posInf(), "up")
	g.Set(-posInf(), "down")
	g.Set(nan(), "nan")
	got := render(t, r)
	for _, line := range []string{
		`t_special{which="up"} +Inf`,
		`t_special{which="down"} -Inf`,
		`t_special{which="nan"} NaN`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing line %q in:\n%s", line, got)
		}
	}
}

func posInf() float64 { var z float64; return 1 / z }
func nan() float64    { var z float64; return z / z }

func TestInvalidUTF8IsReplaced(t *testing.T) {
	r := New()
	c := r.NewCounter("t_utf_total", "x", "v")
	c.Inc("bad\xffbyte")
	if got := render(t, r); !strings.Contains(got, "t_utf_total{v=\"bad�byte\"} 1\n") {
		t.Fatalf("invalid UTF-8 was not replaced:\n%s", got)
	}
}

// A source that mints label values without bound must cost one series, not
// memory. Without the cap the 600 values below would produce 600 series.
func TestSeriesAreCapped(t *testing.T) {
	r := New()
	c := r.NewCounter("t_capped_total", "x", "code", "kind")
	for i := 0; i < MaxSeries+100; i++ {
		c.Inc(fmt.Sprintf("code-%d", i), "k")
	}
	// A known label set keeps counting after the cap is reached.
	c.Inc("code-0", "k")

	got := render(t, r)
	series := strings.Count(got, "\nt_capped_total{")
	if series != MaxSeries+1 {
		t.Fatalf("got %d series, want %d (the cap plus one overflow series)", series, MaxSeries+1)
	}
	if !strings.Contains(got, `t_capped_total{code="other",kind="other"} 100`+"\n") {
		t.Fatalf("overflow series missing or miscounted:\n%s", got[:300])
	}
	if !strings.Contains(got, `t_capped_total{code="code-0",kind="k"} 2`+"\n") {
		t.Fatal("a series created before the cap stopped counting")
	}
}

func TestWrongLabelArityIsDropped(t *testing.T) {
	r := New()
	c := r.NewCounter("t_arity_total", "x", "a", "b")
	c.Inc("only-one")
	c.Inc("one", "two", "three")
	c.Inc("one", "two")
	got := render(t, r)
	if strings.Count(got, "\nt_arity_total{") != 1 || !strings.Contains(got, `t_arity_total{a="one",b="two"} 1`) {
		t.Fatalf("wrong-arity observations were not dropped:\n%s", got)
	}
}

func TestRegistrationRejectsBadMetrics(t *testing.T) {
	cases := map[string]func(*Registry){
		"bad metric name":    func(r *Registry) { r.NewCounter("1bad", "x") },
		"bad label name":     func(r *Registry) { r.NewCounter("t_a_total", "x", "has-dash") },
		"reserved label":     func(r *Registry) { r.NewCounter("t_b_total", "x", "__name") },
		"duplicate label":    func(r *Registry) { r.NewCounter("t_c_total", "x", "a", "a") },
		"le on a histogram":  func(r *Registry) { r.NewHistogram("t_d", "x", []float64{1}, "le") },
		"unsorted buckets":   func(r *Registry) { r.NewHistogram("t_e", "x", []float64{2, 1}) },
		"repeated bucket":    func(r *Registry) { r.NewHistogram("t_f", "x", []float64{1, 1}) },
		"registered twice":   func(r *Registry) { r.NewGauge("t_g", "x"); r.NewGauge("t_g", "x") },
		"infinite bucket":    func(r *Registry) { r.NewHistogram("t_h", "x", []float64{posInf()}) },
		"collides with self": func(r *Registry) { r.NewCounter("vyshka_collect_errors_total", "x") },
	}
	for name, register := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registration did not panic")
				}
			}()
			register(New())
		})
	}
}

func TestScrapeHooksRefreshGaugesAndCountFailures(t *testing.T) {
	r := New()
	depth := r.NewGauge("t_depth", "x", "server")
	calls := 0
	r.OnScrape("depth", func(context.Context) error {
		calls++
		depth.Reset()
		// The second scrape reports a different set: the old series must go.
		depth.Set(float64(calls), fmt.Sprintf("s%d", calls))
		return nil
	})
	r.OnScrape("broken", func(context.Context) error { return errors.New("db down") })

	first := render(t, r)
	second := render(t, r)
	if !strings.Contains(first, `t_depth{server="s1"} 1`) || strings.Contains(second, `server="s1"`) {
		t.Fatalf("gauge was not refreshed:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.Contains(second, `t_depth{server="s2"} 2`) {
		t.Fatalf("refreshed series missing:\n%s", second)
	}
	if !strings.Contains(second, `vyshka_collect_errors_total{collector="broken"} 2`) {
		t.Fatalf("collector failures not counted:\n%s", second)
	}
}

func TestHandler(t *testing.T) {
	r := New()
	r.NewGauge("t_up", "x").Set(1)
	handler := r.Handler()

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if get.Code != http.StatusOK || get.Header().Get("Content-Type") != ContentType ||
		!strings.Contains(get.Body.String(), "t_up 1\n") {
		t.Fatalf("GET: %d %q\n%s", get.Code, get.Header().Get("Content-Type"), get.Body)
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/metrics", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD: %d with %d body bytes", head.Code, head.Body.Len())
	}

	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %d allow=%q", post.Code, post.Header().Get("Allow"))
	}
}
