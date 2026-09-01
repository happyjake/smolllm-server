package llm

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/rocry/smolllm-go/smolllm"
)

// BuildOptions converts an incoming ChatRequest into smolllm options.
// `aliasResolve` should map an alias name to a comma-separated chain or return
// the input unchanged if no alias applies (typically *config.Config.ResolveModel).
func BuildOptions(req *ChatRequest, aliasResolve func(string) string) (smolllm.Prompt, []smolllm.Option, error) {
	if req == nil {
		return smolllm.Prompt{}, nil, errors.New("request must not be nil")
	}
	if strings.TrimSpace(req.Model) == "" {
		return smolllm.Prompt{}, nil, errors.New("model is required")
	}
	if len(req.Messages) == 0 {
		return smolllm.Prompt{}, nil, errors.New("messages must not be empty")
	}

	if !isJSONNullOrEmpty(req.Functions) {
		return smolllm.Prompt{}, nil, errors.New(
			"functions are not supported by smolllm-server (the legacy API is superseded by tools)")
	}
	if req.N != nil && *req.N != 1 {
		return smolllm.Prompt{}, nil, fmt.Errorf("n=%d is not supported (only n=1)", *req.N)
	}

	model := req.Model
	if aliasResolve != nil {
		model = aliasResolve(model)
	}

	prompt := smolllm.PromptFromMessages(req.Messages)
	if err := restoreToolCallExtras(prompt.Messages, req.rawMessages); err != nil {
		return smolllm.Prompt{}, nil, err
	}
	if err := restoreMessageExtras(prompt.Messages, req.rawMessages); err != nil {
		return smolllm.Prompt{}, nil, err
	}
	if err := prompt.Validate(); err != nil {
		return smolllm.Prompt{}, nil, err
	}

	opts := []smolllm.Option{smolllm.WithModel(model)}
	if req.Temperature != nil {
		opts = append(opts, smolllm.WithTemperature(*req.Temperature))
	}
	if req.TopP != nil {
		opts = append(opts, smolllm.WithTopP(*req.TopP))
	}
	if req.MaxTokens != nil {
		if *req.MaxTokens <= 0 {
			return smolllm.Prompt{}, nil, fmt.Errorf("max_tokens must be positive (got %d)", *req.MaxTokens)
		}
		opts = append(opts, smolllm.WithMaxTokens(*req.MaxTokens))
	}
	if !isJSONNullOrEmpty(req.Stop) {
		stops, err := decodeStop(req.Stop)
		if err != nil {
			return smolllm.Prompt{}, nil, err
		}
		opts = append(opts, smolllm.WithStop(stops...))
	}
	if req.Seed != nil {
		opts = append(opts, smolllm.WithSeed(*req.Seed))
	}
	if req.ReasoningEffort != nil && strings.TrimSpace(*req.ReasoningEffort) != "" {
		opts = append(opts, smolllm.WithReasoningEffort(*req.ReasoningEffort))
	}
	// Pass-through fields reach the provider verbatim; the server models none of
	// them, so a provider error about one surfaces unchanged.
	if extra := passThroughFields(req); len(extra) > 0 {
		opts = append(opts, smolllm.WithExtraBody(extra))
	}
	if req.Timeout != nil {
		if *req.Timeout < 0 {
			return smolllm.Prompt{}, nil, fmt.Errorf("timeout must be >= 0 (got %g)", *req.Timeout)
		}
		opts = append(opts, smolllm.WithTimeout(time.Duration(*req.Timeout*float64(time.Second))))
	}
	return prompt, opts, nil
}

// restoreToolCallExtras puts back the tool-call keys the openai param union drops
// while decoding. A caller replaying an assistant turn must reach the provider
// with it intact — Gemini expects its thought signature echoed back.
func restoreToolCallExtras(messages []smolllm.Message, raw []json.RawMessage) error {
	if len(raw) != len(messages) {
		return nil // nothing to align against; leave the decoded messages alone
	}
	for i, rawMessage := range raw {
		assistant := messages[i].OfAssistant
		if assistant == nil || len(assistant.ToolCalls) == 0 {
			continue
		}
		var envelope struct {
			ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
		}
		if err := json.Unmarshal(rawMessage, &envelope); err != nil {
			return fmt.Errorf("decode message #%d tool calls: %w", i, err)
		}
		if len(envelope.ToolCalls) != len(assistant.ToolCalls) {
			continue
		}
		for j, rawCall := range envelope.ToolCalls {
			extra := map[string]any{}
			for key, value := range rawCall {
				switch key {
				case "id", "type", "function", "index":
				default:
					extra[key] = value
				}
			}
			if len(extra) == 0 {
				continue
			}
			if fn := assistant.ToolCalls[j].OfFunction; fn != nil {
				fn.SetExtraFields(extra)
			}
		}
	}
	return nil
}

