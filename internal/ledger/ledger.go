// Package ledger aggregates per-attempt LLM usage in memory.
package ledger

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
)

const retention = 31 * 24 * time.Hour

// Bucket aggregates attempts sharing a UTC day, requested alias, and served
// provider/model.
type Bucket struct {
	Day          string `json:"day"`
	Alias        string `json:"alias"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Requests     int    `json:"requests"`
	Failures     int    `json:"failures"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	// CacheReadTokens is the subset of InputTokens the provider served from a
	// prompt cache; CacheWriteTokens is what it wrote. Both are reported-only,
	// so a zero means "not reported" as much as "no cache".
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	// CacheReportedRequests and CacheReportedInputTokens bound the denominator
	// for a hit rate. Without them, one call reporting 80/100 cached and one
	// reporting nothing average to 80/200 — a confident 40% assembled partly
	// from data that does not exist.
	CacheReportedRequests    int `json:"cache_reported_requests"`
	CacheReportedInputTokens int `json:"cache_reported_input_tokens"`
	EstimatedRequests        int `json:"estimated_requests"`
}

type bucketKey struct {
	day      string
	alias    string
	provider string
	model    string
}

// Ledger is a concurrency-safe, process-local usage ledger.
type Ledger struct {
	mu      sync.Mutex
	buckets map[bucketKey]Bucket
	// attempts counts call attempts by alias, model and outcome, keyed by
	// outcome+phase. Unlike buckets it is not partitioned by day: it answers
	// "is this model healthy right now", and a day boundary would reset the
	// answer exactly when someone is looking at it.
	attempts map[attemptKey]int64
	// firstOutput counts, per alias and model, how long attempts waited before
	// their first token. A histogram rather than an average because the shape
	// is the point: a normal distribution of 1-3s with a spike at the budget
	// means queueing, while a fat tail below it means a general slowdown.
	firstOutput map[firstOutputKey]*firstOutputHist
}

// Attempt outcomes. A queued leg is separated from a broken one on purpose:
// only the latter is evidence about the model.
const (
	OutcomeOK          = "ok"
	OutcomeFirstOutput = "first_output_timeout"
	OutcomeError       = "error"
	OutcomeCanceled    = "canceled"
)

type attemptKey struct {
	alias    string
	provider string
	model    string
	outcome  string
	phase    string
}

type firstOutputKey struct {
	alias    string
	provider string
	model    string
}

// firstOutputHist is a fixed-bucket histogram of first-token waits.
type firstOutputHist struct {
	// Bounds are seconds; the last bucket is +Inf.
	Counts []int64
	Sum    float64
	Count  int64
}

// firstOutputBounds are the histogram edges in seconds. They spread across
// the range where the decision actually changes: sub-second answers are fine,
// the 5-30s band is queueing worth watching, and past the budget the leg is
// already abandoned.
var firstOutputBounds = []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120}

// New returns an empty ledger.
func New() *Ledger {
	return &Ledger{
		buckets:     make(map[bucketKey]Bucket),
		attempts:    make(map[attemptKey]int64),
		firstOutput: make(map[firstOutputKey]*firstOutputHist),
	}
}

// Hook returns a request hook bound to the alias or raw model requested by the
// client.
func (l *Ledger) Hook(alias string) func(smolllm.RequestEvent) {
	return func(event smolllm.RequestEvent) {
		l.Record(alias, event)
	}
}

// Record adds one library request event to its stats bucket.
func (l *Ledger) Record(alias string, event smolllm.RequestEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := bucketKey{
		day:      event.Timestamp.UTC().Format(time.DateOnly),
		alias:    alias,
		provider: event.Provider,
		model:    event.ModelName,
	}
	bucket := l.buckets[key]
	bucket.Day = key.day
	bucket.Alias = key.alias
	bucket.Provider = key.provider
	bucket.Model = key.model
	bucket.Requests++
	if event.Error != nil {
		bucket.Failures++
	}
	l.recordAttemptLocked(alias, event)
	bucket.InputTokens += event.InputTokens
	bucket.OutputTokens += event.OutputTokens
	bucket.CacheReadTokens += event.CacheReadTokens
	bucket.CacheWriteTokens += event.CacheWriteTokens
	// Keyed on READ reporting: a provider that reported only cache writes has
	// said nothing about reads, and counting it here would turn an unknown read
	// into a known miss.
	if event.CacheReadReported {
		bucket.CacheReportedRequests++
		bucket.CacheReportedInputTokens += event.InputTokens
	}
	if event.Estimated {
		bucket.EstimatedRequests++
	}
	l.buckets[key] = bucket
	l.pruneLocked(time.Now().UTC())
}

