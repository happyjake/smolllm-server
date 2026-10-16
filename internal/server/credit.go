package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/llm"
)

// Request-local actual provider debits, including failed legs/retries. No rate
// table, currency conversion or guessed cost; nil credit is not a free call.
type creditTally struct {
	mu       sync.Mutex
	total    big.Rat
	scale    int
	id       string
	attempts int
	known    int
}

func (b *creditTally) record(e smolllm.RequestEvent) {
	var raw struct {
		Credit *json.Number `json:"credit"`
	}
	err := json.Unmarshal(e.RawUsage, &raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	if err != nil || raw.Credit == nil {
		return
	}
	// json.Number may accept quoted values; require the original JSON number.
	var fields map[string]json.RawMessage
	if json.Unmarshal(e.RawUsage, &fields) != nil || len(fields["credit"]) == 0 || fields["credit"][0] == '"' {
		return
	}
	text := raw.Credit.String()
	if len(text) > 64 {
		return
	}
	// Bound exponent/precision to avoid pathological huge output from a provider.
	parts := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return r == 'e' })
	scale := 0
	if i := strings.IndexByte(parts[0], '.'); i >= 0 {
		scale = len(parts[0]) - i - 1
	}
	if len(parts) == 2 {
		exp, err := strconv.Atoi(parts[1])
		if err != nil || exp < -18 || exp > 18 {
			return
		}
		scale -= exp
	}
	if scale > 18 {
		return
	}
	var v big.Rat
	if _, ok := v.SetString(text); !ok || v.Sign() < 0 {
		return
	}
	if scale > b.scale {
		b.scale = scale
	}
	b.total.Add(&b.total, &v)
	b.known++
}

func (b *creditTally) apply(u llm.CompletionUsage) llm.CompletionUsage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.id == "" {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			panic(err)
		}
		b.id = hex.EncodeToString(bytes[:])
	}
	u.CreditReportID = b.id
	if b.known > 0 {
		v := json.Number(b.total.FloatString(b.scale))
		u.Credit = &v
	}
	u.CreditAttempts = b.attempts
	u.CreditKnownAttempts = b.known
	u.CreditComplete = b.attempts > 0 && b.known == b.attempts
	return u
}

// HTTP failures have no SSE usage frame. Expose the same restricted metadata
// in headers so clients can retain reported debits without parsing error bodies.
func (b *creditTally) errorHeaders(w http.ResponseWriter) {
	u := b.apply(llm.CompletionUsage{})
	if u.Credit != nil {
		w.Header().Set("X-Usage-Credit", u.Credit.String())
	}
	w.Header().Set("X-Usage-Credit-Report-Id", u.CreditReportID)
	w.Header().Set("X-Usage-Credit-Complete", strconv.FormatBool(u.CreditComplete))
	w.Header().Set("X-Usage-Credit-Attempts", strconv.Itoa(u.CreditAttempts))
	w.Header().Set("X-Usage-Credit-Known-Attempts", strconv.Itoa(u.CreditKnownAttempts))
}