// extraFieldSetter is implemented by every openai-go message variant: each embeds
// paramObj, whose SetExtraFields overrides same-named modeled fields at marshal
// time. Extras must be set on the concrete variant, never on the union wrapper —
// param.MarshalUnion marshals only the present variant and ignores the wrapper's
// own metadata whenever one is set.
type extraFieldSetter interface {
	SetExtraFields(map[string]any)
}

func messageVariant(m *openai.ChatCompletionMessageParamUnion) extraFieldSetter {
	switch {
	case m.OfDeveloper != nil:
		return m.OfDeveloper
	case m.OfSystem != nil:
		return m.OfSystem
	case m.OfUser != nil:
		return m.OfUser
	case m.OfAssistant != nil:
		return m.OfAssistant
	case m.OfTool != nil:
		return m.OfTool
	case m.OfFunction != nil:
		return m.OfFunction
	}
	return nil
}

// restoreMessageExtras re-attaches every top-level message key the openai param
// union dropped or re-encoded lossily, taken verbatim from what the client sent.
//
// The union models only the keys OpenAI documents, so anything else on a message
// is discarded during decode. That silently broke Anthropic-style prompt caching
// through OpenAI-compatible providers: a cache_control marker on a system or user
// content part never reached the provider, so a coding agent re-billed its whole
// prompt every turn while the markers on tool definitions (which ride the raw-JSON
// tools path) did survive — caching half-applied, which is worse than off, because
// the cost accounting silently stops matching reality.
//
// Rather than walk the content-part tree (three different part types across the
// roles, each of which must be reached through its own union), this compares the
// re-encoded message against the raw one and re-attaches whatever did not survive.
// A message the union encodes losslessly yields no extras at all, so ordinary
// traffic is untouched.
func restoreMessageExtras(messages []smolllm.Message, raw []json.RawMessage) error {
	if len(raw) != len(messages) {
		return nil // nothing to align against; leave the decoded messages alone
	}
	for i := range messages {
		variant := messageVariant(&messages[i])
		if variant == nil {
			continue
		}
		encoded, err := json.Marshal(messages[i])
		if err != nil {
			return fmt.Errorf("re-encode message #%d: %w", i, err)
		}
		var rawObj, encodedObj map[string]json.RawMessage
		if err := json.Unmarshal(raw[i], &rawObj); err != nil {
			return fmt.Errorf("decode message #%d: %w", i, err)
		}
		if err := json.Unmarshal(encoded, &encodedObj); err != nil {
			return fmt.Errorf("decode re-encoded message #%d: %w", i, err)
		}

		extra := map[string]any{}
		for key, rawValue := range rawObj {
			// tool_calls belongs to restoreToolCallExtras whenever there are
			// calls to own: it deliberately drops the streaming-only "index"
			// key, which a verbatim override here would put back on the wire.
			// When the union decoded no calls the raw value is null or [], which
			// that restorer skips entirely, so it is forwarded here instead —
			// otherwise the key vanishes from a message that explicitly sent it.
			if key == "tool_calls" {
				if restorerOwnsToolCalls(&messages[i]) {
					continue
				}
				// Forwarded verbatim, minus "index": that key belongs to
				// streaming deltas, and replaying it is what restoreToolCallExtras
				// deliberately avoids. Strict providers reject it.
				stripped, err := stripToolCallIndexes(rawValue)
				if err != nil {
					return fmt.Errorf("normalize message #%d tool calls: %w", i, err)
				}
				extra[key] = stripped
				continue
			}
			encodedValue, ok := encodedObj[key]
			if !ok || !jsonSubset(rawValue, encodedValue) {
				extra[key] = rawValue
			}
		}
		if len(extra) > 0 {
			// SetExtraFields replaces the variant's extras wholesale rather than
			// accumulating, so this must be a single call with the full map.
			variant.SetExtraFields(extra)
		}
	}
	return nil
}

