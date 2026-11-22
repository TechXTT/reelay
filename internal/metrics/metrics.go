// Package metrics holds the small Prometheus primitives behind GET
// /api/v1/metrics: counters with a fixed label value set, a fixed-bucket
// histogram, and a writer for the text exposition format (version 0.0.4).
//
// Memory is bounded by construction: every label value is declared up front,
// so no request or indexer response can create a new series.
package metrics

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ContentType is the Content-Type of the text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Counter is a monotonically increasing value without labels.
type Counter struct {
	name, help string
	value      atomic.Uint64
}

func NewCounter(name, help string) *Counter { return &Counter{name: name, help: help} }

func (c *Counter) Inc() { c.value.Add(1) }

func (c *Counter) Value() uint64 { return c.value.Load() }

// CounterVec is a counter family with one label whose values are fixed at
// construction.
type CounterVec struct {
	name, help, label string
	values            []string
	counts            []atomic.Uint64
}

// NewCounterVec declares every label value up front. Inc panics on any other
// value, because an undeclared value is a programmer error and silently
// mapping it elsewhere would hide a missing declaration.
func NewCounterVec(name, help, label string, values ...string) *CounterVec {
	return &CounterVec{name: name, help: help, label: label, values: slices.Clone(values),
		counts: make([]atomic.Uint64, len(values))}
}

func (c *CounterVec) Inc(value string) {
	var index = slices.Index(c.values, value)

	if index < 0 {
		panic(fmt.Sprintf("metrics: %s has no label value %q", c.name, value))
	}
	c.counts[index].Add(1)
}

// Value returns the count for a declared label value, or 0 for an unknown one.
func (c *CounterVec) Value(value string) uint64 {
	var index = slices.Index(c.values, value)

	if index < 0 {
		return 0
	}
	return c.counts[index].Load()
}

// Histogram observes seconds into fixed upper-bound buckets.
type Histogram struct {
	name, help string
	bounds     []float64

	mu     sync.Mutex
	counts []uint64 // per bucket, not cumulative; the last slot is +Inf
	sum    float64
}

// NewHistogram takes strictly ascending bucket upper bounds; +Inf is implicit.
func NewHistogram(name, help string, bounds ...float64) *Histogram {
	return &Histogram{name: name, help: help, bounds: slices.Clone(bounds),
		counts: make([]uint64, len(bounds)+1)}
}

func (h *Histogram) Observe(value float64) {
	var index = len(h.bounds)

	for i, bound := range h.bounds {
		if value <= bound {
			index = i
			break
		}
	}
	h.mu.Lock()
	h.counts[index]++
	h.sum += value
	h.mu.Unlock()
}

// Writer emits the text exposition format. The first error from the
// destination is kept and returned by Err, so a caller can write into a
// buffer and check once.
type Writer struct {
	w   io.Writer
	err error
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (w *Writer) Err() error { return w.err }

func (w *Writer) printf(format string, args ...any) {
	if w.err == nil {
		_, w.err = fmt.Fprintf(w.w, format, args...)
	}
}

// Family writes the HELP and TYPE lines; kind is counter, gauge or histogram.
func (w *Writer) Family(name, help, kind string) {
	w.printf("# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, kind)
}

// Sample writes one sample. labels are name, value pairs.
func (w *Writer) Sample(name string, value float64, labels ...string) {
	w.printf("%s%s %s\n", name, formatLabels(labels), formatFloat(value))
}

// Gauge writes a single-sample gauge family.
func (w *Writer) Gauge(name, help string, value float64, labels ...string) {
	w.Family(name, help, "gauge")
	w.Sample(name, value, labels...)
}

func (w *Writer) Counter(c *Counter) {
	w.Family(c.name, c.help, "counter")
	w.Sample(c.name, float64(c.Value()))
}

func (w *Writer) CounterVec(c *CounterVec) {
	w.Family(c.name, c.help, "counter")
	for i, value := range c.values {
		w.Sample(c.name, float64(c.counts[i].Load()), c.label, value)
	}
}

func (w *Writer) Histogram(h *Histogram) {
	h.mu.Lock()
	var counts, sum = slices.Clone(h.counts), h.sum
	h.mu.Unlock()

	var cumulative uint64

	w.Family(h.name, h.help, "histogram")
	for i, bound := range h.bounds {
		cumulative += counts[i]
		w.Sample(h.name+"_bucket", float64(cumulative), "le", formatFloat(bound))
	}
	cumulative += counts[len(h.bounds)]
	w.Sample(h.name+"_bucket", float64(cumulative), "le", "+Inf")
	w.Sample(h.name+"_sum", sum)
	w.Sample(h.name+"_count", float64(cumulative))
}

func formatFloat(value float64) string {
	switch {
	case math.IsInf(value, 1):
		return "+Inf"
	case math.IsInf(value, -1):
		return "-Inf"
	case math.IsNaN(value):
		return "NaN"
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func formatLabels(pairs []string) string {
	var out strings.Builder

	if len(pairs) == 0 {
		return ""
	}
	out.WriteByte('{')
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(pairs[i])
		out.WriteString(`="`)
		out.WriteString(escapeLabel(pairs[i+1]))
		out.WriteByte('"')
	}
	out.WriteByte('}')
	return out.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(value string) string { return labelEscaper.Replace(value) }

func escapeHelp(value string) string { return helpEscaper.Replace(value) }
