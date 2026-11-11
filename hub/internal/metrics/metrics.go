// Package metrics is a small Prometheus registry: counters, gauges, and
// histograms with fixed buckets, rendered in the text exposition format 0.0.4.
//
// It exists so the hub stays a single static binary with a short dependency
// list; the official client library would be most of the module graph for the
// few hundred lines this needs. It implements only what the hub uses: metric
// vectors with a fixed label set, a histogram with caller-chosen buckets, and
// a collect hook that refreshes gauges at scrape time.
//
// Adding a metric is one line at the place that owns the event:
//
//	var dropped = registry.NewCounter("vyshka_things_dropped_total", "Things dropped.", "reason")
//	...
//	dropped.Inc("queue_full")
//
// Label values are never trusted to be bounded. Every vector holds at most
// MaxSeries distinct label sets; the one that would exceed it is folded into a
// single series whose label values are all "other", so a source that mints
// label values without bound costs one series, not the process's memory.
package metrics

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxSeries bounds the distinct label sets one metric keeps (see the package
// comment). It is far above what the hub's own labels produce: a handful of
// servers, webhooks, and action codes.
const MaxSeries = 1000

// OverflowValue is every label value of the series that absorbs label sets
// past MaxSeries.
const OverflowValue = "other"

// ContentType is what a scrape answers with.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

var (
	metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelName  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Registry holds the metrics and renders them. The zero value is not usable;
// call New.
type Registry struct {
	mu         sync.Mutex
	families   []family
	byName     map[string]bool
	collectors []collector

	// scrape serializes scrapes, so a collect hook that resets and refills a
	// gauge is never observed half done by a concurrent scrape.
	scrape sync.Mutex

	collectErrors *Counter
}

type collector struct {
	name string
	fn   func(context.Context) error
}

// family is what the registry needs of each metric type.
type family interface {
	name() string
	write(w io.Writer)
}

// New returns an empty registry.
func New() *Registry {
	r := &Registry{byName: map[string]bool{}}
	r.collectErrors = r.NewCounter("vyshka_collect_errors_total",
		"Scrape-time collectors that failed; their gauges are omitted from that scrape.", "collector")
	return r
}

// register adds a family under a unique, valid name. A bad metric is a
// programming error found at boot, so it panics rather than returning.
func (r *Registry) register(f family, labels []string) {
	if !metricName.MatchString(f.name()) {
		panic(fmt.Sprintf("metrics: invalid metric name %q", f.name()))
	}
	seen := map[string]bool{}
	for _, label := range labels {
		if !labelName.MatchString(label) || strings.HasPrefix(label, "__") {
			panic(fmt.Sprintf("metrics: %s: invalid label name %q", f.name(), label))
		}
		if seen[label] {
			panic(fmt.Sprintf("metrics: %s: duplicate label name %q", f.name(), label))
		}
		seen[label] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName[f.name()] {
		panic(fmt.Sprintf("metrics: %s registered twice", f.name()))
	}
	r.byName[f.name()] = true
	r.families = append(r.families, f)
}

// vec is the label-set bookkeeping shared by the three metric types.
type vec[T any] struct {
	metric string
	help   string
	labels []string
	mu     sync.Mutex
	series map[string]*entry[T]
	make   func() *T
}

type entry[T any] struct {
	values []string
	state  *T
}

func (v *vec[T]) name() string { return v.metric }

// get returns the series for a label set, creating it, or the overflow series
// once MaxSeries is reached. nil means the arity was wrong: the observation is
// dropped, not a crash, because the hub must not die over its own telemetry.
// Callers hold v.mu.
func (v *vec[T]) get(values []string) *T {
	if len(values) != len(v.labels) {
		return nil
	}
	key := seriesKey(values)
	if found, ok := v.series[key]; ok {
		return found.state
	}
	if len(v.series) >= MaxSeries {
		values = overflowValues(len(v.labels))
		key = seriesKey(values)
		if found, ok := v.series[key]; ok {
			return found.state
		}
	}
	created := &entry[T]{values: append([]string(nil), values...), state: v.make()}
	v.series[key] = created
	return created.state
}

func overflowValues(n int) []string {
	values := make([]string, n)
	for i := range values {
		values[i] = OverflowValue
	}
	return values
}

// seriesKey joins values with a separator that cannot occur in valid text.
func seriesKey(values []string) string { return strings.Join(values, "\xff") }

// sorted returns the entries in label order, for stable output. Callers hold
// v.mu.
func (v *vec[T]) sorted() []*entry[T] {
	entries := make([]*entry[T], 0, len(v.series))
	for _, e := range v.series {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i].values, entries[j].values
		for k := range a {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
	return entries
}

// Counter is a monotonically increasing value per label set.
type Counter struct{ vec[float64] }

// NewCounter registers a counter. By convention its name ends in _total.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{vec[float64]{
		metric: name, help: help, labels: labels,
		series: map[string]*entry[float64]{},
		make:   func() *float64 { return new(float64) },
	}}
	r.register(c, labels)
	return c
}

// Inc adds one.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds n, which must not be negative or NaN: a counter never goes down.
func (c *Counter) Add(n float64, labelValues ...string) {
	if n < 0 || math.IsNaN(n) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.get(labelValues); s != nil {
		*s += n
	}
}

func (c *Counter) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	writeHeader(w, c.metric, c.help, "counter")
	for _, e := range c.sorted() {
		writeSample(w, c.metric, c.labels, e.values, "", *e.state)
	}
}

// Gauge is a value per label set that may go up and down.
type Gauge struct{ vec[float64] }

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string, labels ...string) *Gauge {
	g := &Gauge{vec[float64]{
		metric: name, help: help, labels: labels,
		series: map[string]*entry[float64]{},
		make:   func() *float64 { return new(float64) },
	}}
	r.register(g, labels)
	return g
}