// Snapshot returns a stable, sorted copy of retained buckets.
func (l *Ledger) Snapshot() []Bucket {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneLocked(time.Now().UTC())
	buckets := make([]Bucket, 0, len(l.buckets))
	for _, bucket := range l.buckets {
		buckets = append(buckets, bucket)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Day != buckets[j].Day {
			return buckets[i].Day < buckets[j].Day
		}
		if buckets[i].Alias != buckets[j].Alias {
			return buckets[i].Alias < buckets[j].Alias
		}
		if buckets[i].Provider != buckets[j].Provider {
			return buckets[i].Provider < buckets[j].Provider
		}
		return buckets[i].Model < buckets[j].Model
	})
	return buckets
}

func (l *Ledger) pruneLocked(now time.Time) {
	cutoffDay := now.Add(-retention).Format(time.DateOnly)
	for key := range l.buckets {
		if key.day < cutoffDay {
			delete(l.buckets, key)
		}
	}
}

// recordAttemptLocked classifies one attempt and feeds the counters. Callers
// hold l.mu.
func (l *Ledger) recordAttemptLocked(alias string, event smolllm.RequestEvent) {
	outcome := OutcomeOK
	phase := ""
	switch {
	case event.FirstOutputTimeout:
		// Order matters: a first-output timeout also carries an Error, and
		// classifying it as a plain error is what makes a queued leg look sick.
		outcome = OutcomeFirstOutput
		phase = event.FirstOutputPhase
	case event.Error != nil && errors.Is(event.Error, context.Canceled):
		// The caller hung up. Counting that as a model failure would blame the
		// provider for the client's decision to leave.
		outcome = OutcomeCanceled
	case event.Error != nil:
		outcome = OutcomeError
	}

	key := attemptKey{
		alias:    alias,
		provider: event.Provider,
		model:    event.ModelName,
		outcome:  outcome,
		phase:    phase,
	}
	l.attempts[key]++

	// Only successful attempts contribute first-token latency: a timeout's
	// "latency" is the budget itself, already visible as an outcome, and
	// folding it in would drag the distribution toward whatever budget is set.
	if outcome == OutcomeOK && event.TTFT > 0 {
		fk := firstOutputKey{alias: alias, provider: event.Provider, model: event.ModelName}
		hist := l.firstOutput[fk]
		if hist == nil {
			hist = &firstOutputHist{Counts: make([]int64, len(firstOutputBounds)+1)}
			l.firstOutput[fk] = hist
		}
		seconds := event.TTFT.Seconds()
		idx := len(firstOutputBounds) // +Inf bucket by default
		for i, bound := range firstOutputBounds {
			if seconds <= bound {
				idx = i
				break
			}
		}
		hist.Counts[idx]++
		hist.Sum += seconds
		hist.Count++
	}
}

// AttemptCount is one attempt counter line.
type AttemptCount struct {
	Alias    string
	Provider string
	Model    string
	Outcome  string
	Phase    string
	Count    int64
}

// FirstOutputSample is one model's first-token wait distribution.
type FirstOutputSample struct {
	Alias    string
	Provider string
	Model    string
	Bounds   []float64
	// Counts has len(Bounds)+1 entries; the last is the +Inf bucket. These are
	// cumulative, matching Prometheus semantics so the two cannot diverge.
	Counts []int64
	Sum    float64
	Count  int64
}

// Attempts returns a stable, sorted copy of the attempt counters.
func (l *Ledger) Attempts() []AttemptCount {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]AttemptCount, 0, len(l.attempts))
	for k, n := range l.attempts {
		out = append(out, AttemptCount{
			Alias: k.alias, Provider: k.provider, Model: k.model,
			Outcome: k.outcome, Phase: k.phase, Count: n,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Alias != b.Alias {
			return a.Alias < b.Alias
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Outcome != b.Outcome {
			return a.Outcome < b.Outcome
		}
		return a.Phase < b.Phase
	})
	return out
}

// FirstOutputs returns per-model first-token histograms with cumulative counts.
func (l *Ledger) FirstOutputs() []FirstOutputSample {
	l.mu.Lock()
	defer l.mu.Unlock()

	bounds := append([]float64(nil), firstOutputBounds...)
	out := make([]FirstOutputSample, 0, len(l.firstOutput))
	for k, hist := range l.firstOutput {
		// Convert the per-bucket counts to cumulative, so a rendered histogram
		// and a Prometheus one agree on what the numbers mean.
		cumulative := make([]int64, len(hist.Counts))
		var running int64
		for i, c := range hist.Counts {
			running += c
			cumulative[i] = running
		}
		out = append(out, FirstOutputSample{
			Alias: k.alias, Provider: k.provider, Model: k.model,
			Bounds: bounds, Counts: cumulative,
			Sum: hist.Sum, Count: hist.Count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Alias != b.Alias {
			return a.Alias < b.Alias
		}
		return a.Model < b.Model
	})
	return out
}
