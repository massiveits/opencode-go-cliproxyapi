// Package chatcompletions implements the OpenCode Go Chat Completions
// protocol adapter (arch §8) for routes served via /v1/chat/completions:
// request conversion (FR-005), non-stream response conversion (FR-006),
// and SSE stream conversion (AC §B).
package chatcompletions

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/errclass"
	"opencode-go-cliproxyapi/internal/thinking"
)

// EndpointPath is the upstream OpenCode Go Chat Completions endpoint.
var EndpointPath = catalog.RouteChatCompletions.EndpointPath()

// AuthHeaders returns the FR-007 authorization headers for an OpenCode Go key.
func AuthHeaders(key string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+key)
	return h
}

// BuildRequest translates an inbound request body from sourceFormat
// ("openai" Chat Completions, "openai-response" Responses, "claude"
// Messages) into a Chat Completions body for upstreamModel (FR-005).
// ts carries the target model's thinking capability so reasoning
// budgets map to a supported effort level instead of being silently
// degraded (FR-005). Unknown formats are ClassUnsupported; malformed
// input is ClassTranslation. Errors are descriptive and redacted — no
// silent loss of tools or reasoning controls.
func BuildRequest(upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	switch sourceFormat {
	case "openai":
		return buildOpenAIRequest(upstreamModel, sourceBody, ts)
	case "claude":
		return claudeToChat(upstreamModel, sourceBody, ts)
	case "openai-response":
		return responsesToChat(upstreamModel, sourceBody, ts)
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// buildOpenAIRequest rewrites the top-level model field of an OpenAI request
// body to upstreamModel, normalizes role:"developer" messages to role:"system",
// and strips any malformed top-level thinking object. DeepSeek models fail if
// thinking lacks a valid string type field or if messages contain role:"developer".
func buildOpenAIRequest(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errclass.Translation("malformed openai request JSON: " + err.Error())
	}
	if req == nil {
		return nil, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	if raw := req["reasoning_effort"]; len(raw) > 0 && string(raw) != "null" {
		var effort string
		if json.Unmarshal(raw, &effort) != nil {
			return nil, errclass.Translation("reasoning_effort must be a string")
		}
		if effort != "" {
			if eErr := thinking.ValidateEffort(effort, ts); eErr != nil {
				return nil, eErr
			}
		}
	}
	rawThinking, hasThinking := req["thinking"]
	validThinking := hasThinking && isValidThinking(rawThinking)
	if hasThinking && !validThinking {
		delete(req, "thinking")
	}

	var msgsModified bool
	if rawMsgs, ok := req["messages"]; ok && strings.Contains(string(rawMsgs), `"developer"`) {
		var msgs []map[string]json.RawMessage
		if err := json.Unmarshal(rawMsgs, &msgs); err == nil {
			for _, m := range msgs {
				var role string
				if err := json.Unmarshal(m["role"], &role); err == nil && role == "developer" {
					m["role"] = json.RawMessage(`"system"`)
					msgsModified = true
				}
			}
			if msgsModified {
				if b, err := json.Marshal(msgs); err == nil {
					req["messages"] = b
				}
			}
		}
	}
	if raw, ok := req["model"]; ok && string(raw) == `"`+upstreamModel+`"` && (validThinking || !hasThinking) && !msgsModified {
		return body, nil
	}
	req["model"] = json.RawMessage(`"` + upstreamModel + `"`)

	b, err := json.Marshal(req)
	if err != nil {
		return nil, errclass.Translation("model id cannot be represented as JSON")
	}
	return b, nil
}

// isValidThinking reports whether raw is a JSON object with a non-empty string "type" field.
func isValidThinking(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return false
	}
	rawType, ok := obj["type"]
	if !ok {
		return false
	}
	var typeStr string
	if err := json.Unmarshal(rawType, &typeStr); err != nil {
		return false
	}
	return strings.TrimSpace(typeStr) != ""
}

type imageURLField struct {
	URL string `json:"url"`
}

type ccContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *imageURLField `json:"image_url,omitempty"`
}

