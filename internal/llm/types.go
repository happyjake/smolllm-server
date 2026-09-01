package llm

import (
	"encoding/json"
	"fmt"

	openai "github.com/openai/openai-go/v3"
	"github.com/rocry/smolllm-go/smolllm"
)

// ChatRequest mirrors the OpenAI Chat Completions request body. Only the fields
// we actually forward are explicit; the rest is captured in Extras for future
// pass-through but currently ignored.
type ChatRequest struct {
	Model           string                                   `json:"model"`
	Messages        []openai.ChatCompletionMessageParamUnion `json:"messages"`
	Stream          bool                                     `json:"stream"`
	Temperature     *float64                                 `json:"temperature,omitempty"`
	TopP            *float64                                 `json:"top_p,omitempty"`
	ReasoningEffort *string                                  `json:"reasoning_effort,omitempty"`
	MaxTokens       *int                                     `json:"max_tokens,omitempty"`
	Stop            json.RawMessage                          `json:"stop,omitempty"`
	Seed            *int                                     `json:"seed,omitempty"`
	N               *int                                     `json:"n,omitempty"`
	// Timeout in seconds. 0 disables the timeout (relies on the request context).
	// When omitted, smolllm-go's default applies.
	Timeout *float64 `json:"timeout,omitempty"`

	// Pass-through fields: forwarded to the provider verbatim, never modeled.
	Tools             json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls json.RawMessage `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    json.RawMessage `json:"response_format,omitempty"`

	// StreamOptions is consumed by this server rather than forwarded: the
	// library manages upstream stream usage itself, and smolllm.WithExtraBody
	// panics if handed this key. Only include_usage is honoured.
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`

	// Unsupported; presence triggers 400. The legacy functions API is deprecated
	// upstream and superseded by tools.
	Functions json.RawMessage `json:"functions,omitempty"`

	// rawMessages keeps the messages exactly as the client sent them. The openai
	// param union drops keys it does not model, which would silently strip
	// provider extras (e.g. Gemini thought signatures) from a replayed turn.
	rawMessages []json.RawMessage

	// extraFields holds top-level keys this struct does not model. They are
	// forwarded to the provider verbatim; see passThroughFields.
	extraFields map[string]json.RawMessage
}

// modeledTopLevelKeys are the request fields ChatRequest decodes itself. They are
// never forwarded through the extra-body path, because the struct already carries
// them (and, for functions/n, rejects them).
var modeledTopLevelKeys = map[string]bool{
	"model": true, "messages": true, "stream": true, "temperature": true,
	"top_p": true, "reasoning_effort": true, "max_tokens": true, "stop": true,
	"seed": true, "n": true, "timeout": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "response_format": true, "functions": true,
}

// droppedTopLevelKeys are read back by smolllm-go's own machinery (stream parsing
// and usage collection), which is why smolllm.WithExtraBody PANICS when asked to
// set one. Forwarding a client's stream_options would therefore take the process
// down on the first OpenAI-SDK client that sends it — and those send
// stream_options:{"include_usage":true} routinely. The library requests usage
// itself and owns the retry-without-it path, so dropping it loses nothing.
var droppedTopLevelKeys = map[string]bool{
	"stream": true, "stream_options": true, "messages": true, "model": true,
}

// UnmarshalJSON decodes the request and keeps the raw messages alongside it.
func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	type plain ChatRequest // avoid recursing into this method
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var envelope struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	*r = ChatRequest(decoded)
	r.rawMessages = envelope.Messages

	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	for key := range all {
		if modeledTopLevelKeys[key] || droppedTopLevelKeys[key] {
			delete(all, key)
		}
	}
	if len(all) > 0 {
		r.extraFields = all
	}
	return nil
}

// EmbeddingRequest mirrors POST /v1/embeddings.
// Input may be a single string or an array of strings.
type EmbeddingRequest struct {
	Model      string          `json:"model"`
	Input      json.RawMessage `json:"input"`
	Dimensions *int            `json:"dimensions,omitempty"`
}

// ChatCompletion is the OpenAI non-streaming response shape.
type ChatCompletion struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []ChatChoice    `json:"choices"`
	Usage   CompletionUsage `json:"usage"`
}

type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type ChatMessage struct {
	Role string `json:"role"`
	// Content is null on an assistant turn that only requests tool calls.
	Content          *string            `json:"content"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
	ToolCalls        []smolllm.ToolCall `json:"tool_calls,omitempty"`
}

type CompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// PromptTokensDetails is OpenAI's own field for cache reads, so clients
	// that already understand prompt caching read it without changes. Omitted
	// entirely when the provider reported no cache data: emitting
	// {"cached_tokens":0} would assert "nothing was cached" where the truth is
	// "the provider did not say".
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	// CacheCreationInputTokens has no OpenAI equivalent, so it rides as an
	// Anthropic-style extension. OpenAI clients ignore unknown keys, and
	// omitempty keeps ordinary responses byte-identical to before.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// ChatCompletionChunk is a single SSE frame for streaming chat.
type ChatCompletionChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []ChatChoiceDelta `json:"choices"`
	// Usage rides the terminal frame when the client asked for it, matching
	// OpenAI's stream_options.include_usage. Without it a streaming client can
	// never see token counts — cache reads included — which is the transport
	// coding agents actually use.
	Usage *CompletionUsage `json:"usage,omitempty"`
	Error *ChatStreamError `json:"error,omitempty"`
}

type ChatStreamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type ChatChoiceDelta struct {
	Index        int       `json:"index"`
	Delta        ChatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type ChatDelta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// ToolCalls arrive in one frame at stream end: smolllm-go exposes complete
	// calls only, so there is nothing to emit before then.
	ToolCalls []DeltaToolCall `json:"tool_calls,omitempty"`
}

// DeltaToolCall is one assembled tool call in a streamed delta. The index is the
// call's position, which OpenAI clients use to reassemble.
type DeltaToolCall struct {
	Index int
	Call  smolllm.ToolCall
}

// MarshalJSON writes the call's own fields with `index` alongside them.
func (d DeltaToolCall) MarshalJSON() ([]byte, error) {
	encoded, err := json.Marshal(d.Call)
	if err != nil {
		return nil, fmt.Errorf("encode tool call: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, fmt.Errorf("encode tool call index: %w", err)
	}
	index, err := json.Marshal(d.Index)
	if err != nil {
		return nil, fmt.Errorf("encode tool call index: %w", err)
	}
	fields["index"] = index
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode tool call index: %w", err)
	}
	return out, nil
}

// EmbeddingResponse is the OpenAI /v1/embeddings response shape.
type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []EmbeddingItem `json:"data"`
	Model  string          `json:"model"`
	Usage  EmbeddingUsage  `json:"usage"`
}

type EmbeddingItem struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ModelsResponse is the OpenAI /v1/models response shape.
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Created int64  `json:"created"`
}