// Set replaces the value of one label set.
func (g *Gauge) Set(v float64, labelValues ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.get(labelValues); s != nil {
		*s = v
	}
}

// Reset drops every label set. A collect hook calls it before refilling, so a
// server that no longer exists stops being reported.
func (g *Gauge) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.series = map[string]*entry[float64]{}
}

func (g *Gauge) write(w io.Writer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	writeHeader(w, g.metric, g.help, "gauge")
	for _, e := range g.sorted() {
		writeSample(w, g.metric, g.labels, e.values, "", *e.state)
	}
}

// Histogram counts observations into fixed cumulative buckets.
type Histogram struct {
	vec[histogramState]
	bounds []float64
}

type histogramState struct {
	counts []uint64 // per bucket, not cumulative; the +Inf bucket is count
	sum    float64
	count  uint64
}

// NewHistogram registers a histogram. bounds are the upper bounds of the
// buckets, strictly increasing and finite; the +Inf bucket is implicit.
func (r *Registry) NewHistogram(name, help string, bounds []float64, labels ...string) *Histogram {
	for i, b := range bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) || (i > 0 && b <= bounds[i-1]) {
			panic(fmt.Sprintf("metrics: %s: bucket bounds must be finite and strictly increasing", name))
		}
	}
	for _, label := range labels {
		if label == "le" {
			panic(fmt.Sprintf("metrics: %s: %q is reserved for the bucket bound", name, label))
		}
	}
	h := &Histogram{bounds: append([]float64(nil), bounds...)}
	h.vec = vec[histogramState]{
		metric: name, help: help, labels: labels,
		series: map[string]*entry[histogramState]{},
		make:   func() *histogramState { return &histogramState{counts: make([]uint64, len(h.bounds))} },
	}
	r.register(h, labels)
	return h
}

// Observe records one value. NaN is ignored.
func (h *Histogram) Observe(v float64, labelValues ...string) {
	if math.IsNaN(v) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(labelValues)
	if s == nil {
		return
	}
	// The first bucket whose bound is at or above v: bounds are inclusive.
	if i := sort.SearchFloat64s(h.bounds, v); i < len(h.bounds) {
		s.counts[i]++
	}
	s.sum += v
	s.count++
}

// ObserveDuration records d in seconds.
func (h *Histogram) ObserveDuration(d time.Duration, labelValues ...string) {
	h.Observe(d.Seconds(), labelValues...)
}

func (h *Histogram) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	writeHeader(w, h.metric, h.help, "histogram")
	for _, e := range h.sorted() {
		var cumulative uint64
		for i, bound := range h.bounds {
			cumulative += e.state.counts[i]
			writeSample(w, h.metric+"_bucket", h.labels, e.values, formatFloat(bound), float64(cumulative))
		}
		writeSample(w, h.metric+"_bucket", h.labels, e.values, "+Inf", float64(e.state.count))
		writeSample(w, h.metric+"_sum", h.labels, e.values, "", e.state.sum)
		writeSample(w, h.metric+"_count", h.labels, e.values, "", float64(e.state.count))
	}
}

// OnScrape registers a hook that runs at the start of every scrape, before
// anything is rendered, for gauges whose value is read from somewhere else (a
// database) rather than maintained as events happen. name labels the failure
// counter when fn errors. Hooks run one at a time under the scrape lock with
// the scrape's context, so they may Reset and refill a gauge without a
// concurrent scrape seeing it half done.
func (r *Registry) OnScrape(name string, fn func(context.Context) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collectors = append(r.collectors, collector{name: name, fn: fn})
}

// Write runs the scrape hooks and renders every metric, sorted by name.
func (r *Registry) Write(ctx context.Context, w io.Writer) {
	r.scrape.Lock()
	defer r.scrape.Unlock()

	r.mu.Lock()
	collectors := append([]collector(nil), r.collectors...)
	families := append([]family(nil), r.families...)
	r.mu.Unlock()

	for _, c := range collectors {
		if err := c.fn(ctx); err != nil {
			r.collectErrors.Inc(c.name)
		}
	}
	sort.Slice(families, func(i, j int) bool { return families[i].name() < families[j].name() })
	for _, f := range families {
		f.write(w)
	}
}

// Handler serves the registry as GET /metrics would: the exposition format for
// GET and HEAD, 405 for anything else. Every path is answered the same way, so
// mount it at the path you want.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", ContentType)
		if req.Method == http.MethodHead {
			return
		}
		r.Write(ctx, w)
	})
}

func writeHeader(w io.Writer, name, help, kind string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, escapeHelp(help))
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
}

// writeSample writes one line. le, when not empty, is appended as the last
// label, which is where histogram buckets carry it.
func writeSample(w io.Writer, name string, labels, values []string, le string, value float64) {
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 || le != "" {
		b.WriteByte('{')
		for i, label := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(label)
			b.WriteString(`="`)
			b.WriteString(escapeLabelValue(values[i]))
			b.WriteByte('"')
		}
		if le != "" {
			if len(labels) > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`le="`)
			b.WriteString(le)
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatFloat(value))
	b.WriteByte('\n')
	_, _ = io.WriteString(w, b.String())
}

// formatFloat renders a sample value: the special values spelled as the format
// wants them, whole numbers without an exponent, everything else in the
// shortest form that round-trips.
func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeHelp escapes a HELP text: backslash and line feed.
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(strings.ToValidUTF8(s, "�"))
}

// escapeLabelValue escapes a label value: backslash, double quote, and line
// feed. Invalid UTF-8 is replaced, because the format is defined over UTF-8.
func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(strings.ToValidUTF8(s, "�"))
}