// stripToolCallIndexes removes the streaming-only "index" key from each call in
// a raw tool_calls value, leaving everything else byte-identical. A null or
// non-array value passes through untouched.
func stripToolCallIndexes(raw json.RawMessage) (json.RawMessage, error) {
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return raw, nil // null, or a shape we do not model; forward as sent
	}
	changed := false
	for _, call := range calls {
		if _, ok := call["index"]; ok {
			delete(call, "index")
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	out, err := json.Marshal(calls)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// restorerOwnsToolCalls reports whether restoreToolCallExtras can fully handle
// this message's tool calls, in which case the generic pass must leave the key
// alone (that restorer deliberately drops the streaming-only "index" key).
//
// It only reaches OfFunction, so a message carrying a non-function call — a
// type:"custom" call, say — would silently lose that call's provider extras.
// Those messages are forwarded verbatim by the generic pass instead. An empty
// or null tool_calls is likewise not owned, so an explicitly sent empty array
// still reaches the provider.
func restorerOwnsToolCalls(m *openai.ChatCompletionMessageParamUnion) bool {
	if m.OfAssistant == nil || len(m.OfAssistant.ToolCalls) == 0 {
		return false
	}
	for _, call := range m.OfAssistant.ToolCalls {
		if call.OfFunction == nil {
			return false
		}
	}
	return true
}

// jsonSubset reports whether everything in want survives in got: same scalars,
// same-length arrays element-wise, and every object key present with a subset
// value. Decoding both sides first normalizes number formatting (1 vs 1.0), so a
// re-encoded message does not look lossy just because it round-tripped.
func jsonSubset(want, got json.RawMessage) bool {
	var wantValue, gotValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		return false
	}
	if err := json.Unmarshal(got, &gotValue); err != nil {
		return false
	}
	return valueSubset(wantValue, gotValue)
}

func valueSubset(want, got any) bool {
	switch wantTyped := want.(type) {
	case map[string]any:
		gotTyped, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for key, wantChild := range wantTyped {
			gotChild, ok := gotTyped[key]
			if !ok || !valueSubset(wantChild, gotChild) {
				return false
			}
		}
		return true
	case []any:
		gotTyped, ok := got.([]any)
		if !ok || len(gotTyped) != len(wantTyped) {
			return false
		}
		for i := range wantTyped {
			if !valueSubset(wantTyped[i], gotTyped[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}

// passThroughFields collects the request fields the server forwards verbatim.
func passThroughFields(req *ChatRequest) map[string]any {
	fields := map[string]any{}
	for name, raw := range map[string]json.RawMessage{
		"tools":               req.Tools,
		"tool_choice":         req.ToolChoice,
		"parallel_tool_calls": req.ParallelToolCalls,
		"response_format":     req.ResponseFormat,
	} {
		if !isJSONNullOrEmpty(raw) {
			fields[name] = raw
		}
	}
	// Top-level keys the server does not model reach the provider unchanged.
	// UnmarshalJSON has already removed the modeled ones and the reserved ones
	// smolllm.WithExtraBody would panic on.
	for name, raw := range req.extraFields {
		if _, taken := fields[name]; taken {
			continue
		}
		fields[name] = raw
	}
	return fields
}

func isJSONNullOrEmpty(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return true
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	switch decoded := value.(type) {
	case nil:
		return true
	case []any:
		return len(decoded) == 0
	case map[string]any:
		return len(decoded) == 0
	default:
		return false
	}
}

func decodeStop(raw json.RawMessage) ([]string, error) {
	if isJSONNullOrEmpty(raw) {
		return nil, nil
	}

	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if strings.TrimSpace(single) == "" {
			return nil, errors.New("stop must not be empty")
		}
		return []string{single}, nil
	}

	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil {
		return nil, errors.New("stop must be a string or array of strings")
	}
	if len(multiple) == 0 {
		return nil, errors.New("stop must contain at least one entry")
	}
	for _, stop := range multiple {
		if strings.TrimSpace(stop) == "" {
			return nil, errors.New("stop entries must not be empty")
		}
	}
	return multiple, nil
}

// NewID returns an OpenAI-style chat completion ID, e.g. "chatcmpl-1f2a...".
func NewID() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return "chatcmpl-" + hex.EncodeToString(buf[:])
}
