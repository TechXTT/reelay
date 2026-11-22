package metrics

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestWriterCounterVecEmitsEveryDeclaredValue(t *testing.T) {
	var counter = NewCounterVec("jobs_total", "Jobs.", "kind", "a", "b")
	var buf bytes.Buffer

	counter.Inc("b")
	counter.Inc("b")
	var w = NewWriter(&buf)
	w.CounterVec(counter)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	var want = "# HELP jobs_total Jobs.\n# TYPE jobs_total counter\njobs_total{kind=\"a\"} 0\njobs_total{kind=\"b\"} 2\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
	if counter.Value("b") != 2 || counter.Value("missing") != 0 {
		t.Fatal("unexpected counter values")
	}
}

func TestCounterVecPanicsOnUndeclaredValue(t *testing.T) {
	var counter = NewCounterVec("jobs_total", "Jobs.", "kind", "a")

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	counter.Inc("unknown")
}

func TestPlainCounter(t *testing.T) {
	var counter = NewCounter("fails_total", "Fails.")
	var buf bytes.Buffer

	counter.Inc()
	NewWriter(&buf).Counter(counter)
	if !strings.Contains(buf.String(), "# TYPE fails_total counter\nfails_total 1\n") {
		t.Fatalf("got %q", buf.String())
	}
}

func TestLabelAndHelpEscaping(t *testing.T) {
	var buf bytes.Buffer
	var w = NewWriter(&buf)

	w.Family("x", "line1\nback\\slash", "gauge")
	w.Sample("x", 1, "name", "a\"b\\c\nd")
	var want = "# HELP x line1\\nback\\\\slash\n# TYPE x gauge\nx{name=\"a\\\"b\\\\c\\nd\"} 1\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}

func TestFloatFormatting(t *testing.T) {
	var buf bytes.Buffer
	var w = NewWriter(&buf)

	w.Sample("v", 0.5)
	w.Sample("v", 1e21)
	w.Sample("v", math.Inf(1))
	w.Sample("v", math.NaN())
	if buf.String() != "v 0.5\nv 1e+21\nv +Inf\nv NaN\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestHistogramBucketsAreCumulativeWithInf(t *testing.T) {
	var h = NewHistogram("dur_seconds", "Duration.", 1, 5, 15)
	var buf bytes.Buffer

	h.Observe(0.5)
	h.Observe(5)
	h.Observe(100)
	NewWriter(&buf).Histogram(h)
	var want = "# HELP dur_seconds Duration.\n# TYPE dur_seconds histogram\n" +
		"dur_seconds_bucket{le=\"1\"} 1\ndur_seconds_bucket{le=\"5\"} 2\n" +
		"dur_seconds_bucket{le=\"15\"} 2\ndur_seconds_bucket{le=\"+Inf\"} 3\n" +
		"dur_seconds_sum 105.5\ndur_seconds_count 3\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}
