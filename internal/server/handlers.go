package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/apierr"
	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/ledger"
)

type handlers struct {
	store  *config.Store
	logger *slog.Logger
	ledger *ledger.Ledger
	// breaker is shared by every chat call in the process: one request's
	// structured quota rejection is what lets the next skip that account.
	breaker  *smolllm.AccountBreaker
	accounts *accountMetrics
}

// cfg returns the current config snapshot. Always call this — never cache
// the result across request boundaries — so SIGHUP reloads take effect.
func (h *handlers) cfg() *config.Config { return h.store.Get() }

func (h *handlers) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// badRequest writes an OpenAI-style 400.
func badRequest(w http.ResponseWriter, message string) {
	apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", message)
}

// maxRequestModelLen bounds the requested model string at the door. That
// string becomes labels in the usage ledger, the account breaker and the
// /metrics and /stats output: downstream maps cap how many series may exist,
// and this caps how many bytes one series may carry. Real names — aliases or
// provider/model strings — are far shorter.
const maxRequestModelLen = 256

// validateModelName rejects requested models that can only be hostile or
// malformed: absurdly long, or carrying control characters (a NUL would
// corrupt account-breaker key parsing, the rest only poison output).
func validateModelName(model string) error {
	if len(model) > maxRequestModelLen {
		return fmt.Errorf("model name longer than %d bytes", maxRequestModelLen)
	}
	for i := 0; i < len(model); i++ {
		if model[i] < 0x20 || model[i] == 0x7f {
			return errors.New("model name contains control characters")
		}
	}
	return nil
}

// upstreamError writes an OpenAI-style 502 — provider call failed.
func upstreamError(w http.ResponseWriter, err error) {
	apierr.Write(w, http.StatusBadGateway, "upstream_error", "api_error", err.Error())
}
