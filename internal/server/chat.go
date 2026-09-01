package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/llm"
)

func (h *handlers) chat(w http.ResponseWriter, r *http.Request) {
	var req llm.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	prompt, opts, err := llm.BuildOptions(&req, h.cfg().ResolveModel, h.cfg().Server.RequestTimeout)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	opts = append(opts, smolllm.WithLogger(h.logger))
	opts = append(opts, smolllm.WithHook(h.ledger.Hook(req.Model)))

	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		h.chatStream(w, r, prompt, opts, includeUsage)
		return
	}
	h.chatBlocking(w, r, prompt, opts, req.Model)
}

func (h *handlers) chatBlocking(w http.ResponseWriter, r *http.Request, prompt smolllm.Prompt, opts []smolllm.Option, requestedModel string) {
	resp, err := smolllm.Ask(r.Context(), prompt, opts...)
	if err != nil {
		upstreamError(w, err)
		return
	}

	// OpenAI reports content as null on an assistant turn that only requested
	// tool calls; clients key on tool_calls, not on the empty string.
	var content *string
	if resp.Text != "" || len(resp.ToolCalls) == 0 {
		text := resp.Text
		content = &text
	}

	out := llm.ChatCompletion{
		ID:      llm.NewID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resolvedModel(resp.Model, requestedModel),
		Choices: []llm.ChatChoice{{
			Index: 0,
			Message: llm.ChatMessage{
				Role:             "assistant",
				Content:          content,
				ReasoningContent: resp.Reasoning,
				ToolCalls:        resp.ToolCalls,
			},
			FinishReason: finishReasonForTurn(resp.FinishReason, len(resp.ToolCalls)),
		}},
		Usage: usageFor(resp.Usage),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		h.logger.Warn("encode response failed", "error", err)
	}
}

func (h *handlers) chatStream(
	w http.ResponseWriter, r *http.Request, prompt smolllm.Prompt, opts []smolllm.Option, includeUsage bool,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		upstreamError(w, errors.New("streaming not supported by this server"))
		return
	}

	stream, err := smolllm.Stream(r.Context(), prompt, opts...)
	if err != nil {
		upstreamError(w, err)
		return
	}
	defer stream.Stream.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := llm.NewID()
	created := time.Now().Unix()
	model := stream.Model

	writeChunk(w, flusher, llm.ChatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []llm.ChatChoiceDelta{{Index: 0, Delta: llm.ChatDelta{Role: "assistant"}}},
	})

	ctx := r.Context()
streamLoop:
	for {
		select {
		case <-ctx.Done():
			stream.Stream.Close()
			_ = stream.Stream.Wait()
			return
		case chunk, ok := <-stream.Stream.Chan():
			if !ok {
				break streamLoop
			}
			if chunk.Content == "" && chunk.Reasoning == "" {
				continue
			}
			writeChunk(w, flusher, llm.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []llm.ChatChoiceDelta{{
					Index: 0,
					Delta: llm.ChatDelta{
						Content:          chunk.Content,
						ReasoningContent: chunk.Reasoning,
					},
				}},
			})
		}
	}

	if err := stream.Stream.Wait(); err != nil {
		reason := "error"
		writeChunk(w, flusher, llm.ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []llm.ChatChoiceDelta{{Index: 0, Delta: llm.ChatDelta{}, FinishReason: &reason}},
			Error:   &llm.ChatStreamError{Message: err.Error(), Type: "api_error"},
		})
		// A failed turn still costs tokens, and the library now preserves usage
		// that arrived before the failure. A client that asked for usage needs it
		// most on the path where something went wrong.
		writeUsageFrame(w, flusher, includeUsage, id, created, model, stream.Usage)
		writeRaw(w, flusher, "[DONE]")
		return
	}

	// smolllm-go exposes complete calls only, so they arrive here in one frame at
	// stream end. Mainstream OpenAI clients reassemble a single complete delta.
	if len(stream.ToolCalls) > 0 {
		deltas := make([]llm.DeltaToolCall, 0, len(stream.ToolCalls))
		for i, call := range stream.ToolCalls {
			deltas = append(deltas, llm.DeltaToolCall{Index: i, Call: call})
		}
		writeChunk(w, flusher, llm.ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []llm.ChatChoiceDelta{{Index: 0, Delta: llm.ChatDelta{ToolCalls: deltas}}},
		})
	}

	finishReason := finishReasonForTurn(stream.FinishReason, len(stream.ToolCalls))
	writeChunk(w, flusher, llm.ChatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []llm.ChatChoiceDelta{{Index: 0, Delta: llm.ChatDelta{}, FinishReason: &finishReason}},
	})
	writeUsageFrame(w, flusher, includeUsage, id, created, model, stream.Usage)
	writeRaw(w, flusher, "[DONE]")
}

// writeUsageFrame emits OpenAI's final usage frame — an empty choices array
// carrying usage — when the client asked for one. Both the success and error
// exits go through it so they cannot drift apart.
func writeUsageFrame(
	w http.ResponseWriter, f http.Flusher, include bool,
	id string, created int64, model string, u smolllm.Usage,
) {
	if !include {
		return
	}
	usage := usageFor(u)
	writeChunk(w, f, llm.ChatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []llm.ChatChoiceDelta{}, Usage: &usage,
	})
}

func writeChunk(w http.ResponseWriter, f http.Flusher, c llm.ChatCompletionChunk) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	writeRaw(w, f, string(b))
}

func writeRaw(w http.ResponseWriter, f http.Flusher, payload string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	f.Flush()
}

// usageFor maps library usage onto the OpenAI usage object, including prompt
// caching. Without this the gateway silently discarded cache counts, so an
// operator had no way to see whether caching was working — or notice it
// regressing.
func usageFor(u smolllm.Usage) llm.CompletionUsage {
	out := llm.CompletionUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.CacheWriteReported {
		writes := u.CacheWriteTokens
		out.CacheCreationInputTokens = &writes
	}
	// Gated on whether the provider SPOKE about reads, not on a non-zero count:
	// an explicit cached_tokens:0 is a real answer ("this call missed"), and
	// suppressing it makes a genuine miss indistinguishable from silence.
	if u.CacheReadReported {
		out.PromptTokensDetails = &llm.PromptTokensDetails{CachedTokens: u.CacheReadTokens}
	}
	return out
}

func resolvedModel(actual, requested string) string {
	if actual != "" {
		return actual
	}
	return requested
}

// finishReasonForTurn reports the OpenAI-shaped reason for a completed turn.
//
// The provider's string passes through untouched except in two cases this
// endpoint's clients depend on: an absent reason becomes "stop", and a turn that
// carries tool calls reports "tool_calls" whatever the provider said. Gemini
// says "stop" while returning tool calls, and an agent loop branching on this
// field would treat that turn as a final answer and never run the tool. The
// libraries still surface the provider's reason verbatim; only this
// OpenAI-compatible surface normalizes it.
func finishReasonForTurn(reason string, toolCalls int) string {
	if toolCalls > 0 {
		return "tool_calls"
	}
	if reason == "" {
		return "stop"
	}
	return reason
}
