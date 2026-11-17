package server

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/rocry/smolllm-go/smolllm"
)

// accountMetrics counts the shared account breaker's decisions for /metrics.
//
// It is fed by smolllm.WithAccountEventHook, never by the attempt hook: a
// skipped leg is not an upstream attempt, and counting it there would corrupt
// the attempt ledger and billing. Labels are bounded by configuration: the
// provider and model come from the alias chains, the account is a 12-hex
// fingerprint of provider plus credential (never the credential), and alias is
// the same requested-model label smolllm_attempts_total already carries.
type accountMetrics struct {
	mu sync.Mutex
	// transitions is keyed by account+model and opened/closed. The circuit is
	// shared by every alias, so an alias label here would describe the request
	// that happened to trip it, not the circuit; the model is part of the
	// circuit key since quota rejections are per account+model.
	transitions map[accountTransitionKey]int64
	skips       map[accountSkipKey]int64
	probes      map[accountLegKey]int64
	failOpens   map[accountLegKey]int64
	// unrecognized429 counts structured 429s whose code the breaker does not
	// know. They stay model-local; this is how a new quota code becomes
	// visible before anyone is tempted to match message text.
	unrecognized429 map[accountLegKey]int64
	// seen lists every account+model pair the breaker has reported on, so the
	// state gauge can show closed pairs too instead of only the open ones.
	// Model names arrive from requests (not only configured chains), so seen
	// is capped: pairs past the cap simply have no closed series. The circuit
	// events themselves stay bounded by the library's maxAccountCircuits.
	seen map[accountLegID]struct{}
	// maxSeen caps len(seen); a field so tests can shrink it. Past the cap a
	// pair gets no closed gauge series — it is closed either way.
	maxSeen int
}

// maxSeenPairs bounds the state-gauge pair set: comfortably above the
// breaker's own maxAccountCircuits (every circuit can appear here, times the
// configured aliases), far below anything that could grow with requests.
const maxSeenPairs = 8192

type accountID struct {
	provider string
	account  string
}

// accountLegID is the circuit identity: one credential plus one model.
type accountLegID struct {
	accountID
	model string
}

type accountTransitionKey struct {
	accountLegID
	transition string
}

type accountLegKey struct {
	alias string
	accountID
	model string
}

type accountSkipKey struct {
	accountLegKey
	reason string
}

func newAccountMetrics() *accountMetrics {
	return &accountMetrics{
		transitions:     map[accountTransitionKey]int64{},
		skips:           map[accountSkipKey]int64{},
		probes:          map[accountLegKey]int64{},
		failOpens:       map[accountLegKey]int64{},
		unrecognized429: map[accountLegKey]int64{},
		seen:            map[accountLegID]struct{}{},
		maxSeen:         maxSeenPairs,
	}
}

// hook returns the account-event hook for one request on alias.
func (m *accountMetrics) hook(alias string) func(smolllm.AccountEvent) {
	return func(e smolllm.AccountEvent) { m.record(alias, e) }
}

func (m *accountMetrics) record(alias string, e smolllm.AccountEvent) {
	id := accountLegID{accountID: accountID{provider: e.Provider, account: e.Account}, model: e.ModelName}
	leg := accountLegKey{alias: alias, accountID: id.accountID, model: e.ModelName}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.seen[id]; ok || len(m.seen) < m.maxSeen {
		m.seen[id] = struct{}{}
	}
	switch e.Kind {
	case smolllm.AccountOpened, smolllm.AccountClosed:
		m.transitions[accountTransitionKey{accountLegID: id, transition: string(e.Kind)}]++
	case smolllm.AccountSkipped:
		m.skips[accountSkipKey{accountLegKey: leg, reason: e.SkipReason}]++
	case smolllm.AccountProbing:
		m.probes[leg]++
	case smolllm.AccountFailOpen:
		m.failOpens[leg]++
	case smolllm.AccountUnrecognized429:
		m.unrecognized429[leg]++
	}
}