type ccMessage struct {
	Role       string              `json:"role"`
	Content    any                 `json:"content"` // string, []ccContentPart, or nil
	ToolCalls  []shared.CCToolCall `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
}

type ccRequest struct {
	Model             string          `json:"model"`
	Messages          []ccMessage     `json:"messages"`
	MaxTokens         *int64          `json:"max_tokens,omitempty"`
	Stop              any             `json:"stop,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	Tools             []shared.CCTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
}

// encode finalizes and encodes a translated request; the composed types
// are always marshallable.
func encode(req *ccRequest) []byte {
	if req.Messages == nil {
		req.Messages = []ccMessage{}
	}
	b, _ := json.Marshal(req)
	return b
}

// ---- claude (Anthropic Messages) -> Chat Completions (FR-005, AC §B) ----

type claudeBlock = map[string]any

// claudeToChat translates an Anthropic Messages request into a Chat
// Completions request (FR-005): top-level system becomes a system
// message, text blocks become string content, tool_use blocks become
// assistant tool_calls, tool_result blocks become role:"tool" messages,
// max_tokens/stop_sequences/tools map to their CC equivalents
// (max_tokens defaulted by the shared kernel, FR-005), and the thinking
// budget maps to a capability-aware reasoning_effort via
// thinking.EffortFromBudget. Decoding is owned entirely by the shared
// Claude-request kernel; only target-shape rendering stays local.
func claudeToChat(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	src, eErr := shared.DecodeClaudeMessages(body)
	if eErr != nil {
		return nil, eErr
	}
	out := &ccRequest{
		Model:       upstreamModel,
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}
	out.MaxTokens = &src.MaxTokens
	if len(src.StopSequences) > 0 {
		out.Stop = src.StopSequences
	}
	if shared.ThinkingEnabled(src.Thinking) {
		out.ReasoningEffort = thinking.EffortFromBudget(src.Thinking.BudgetTokens, ts)
	}
	applyToolChoiceCC(out, src.ToolChoiceKind, src.ToolChoiceName)
	if src.System != "" {
		out.Messages = append(out.Messages, ccMessage{Role: "system", Content: src.System})
	}
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "user":
			msgs, eErr := claudeUserMessages(m)
			if eErr != nil {
				return nil, eErr
			}
			out.Messages = append(out.Messages, msgs...)
		case "assistant":
			msg, eErr := claudeAssistantMessage(m)
			if eErr != nil {
				return nil, eErr
			}
			if msg != nil {
				out.Messages = append(out.Messages, *msg)
			}
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
	}
	for _, t := range src.Tools {
		// Claude tools carry no type field; validation cannot reject here.
		schema := shared.ObjectSchema(t.InputSchema)
		out.Tools = append(out.Tools, shared.CCTool{
			Type:     "function",
			Function: shared.CCFunction{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	return encode(out), nil
}

// claudeResultText flattens tool_result content (string or text blocks)
// into a tool-message string via the shared kernel; non-text blocks have
// no tool-message equivalent and are rejected descriptively rather than
// dropped (FR-005).
func claudeResultText(content json.RawMessage) (string, *errclass.Error) {
	return shared.ToolResultText(content, "tool messages carry text only")
}

// claudeUserMessages converts a user turn into Chat Completions messages:
// text/image blocks become one user message (plain string when only
// text), each tool_result block becomes a separate role:"tool" message
// carrying its tool_use_id (FR-005).
func claudeUserMessages(m *shared.ClaudeMessageRecord) ([]ccMessage, *errclass.Error) {
	var msgs []ccMessage
	var parts []ccContentPart
	flush := func() {
		if len(parts) == 0 {
			return
		}
		var c any = parts
		if len(parts) == 1 && parts[0].Type == "text" {
			c = parts[0].Text
		}
		msgs = append(msgs, ccMessage{Role: "user", Content: c})
		parts = nil
	}
	if m.Content != "" {
		parts = append(parts, ccContentPart{Type: "text", Text: m.Content})
	}
	for i := range m.Blocks {
		blk := &m.Blocks[i]
		switch blk.Kind {
		case "text":
			parts = append(parts, ccContentPart{Type: "text", Text: blk.Text})
		case "image":
			parts = append(parts, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: blk.URL}})
		case "tool_result":
			text, eErr := claudeResultText(blk.Result)
			if eErr != nil {
				return nil, eErr
			}
			if blk.IsError {
				// Same convention as the Responses-route twin so the
				// flag is never silently erased (FR-005).
				text = shared.ToolResultErrorPrefix + text
			}
			flush()
			msgs = append(msgs, ccMessage{
				Role: "tool", Content: text, ToolCallID: blk.CallID,
			})
		default:
			return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
		}
	}
	flush()
	return msgs, nil
}

// claudeAssistantMessage converts an assistant turn: text blocks join
// the message content, tool_use blocks become tool_calls. Historical
// thinking and redacted_thinking blocks have no Chat Completions
// representation and are omitted by the FR-005 explicit omission policy
// (the forward-looking control maps via effortFromBudget instead; the
// redacted variant is pure encrypted metadata). Returns nil for empty turns.
func claudeAssistantMessage(m *shared.ClaudeMessageRecord) (*ccMessage, *errclass.Error) {
	msg := &ccMessage{Role: "assistant"}
	var sb strings.Builder
	sb.WriteString(m.Content)
	for i := range m.Blocks {
		blk := &m.Blocks[i]
		switch blk.Kind {
		case "text":
			sb.WriteString(blk.Text)
		case "thinking", "redacted_thinking":
			// omitted: no Chat Completions equivalent (FR-005 policy)
		case "tool_use":
			tc := shared.CCToolCall{ID: blk.CallID, Type: "function"}
			tc.Function.Name = blk.Name
			tc.Function.Arguments = shared.DefaultArgs(string(blk.Input))
			msg.ToolCalls = append(msg.ToolCalls, tc)
		default:
			return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
		}
	}
	if sb.Len() > 0 {
		msg.Content = sb.String()
	}
	if msg.Content == nil && len(msg.ToolCalls) == 0 {
		return nil, nil
	}
	return msg, nil
}

// ---- openai-response (Responses) -> Chat Completions (FR-005) ----

// responsesToChat translates an OpenAI Responses request into a Chat
// Completions request (FR-005): instructions and system items become
// system messages, message items map by role, function_call items merge
// into assistant tool_calls, function_call_output items become role:"tool"
// messages, reasoning.effort maps to reasoning_effort,
// parallel_tool_calls passes through as-is (the reverse leg forwards the
// same field), and max_output_tokens maps to max_tokens. Historical
// reasoning items are omitted (no CC equivalent; FR-005 explicit omission
// policy).
func responsesToChat(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	var src shared.ResponsesRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed openai-response request JSON: " + err.Error())
	}
	out := &ccRequest{
		Model:             upstreamModel,
		Stream:            src.Stream,
		Temperature:       src.Temperature,
		TopP:              src.TopP,
		ParallelToolCalls: src.ParallelToolCalls,
	}
	if src.MaxOutputTokens != nil && *src.MaxOutputTokens > 0 {
		out.MaxTokens = src.MaxOutputTokens
	}
	if src.Reasoning != nil && src.Reasoning.Effort != "" {
		if eErr := thinking.ValidateEffort(src.Reasoning.Effort, ts); eErr != nil {
			return nil, eErr
		}
		out.ReasoningEffort = strings.ToLower(strings.TrimSpace(src.Reasoning.Effort))
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	applyToolChoiceCC(out, kind, tcName)

	addSystem := func(text string) {
		if text != "" {
			out.Messages = append(out.Messages, ccMessage{Role: "system", Content: text})
		}
	}
	instr, eErr := src.DecodeInstructions()
	if eErr != nil {
		return nil, eErr
	}
	addSystem(instr)

	items, eErr := src.DecodeInputItems()
	if eErr != nil {
		return nil, eErr
	}
	for _, item := range items {
		switch item.Type {
		case "message":
			content, eErr := respContent(item.Content)
			if eErr != nil {
				return nil, eErr
			}
			switch item.Role {
			case "system", "developer":
				// Flatten to text; images have no system-message
				// equivalent and are rejected descriptively (FR-005).
				text := ""
				if s, ok := content.(string); ok {
					text = s
				} else if parts, ok := content.([]ccContentPart); ok {
					var sb strings.Builder
					for _, p := range parts {
						if p.Type != "text" {
							return nil, shared.SystemImageRejected()
						}
						sb.WriteString(p.Text)
					}
					text = sb.String()
				}
				addSystem(text)
			case "user", "assistant":
				if content != nil {
					out.Messages = append(out.Messages, ccMessage{Role: item.Role, Content: content})
				}
			default:
				return nil, shared.ValidateRole(item.Role, EndpointPath)
			}
		case "function_call":
			tc := shared.CCToolCall{ID: item.CallID, Type: "function"}
			tc.Function.Name = item.Name
			tc.Function.Arguments = shared.DefaultArgs(item.Arguments)
			// Merge consecutive function_call items into one
			// assistant message so multi-call turns round-trip.
			if n := len(out.Messages); n > 0 {
				last := &out.Messages[n-1]
				if last.Role == "assistant" && last.Content == nil {
					last.ToolCalls = append(last.ToolCalls, tc)
					continue
				}
			}
			out.Messages = append(out.Messages, ccMessage{
				Role: "assistant", ToolCalls: []shared.CCToolCall{tc},
			})
		case "function_call_output":
			out.Messages = append(out.Messages, ccMessage{
				Role: "tool", Content: item.Output, ToolCallID: item.CallID,
			})
		case "reasoning":
			// omitted: no Chat Completions equivalent (FR-005 policy)
		default:
			return nil, shared.UnsupportedInputItemType(item.Type)
		}
	}

	for _, t := range src.Tools {
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		schema := shared.ObjectSchema(t.Parameters)
		out.Tools = append(out.Tools, shared.CCTool{
			Type:     "function",
			Function: shared.CCFunction{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	return encode(out), nil
}

// respContent normalizes a Responses content field (JSON string or typed
// parts) into Chat Completions content: a plain string, content parts,
// or nil when empty (FR-005 multimodal preservation). The shared kernel
// owns validation and the empty-input policy — empty-text messages drop
// identically on every route (F5 parity).
func respContent(raw json.RawMessage) (any, *errclass.Error) {
	parts, eErr := shared.DecodeStringOrParts(raw, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	switch {
	case len(parts) == 0:
		return nil, nil
	case len(parts) == 1 && parts[0].ImageURL == "":
		return parts[0].Text, nil
	}
	out := make([]ccContentPart, 0, len(parts))
	for _, p := range parts {
		if p.ImageURL != "" {
			out = append(out, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: p.ImageURL}})
			continue
		}
		out = append(out, ccContentPart{Type: "text", Text: p.Text})
	}
	return out, nil
}

// applyToolChoiceCC maps a normalized tool choice onto the outbound Chat
// Completions request, which natively represents every normalized kind
// (FR-005). It is infallible: shared.DecodeToolChoice emits exactly the
// closed set below, so the switch covers every possible kind (the
// exhaustiveness test in request_test.go enforces this against
// shared.AllToolChoiceKinds); absent omits the field.
func applyToolChoiceCC(req *ccRequest, kind, name string) {
	switch kind {
	case shared.ToolChoiceAuto:
		req.ToolChoice = json.RawMessage(`"auto"`)
	case shared.ToolChoiceAny:
		req.ToolChoice = json.RawMessage(`"required"`)
	case shared.ToolChoiceNone:
		req.ToolChoice = json.RawMessage(`"none"`)
	case shared.ToolChoiceNamed:
		b, _ := json.Marshal(map[string]any{
			"type": "function", "function": map[string]any{"name": name},
		})
		req.ToolChoice = b
	}
}
