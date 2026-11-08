package chatcompletions

import (
	"encoding/json"
	"fmt"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/errclass"
)

// ---- upstream Chat Completions response shapes (FR-006) ----

type ccRespMessage struct {
	ReasoningContent string              `json:"reasoning_content"`
	Content          json.RawMessage     `json:"content"` // JSON string or part array
	ToolCalls        []shared.CCToolCall `json:"tool_calls"`
}

type ccChoice struct {
	Message      ccRespMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type ccUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type ccResponse struct {
	ID      string     `json:"id"`
	Model   string     `json:"model"`
	Choices []ccChoice `json:"choices"`
	Usage   *ccUsage   `json:"usage"`
}

// ConvertNonStreamResponse translates an upstream Chat Completions HTTP
// response back into the client protocol sourceFormat (FR-006, AC §B).
// Statuses >= 400 become classified redacted errors (§7); "openai"
// passes through; "claude" and "openai-response" are converted,
// preserving text, tool calls, finish reason, and usage. Unknown formats
// are ClassUnsupported; malformed upstream bodies are ClassTranslation.
func ConvertNonStreamResponse(sourceFormat string, status int, upstreamBody []byte, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	if status >= 400 {
		return nil, shared.UpstreamStatusError(status, upstreamBody)
	}
	switch sourceFormat {
	case "openai":
		return passthroughResponse(upstreamBody)
	case "claude":
		return chatToClaude(upstreamBody)
	case "openai-response":
		return chatToResponses(upstreamBody, tools...)
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// decodeCC validates and decodes an upstream non-stream response body;
// a body without choices is a translation failure, never silently empty
// (FR-006).
func decodeCC(body []byte) (*ccResponse, *errclass.Error) {
	var resp ccResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errclass.Translation("malformed Chat Completions response JSON: " + err.Error())
	}
	if len(resp.Choices) == 0 {
		return nil, errclass.Translation("Chat Completions response carries no choices")
	}
	return &resp, nil
}

// passthroughResponse validates the Chat Completions body decodes as
// JSON and forwards it unchanged (FR-003).
func passthroughResponse(body []byte) ([]byte, *errclass.Error) {
	if !json.Valid(body) {
		return nil, errclass.Translation("malformed openai response JSON")
	}
	return body, nil
}

// ---- Chat Completions -> claude (Anthropic Messages) (FR-006) ----

// chatToClaude converts a Chat Completions result into a Messages
// response via the shared Claude-result kernel: content becomes text or
// image blocks, tool_calls become tool_use blocks, finish_reason maps to
// stop_reason, and prompt/completion token counts map to input/output
// usage (FR-006, AC §B).
func chatToClaude(body []byte) ([]byte, *errclass.Error) {
	resp, eErr := decodeCC(body)
	if eErr != nil {
		return nil, eErr
	}
	choice := resp.Choices[0]
	blocks, eErr := ccContentToBlocks(choice.Message.Content)
	if eErr != nil {
		return nil, eErr
	}
	toolUse, eErr := claudeToolUseBlocks(choice.Message.ToolCalls)
	if eErr != nil {
		return nil, eErr
	}
	var inputTokens, outputTokens int64
	var cacheRead *int64
	if resp.Usage != nil {
		inputTokens, outputTokens = resp.Usage.PromptTokens, resp.Usage.CompletionTokens
		if resp.Usage.PromptDetails != nil {
			cacheRead = resp.Usage.PromptDetails.CachedTokens
		}
	}
	// Observed tool calls outrank the status-derived reason: a terminal
	// length cannot downgrade tool_calls to max_tokens.
	stop := shared.TerminalReason(len(choice.Message.ToolCalls) > 0,
		"tool_use", shared.FinishToClaudeStop(choice.FinishReason))
	b, _ := json.Marshal(shared.NewClaudeResult(resp.ID, resp.Model, stop,
		shared.ClampSubtract(inputTokens, cacheRead), outputTokens, cacheRead, nil, append(blocks, toolUse...)))
	return b, nil // only marshallable composed types; cannot fail
}

// claudeStopReason was replaced by shared.FinishToClaudeStop, which adds
// the missing content_filter→refusal leg so stop reasons round-trip.

// ccContentToBlocks normalizes response content into Anthropic blocks
// via the shared kernel, which owns validation and the empty-input
// policy (FR-006 multimodal preservation). Responses-flavored alias part
// names in upstream CC bodies therefore behave identically to every
// other route (F5 parity).
func ccContentToBlocks(raw json.RawMessage) ([]claudeBlock, *errclass.Error) {
	return shared.ClaudeBlocksFromOpenAI(raw, EndpointPath)
}

// claudeToolUseBlocks converts tool calls into tool_use blocks; malformed
// arguments are a translation failure, never dropped (FR-006).
func claudeToolUseBlocks(calls []shared.CCToolCall) ([]claudeBlock, *errclass.Error) {
	var blocks []claudeBlock
	for _, tc := range calls {
		input, eErr := shared.DecodeArgs(tc.Function.Arguments)
		if eErr != nil {
			return nil, eErr
		}
		blocks = append(blocks, claudeBlock{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
		})
	}
	return blocks, nil
}

// ---- Chat Completions -> openai-response (Responses) (FR-006) ----

// chatToResponses converts a Chat Completions result into Responses
// output items: text content becomes an assistant output_text message
// item, tool_calls become function_call items, finish_reason length maps
// to status incomplete, and prompt/completion tokens map to input/output
// usage (FR-006).
func chatToResponses(body []byte, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	resp, eErr := decodeCC(body)
	if eErr != nil {
		return nil, eErr
	}
	choice := resp.Choices[0]
	// Status derives positionally from finish alone (Responses-status
	// exemption): length→incomplete, else completed — matching the stream
	// sibling; tool calls are represented by output items, not status.
	status := shared.ResponseStatusFromCCFinish(choice.FinishReason)
	out := shared.ResponsesResult{
		ID:     resp.ID,
		Object: "response",
		Model:  resp.Model,
		Status: status,
		Output: []any{},
	}
	blocks, eErr := ccContentToBlocks(choice.Message.Content)
	if eErr != nil {
		return nil, eErr
	}
	// The shared assembler gives Chat Completions placement (no reserved
	// slot): the message leads and appears only when text is non-empty,
	// matching the streaming terminal (FR-006 sibling parity).
	oa := shared.NewOutputAssembler(resp.ID, tools...)
	for _, b := range blocks {
		if b["type"] != "text" {
			return nil, errclass.Translation(fmt.Sprintf(
				"%s output cannot represent %q content blocks", EndpointPath, b["type"]))
		}
		oa.AddText(b["text"].(string))
	}
	for _, tc := range choice.Message.ToolCalls {
		oa.AppendFunctionCall(tc.ID, tc.Function.Name, shared.DefaultArgs(tc.Function.Arguments))
	}
	out.Output = oa.Render()
	if choice.Message.ReasoningContent != "" {
		out.Output = append([]any{reasoningItem(resp.ID, choice.Message.ReasoningContent)}, out.Output...)
	}
	if resp.Usage != nil {
		var details shared.UsageDetails
		if resp.Usage.PromptDetails != nil {
			details.CachedTokens = resp.Usage.PromptDetails.CachedTokens
		}
		if resp.Usage.CompletionDetails != nil {
			details.ReasoningTokens = resp.Usage.CompletionDetails.ReasoningTokens
		}
		out.Usage = shared.NewResponsesUsageFrom(resp.Usage.PromptTokens, resp.Usage.CompletionTokens, details)
	}
	b, _ := json.Marshal(out) // only marshallable composed types; cannot fail
	return b, nil
}
