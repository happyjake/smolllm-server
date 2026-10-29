package server

import (
	"encoding/json"
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

// upstreamError writes an OpenAI-style 502 — provider call failed.
func upstreamError(w http.ResponseWriter, err error) {
	apierr.Write(w, http.StatusBadGateway, "upstream_error", "api_error", err.Error())
}
