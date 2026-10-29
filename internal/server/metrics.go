package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/rocry/smolllm-server/internal/ledger"
)

// writeMetrics renders the in-memory ledger as Prometheus text exposition
// (version 0.0.4).
//
// Rendered by hand rather than via a client library: the numbers already exist
// in the ledger, and a dependency would have to be taught to mirror it. The
// contract here is small - counters and one histogram - so the risk of drift
// is lower than the cost of a second source of truth.
func writeMetrics(w io.Writer, attempts []ledger.AttemptCount, firstOutputs []ledger.FirstOutputSample) {
	var b strings.Builder

	b.WriteString("# HELP smolllm_attempts_total Model call attempts by outcome.\n")
	b.WriteString("# TYPE smolllm_attempts_total counter\n")
	for _, a := range attempts {
		// phase is only meaningful for a first-output timeout; emitting it as
		// empty for other outcomes keeps one label set instead of two shapes.
		// Quote by hand, not with %q: %q would re-escape the backslashes
		// escapeLabel just added, producing \\" for a doubled quote.
		fmt.Fprintf(&b,
			"smolllm_attempts_total{alias=\"%s\",provider=\"%s\",model=\"%s\",outcome=\"%s\",phase=\"%s\",streaming=\"%s\"} %d\n",
			escapeLabel(a.Alias), escapeLabel(a.Provider), escapeLabel(a.Model),
			escapeLabel(a.Outcome), escapeLabel(a.Phase), boolLabel(a.Streaming), a.Count,
		)
	}

	if len(firstOutputs) > 0 {
		b.WriteString("# HELP smolllm_attempt_first_output_seconds Seconds from request start to the first\n")
		b.WriteString("# output token of a successful attempt. Abandoned attempts are excluded:\n")
		b.WriteString("# their wait is the budget, and belongs in smolllm_attempts_total.\n")
		b.WriteString("# TYPE smolllm_attempt_first_output_seconds histogram\n")
	}
	for _, s := range firstOutputs {
		labels := fmt.Sprintf("alias=\"%s\",provider=\"%s\",model=\"%s\",streaming=\"%s\"",
			escapeLabel(s.Alias), escapeLabel(s.Provider), escapeLabel(s.Model),
			boolLabel(s.Streaming))
		for i, bound := range s.Bounds {
			fmt.Fprintf(&b,
				"smolllm_attempt_first_output_seconds_bucket{%s,le=\"%s\"} %d\n",
				labels, formatFloat(bound), s.Counts[i],
			)
		}
		fmt.Fprintf(&b,
			"smolllm_attempt_first_output_seconds_bucket{%s,le=\"+Inf\"} %d\n",
			labels, s.Counts[len(s.Counts)-1],
		)
		fmt.Fprintf(&b, "smolllm_attempt_first_output_seconds_sum{%s} %s\n",
			labels, formatFloat(s.Sum))
		fmt.Fprintf(&b, "smolllm_attempt_first_output_seconds_count{%s} %d\n",
			labels, s.Count)
	}

	_, _ = io.WriteString(w, b.String())
}

// boolLabel renders a Prometheus label value for a boolean. Written out rather
// than %t so the strings are a contract the dashboards and rules can pin.
func boolLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// formatFloat renders a float without a trailing ".0" for whole numbers, so
// le="5" reads like every other Prometheus histogram instead of le="5e+00".
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeLabel escapes a Prometheus label value: backslash, double quote and
// newline. A model name with a quote in it would otherwise corrupt the whole
// scrape, not just its own line.
func escapeLabel(v string) string {
	if !strings.ContainsAny(v, `\"`+"\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// metrics serves the in-memory counters for scraping.
func (h *handlers) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	// A half-written scrape is worse than an old one: the scraper would record
	// truncation as a real drop in counters. Render in full, then send.
	var b bytes.Buffer
	writeMetrics(&b, h.ledger.Attempts(), h.ledger.FirstOutputs())
	h.accounts.write(&b, h.breaker.Circuits())
	_, _ = w.Write(b.Bytes())
}