// circuitStates are the values of the state label, rendered one-hot per
// account so a dashboard can sum or filter without parsing a value.
var circuitStates = []string{"closed", "open", "half_open"}

// write renders the account families. A family with no series is omitted,
// like the first-output histogram, so a server running with the breaker
// disabled exposes none of them.
func (m *accountMetrics) write(w io.Writer, circuits []smolllm.AccountCircuit) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder

	writeFamily(&b, "smolllm_account_circuit_transitions_total", "counter",
		"Account circuit transitions: opened on a structured quota rejection, closed on a later success. One series per account+model pair.",
		sortedSeries(m.transitions, func(k accountTransitionKey) string {
			return legIDLabels(k.accountLegID) + fmt.Sprintf(",transition=\"%s\"", escapeLabel(k.transition))
		}))
	writeFamily(&b, "smolllm_account_circuit_skips_total", "counter",
		"Chain legs skipped without an upstream call because their account was exhausted.",
		sortedSeries(m.skips, func(k accountSkipKey) string {
			return legLabels(k.accountLegKey) + fmt.Sprintf(",reason=\"%s\"", escapeLabel(k.reason))
		}))
	writeFamily(&b, "smolllm_account_circuit_probes_total", "counter",
		"Half-open probes: a real request trying an account after its cooldown.",
		sortedSeries(m.probes, legLabels))
	writeFamily(&b, "smolllm_account_circuit_fail_open_total", "counter",
		"Legs on open accounts tried anyway because no admitted leg succeeded.",
		sortedSeries(m.failOpens, legLabels))
	writeFamily(&b, "smolllm_unrecognized_structured_429_total", "counter",
		"HTTP 429s with a JSON body but no recognized account code; treated as model-local.",
		sortedSeries(m.unrecognized429, legLabels))

	states := make(map[accountLegID]string, len(m.seen)+len(circuits))
	for id := range m.seen {
		states[id] = "closed"
	}
	for _, c := range circuits {
		states[accountLegID{accountID: accountID{provider: c.Provider, account: c.Account}, model: c.ModelName}] = string(c.State)
	}
	ids := make([]accountLegID, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].provider != ids[j].provider {
			return ids[i].provider < ids[j].provider
		}
		if ids[i].account != ids[j].account {
			return ids[i].account < ids[j].account
		}
		return ids[i].model < ids[j].model
	})
	if len(ids) > 0 {
		b.WriteString("# HELP smolllm_account_circuit_state Current account+model circuit state, one-hot over closed/open/half_open.\n")
		b.WriteString("# TYPE smolllm_account_circuit_state gauge\n")
	}
	for _, id := range ids {
		for _, state := range circuitStates {
			value := 0
			if states[id] == state {
				value = 1
			}
			fmt.Fprintf(&b, "smolllm_account_circuit_state{%s,state=\"%s\"} %d\n", legIDLabels(id), state, value)
		}
	}

	_, _ = io.WriteString(w, b.String())
}

type series struct {
	labels string
	value  int64
}

func sortedSeries[K comparable](counts map[K]int64, labels func(K) string) []series {
	out := make([]series, 0, len(counts))
	for k, v := range counts {
		out = append(out, series{labels: labels(k), value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].labels < out[j].labels })
	return out
}

func writeFamily(b *strings.Builder, name, kind, help string, rows []series) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	for _, r := range rows {
		fmt.Fprintf(b, "%s{%s} %d\n", name, r.labels, r.value)
	}
}

func accountLabels(id accountID) string {
	return fmt.Sprintf("provider=\"%s\",account=\"%s\"", escapeLabel(id.provider), escapeLabel(id.account))
}

func legIDLabels(id accountLegID) string {
	return fmt.Sprintf("%s,model=\"%s\"", accountLabels(id.accountID), escapeLabel(id.model))
}

func legLabels(k accountLegKey) string {
	return fmt.Sprintf("alias=\"%s\",%s,model=\"%s\"", escapeLabel(k.alias), accountLabels(k.accountID), escapeLabel(k.model))
}
