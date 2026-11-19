// Package shared holds the wire helpers duplicated across the three
// protocol adapters: model-ID rewriting, tool-argument decoding, image
// source conversion, SSE framing, and redacted snippets.
package shared

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"opencode-go-cliproxyapi/internal/errclass"
)

// SSEFramer buffers SSE bytes across chunks and yields complete frames.
// Multiple data: lines within one frame join with "\n" per the SSE spec;
// comments, heartbeats, and stray blank lines are skipped; a frame is
// emitted only when at least one event:/data: line was seen. Raw blocks
// are captured verbatim (including line terminators) only when wantRaw is
// set — pass false when no consumer reads them.
type SSEFramer struct {
	buf       []byte          // partial line carried across Push calls
	eventType string          // current event: value
	data      strings.Builder // joined data: payloads of the current frame
	sawFrame  bool            // event:/data: seen since last emit
	wantRaw   bool
	raw       []byte // verbatim current block when wantRaw
}

// NewSSEFramer prepares a framer; wantRaw enables verbatim block capture.
func NewSSEFramer(wantRaw bool) *SSEFramer {
	return &SSEFramer{wantRaw: wantRaw}
}

// Push appends one network chunk to the framer buffer.
func (f *SSEFramer) Push(chunk []byte) {
	f.buf = append(f.buf, chunk...)
}

// Next returns the next complete frame (eventType, joined data, raw block).
// ok=false means the buffer needs more input.
func (f *SSEFramer) Next() (eventType, data string, raw []byte, ok bool) {
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			return "", "", nil, false
		}
		line := f.buf[:i] // excludes \n, may include \r
		f.buf = f.buf[i+1:]
		if f.wantRaw {
			f.raw = append(f.raw, line...)
			f.raw = append(f.raw, '\n')
		}
		text := strings.TrimSuffix(string(line), "\r")
		switch {
		case text == "":
			if !f.sawFrame {
				continue // stray blank line between frames
			}
			eventType, data, raw = f.eventType, f.data.String(), f.raw
			f.eventType = ""
			f.data.Reset()
			f.sawFrame = false
			f.raw = nil
			return eventType, data, raw, true
		case strings.HasPrefix(text, ":"):
			// SSE comment/heartbeat: raw capture only.
		case strings.HasPrefix(text, "event:"):
			f.eventType = strings.TrimSpace(strings.TrimPrefix(text, "event:"))
			f.sawFrame = true
		case strings.HasPrefix(text, "data:"):
			if f.data.Len() > 0 {
				f.data.WriteByte('\n')
			}
			f.data.WriteString(strings.TrimPrefix(strings.TrimPrefix(text, "data:"), " "))
			f.sawFrame = true
		default:
			// Unknown lines are ignored outside raw capture.
		}
	}
}

// RewriteModelID rewrites the top-level model field of a native-format
// request body to the bare upstream ID. The body decodes ONCE one level
// into json.RawMessage values, so every nested value passes through
// byte-exact — notably large integers that a float64 roundtrip would
// mangle (FR-005 fidelity). When the decoded model field's raw bytes
// already equal the quoted upstream ID (the common bare-ID/prefix client
// case), the original body is returned untouched before any mutation —
// a pure no-op that skips the remarshal. The comparison target mirrors
// the rewrite quoting below, and a decoded RawMessage always holds a
// valid JSON value, so an upstream id that cannot be represented
// (quotes/backslashes/control characters) can never match here and still
// reaches the rewrite's error. Duplicate keys follow encoding/json
// semantics: the LAST occurrence wins. formatLabel names the source
// protocol in the malformed-JSON error.
func RewriteModelID(upstreamModel string, body []byte, formatLabel string) ([]byte, *errclass.Error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errclass.Translation("malformed " + formatLabel + " request JSON: " + err.Error())
	}
	// A literal JSON null body unmarshals without error into a NIL map;
	// assigning the model field would panic, so reject it as malformed.
	if req == nil {
		return nil, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	if raw, ok := req["model"]; ok && string(raw) == `"`+upstreamModel+`"` {
		return body, nil
	}
	req["model"] = json.RawMessage(`"` + upstreamModel + `"`)
	b, err := json.Marshal(req)
	if err != nil {
		// A catalog model id containing quotes/backslashes/control
		// characters cannot be represented as a JSON string value.
		return nil, errclass.Translation("model id cannot be represented as JSON")
	}
	return b, nil
}

// HasContent reports whether a raw content/instructions/arguments field is
// present (non-empty and not JSON null); it is the presence test used
// before string-or-parts decoding.
func HasContent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// DefaultArgs applies the FR-005/FR-006 absent-tool-arguments policy:
// whitespace-only or JSON-null arguments become an empty JSON object so
// every tool call carries valid arguments.
func DefaultArgs(args string) string {
	s := strings.TrimSpace(args)
	if s == "" || s == "null" {
		return "{}"
	}
	return args
}

// emptyObjectSchema is the normalized parameter schema for tools declared
// without one.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// ObjectSchema normalizes an absent or null parameter schema to the empty
// object schema; a present schema passes through untouched.
func ObjectSchema(raw json.RawMessage) json.RawMessage {
	if !HasContent(raw) {
		return emptyObjectSchema
	}
	return raw
}

// ToolChoice kinds returned by DecodeToolChoice: "absent" (field missing
// or null), "auto", "any" (any/required), "none", "named" (forced single
// tool, name carries the tool name).
const (
	ToolChoiceAbsent = "absent"
	ToolChoiceAuto   = "auto"
	ToolChoiceAny    = "any"
	ToolChoiceNone   = "none"
	ToolChoiceNamed  = "named"
)

// AllToolChoiceKinds enumerates every ToolChoice* kind DecodeToolChoice
// returns, in kind order (absent/auto/any/none/named). Consumers'
// exhaustiveness tests must iterate this slice; add a kind here first.
var AllToolChoiceKinds = []string{
	ToolChoiceAbsent,
	ToolChoiceAuto,
	ToolChoiceAny,
	ToolChoiceNone,
	ToolChoiceNamed,
}

// DecodeToolChoice classifies a tool_choice value across the Chat
// Completions ({type:function,function:{name}}), Anthropic
// ({type:tool,name}), and Responses (string or {type:function,name})
// shapes into a normalized kind plus forced-tool name. Unknown types are
// a translation failure — tool_choice is never silently dropped (FR-005).
func DecodeToolChoice(raw json.RawMessage) (kind, name string, eErr *errclass.Error) {
	if !HasContent(raw) {
		return ToolChoiceAbsent, "", nil
	}
	var tc struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		if s := strings.TrimSpace(string(raw)); s == `"auto"` || s == `"any"` ||
			s == `"required"` || s == `"none"` {
			switch strings.Trim(s, `"`) {
			case "auto":
				return ToolChoiceAuto, "", nil
			case "any", "required":
				return ToolChoiceAny, "", nil
			default:
				return ToolChoiceNone, "", nil
			}
		}
		return "", "", errclass.Translation("malformed tool_choice: " + err.Error())
	}
	name = tc.Function.Name
	if name == "" {
		name = tc.Name
	}
	switch tc.Type {
	case "auto":
		return ToolChoiceAuto, "", nil
	case "any", "required":
		return ToolChoiceAny, "", nil
	case "none":
		return ToolChoiceNone, "", nil
	case "tool", "function":
		if name == "" {
			return "", "", errclass.Translation("malformed tool_choice: named tool requires a name")
		}
		return ToolChoiceNamed, name, nil
	}
	return "", "", errclass.Translation(fmt.Sprintf("unsupported tool_choice type %q", tc.Type))
}

// DecodeArgs parses tool-call arguments JSON; empty arguments decode to an
// empty object so every tool call carries valid input (FR-005/FR-006:
// tool calls are never silently dropped).
//
// Accepted ceiling: cross-format legs round-trip args through float64;
// integers > 2^53 lose precision — Responses passthrough preserves bytes.
// Upgrade path: carry json.RawMessage end-to-end.
func DecodeArgs(args string) (any, *errclass.Error) {
	args = DefaultArgs(args)
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return nil, errclass.Translation("malformed tool call arguments JSON: " + err.Error())
	}
	return v, nil
}

// ImageSource converts an image URL into the Anthropic image block
// source. Only data URLs whose media suffix ends in ";base64" decode to
// base64 sources; every other URL — including percent-encoded or plain
// data URLs — passes through verbatim as a url source so the encoding
// stays faithful and an upstream that cannot render it errors descriptively.
func ImageSource(url string) (map[string]any, *errclass.Error) {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		comma := strings.Index(rest, ",")
		if comma < 0 {
			return nil, errclass.Translation("malformed data URL: missing ',' separator")
		}
		if strings.HasSuffix(rest[:comma], ";base64") {
			media := strings.TrimSuffix(rest[:comma], ";base64")
			if media == "" {
				media = "application/octet-stream"
			}
			return map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "media_type": media, "data": rest[comma+1:],
				},
			}, nil
		}
	}
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": url},
	}, nil
}

// ClaudeStopToFinish maps Anthropic Messages stop reasons to Chat
// Completions finish reasons (FR-006). One table serves the messages
// adapter's streaming and non-stream paths so they cannot diverge:
// refusal maps to content_filter, and unmapped reasons fall back to stop
// by the FR-006 best-effort policy. The pair with FinishToClaudeStop is
// a symmetric round-trip (refusal ↔ content_filter).
func ClaudeStopToFinish(stop string) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, unknown
		return "stop"
	}
}

// FinishToClaudeStop is the inverse of ClaudeStopToFinish: Chat
// Completions finish reasons to Messages stop reasons, served from this
// one shared table so the chatcompletions adapter's streaming and
// non-stream paths match it exactly. content_filter maps to refusal so a
// mapped reason round-trips; unmapped reasons fall back to end_turn by
// the FR-006 best-effort policy.
func FinishToClaudeStop(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default: // stop, stop_sequence, unknown
		return "end_turn"
	}
}

// TerminalReason resolves a converted result's terminal stop/finish
// reason when two signals exist (FR-006): observed tool calls outrank the
// status-derived reason, so a terminal "incomplete" cannot downgrade
// tool_calls to length. toolReason/statusReason carry the target
// protocol's vocabulary ("tool_calls"/"length" for Chat Completions,
// "tool_use"/"max_tokens" for Messages). Responses-target STATUS fields
// are exempt — status derives positionally (truncation→incomplete) because
// tool calls are represented by output items, not status vocabulary;
// applies to CC finish_reason and Claude stop_reason vocabularies only.
func TerminalReason(toolCallsSeen bool, toolReason, statusReason string) string {
	if toolCallsSeen {
		return toolReason
	}
	return statusReason
}

// ResponseStatusFromCCFinish maps a Chat Completions finish_reason into
// the Responses status vocabulary (FR-006): only "length" is truncation
// and maps to incomplete; every other reason — including unknown or
// absent values — completes the response. Shared by the non-streaming
// converter and the stream terminal so the mapping cannot diverge across
// the sibling routes (FR-005/FR-006 consistency).
func ResponseStatusFromCCFinish(finish string) string {
	if finish == "length" {
		return "incomplete"
	}
	return "completed"
}

// ResponseStatusFromClaudeStop maps a Messages stop_reason into the
// Responses status vocabulary (FR-006): only "max_tokens" is truncation
// and maps to incomplete; every other reason — including unknown or
// absent values — completes the response. Shared by the non-streaming
// converter and the stream message_stop handler so the mapping cannot
// diverge across the sibling routes (FR-005/FR-006 consistency).
func ResponseStatusFromClaudeStop(stop string) string {
	if stop == "max_tokens" {
		return "incomplete"
	}
	return "completed"
}

// CCFinishFromResponseStatus maps a Responses result status back to the
// Chat Completions finish_reason vocabulary (inverse of
// ResponseStatusFromCCFinish) so synthesized non-Responses output cannot
// drift from the forward mapping.
func CCFinishFromResponseStatus(status string) string {
	if status == "incomplete" {
		return "length"
	}
	return "stop"
}

// ClaudeStopFromResponseStatus maps a Responses result status back to the
// Anthropic stop_reason vocabulary (inverse of
// ResponseStatusFromClaudeStop) so synthesized non-Responses output cannot
// drift from the forward mapping.
func ClaudeStopFromResponseStatus(status string) string {
	if status == "incomplete" {
		return "max_tokens"
	}
	return "end_turn"
}

// ClaudeMaxTokens applies the FR-005 max_tokens policy shared by every
// Claude-source converter: a positive value passes through, and an
// absent, zero, or negative value defaults to 4096 instead of failing
// translation or silently dropping the cap. One kernel serves all legs
// so identical Anthropic requests cannot diverge between rejecting,
// unbounding, and defaulting depending on the routed model.
func ClaudeMaxTokens(maxTokens int64) int64 {
	if maxTokens > 0 {
		return maxTokens
	}
	return 4096
}

// ClaudeSystemText renders the Anthropic Messages top-level system field
// (JSON string or text-block array) as instruction text (FR-005). Text
// blocks join with blank lines; non-text blocks are rejected descriptively
// rather than dropped. One kernel serves every adapter translating a
// Claude system field so separator and rejection policy cannot diverge.
func ClaudeSystemText(raw json.RawMessage) (string, *errclass.Error) {
	if !HasContent(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errclass.Translation("malformed system field")
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type != "text" {
			return "", errclass.Translation(fmt.Sprintf("unsupported system block type %q", blk.Type))
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(blk.Text)
	}
	return b.String(), nil
}

// ToolResultText flattens Claude tool_result content (JSON string or text
// block array) into one text string for the target protocol's tool-result
// field. Non-text blocks have no textual representation and are rejected
// descriptively rather than dropped (FR-005); targetNoun names the target
// wire vocabulary so each serving route keeps its own wording ("tool
// messages carry text only", "function_call_output carries text only").
// One kernel serves both adapters so the flatten and rejection policy
// cannot diverge again.
func ToolResultText(raw json.RawMessage, targetNoun string) (string, *errclass.Error) {
	if !HasContent(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ToolName string `json:"tool_name"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errclass.Translation("tool_result content must be a string or an array of blocks")
	}
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			b.WriteString(blk.Text)
		case "tool_reference":
			if blk.ToolName != "" {
				b.WriteString(fmt.Sprintf("[tool: %s]", blk.ToolName))
			} else {
				b.WriteString("[tool reference]")
			}
		default:
			return "", errclass.Translation(fmt.Sprintf(
				"unsupported tool_result block type %q; %s", blk.Type, targetNoun))
		}
	}
	return b.String(), nil
}

// ToolResultParts preserves images while using ToolResultText for every
// non-image block, including tool_reference markers. Images use the same
// source validation as ordinary Claude message content.
func ToolResultParts(raw json.RawMessage) ([]ContentPart, *errclass.Error) {
	if !HasContent(raw) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []ContentPart{{Text: text}}, nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, errclass.Translation("tool_result content must be a string or an array of blocks")
	}
	parts := make([]ContentPart, 0, len(blocks))
	for _, block := range blocks {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(block, &kind) != nil {
			return nil, errclass.Translation("malformed content block")
		}
		single := make(json.RawMessage, 0, len(block)+2)
		single = append(single, '[')
		single = append(single, block...)
		single = append(single, ']')
		if kind.Type == "image" {
			images, eErr := decodeClaudeBlocks(single)
			if eErr != nil {
				return nil, eErr
			}
			parts = append(parts, ContentPart{ImageURL: images[0].URL})
		} else {
			text, eErr := ToolResultText(single, "tool results support text, tool references and images")
			if eErr != nil {
				return nil, eErr
			}
			parts = append(parts, ContentPart{Text: text})
		}
	}
	return parts, nil
}

// RespOutputText flattens Responses function_call_output output content (JSON
// string or content part array) into one string for target protocol tool results.
// It accepts part types "input_text" and "text", and returns a translation error
// naming targetNoun on unsupported block types.
func RespOutputText(raw json.RawMessage, targetNoun string) (string, *errclass.Error) {
	if !HasContent(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errclass.Translation("tool output must be a string or an array of content parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "input_text" && p.Type != "output_text" && p.Type != "text" {
			return "", errclass.Translation(fmt.Sprintf(
				"unsupported tool output part type %q; %s", p.Type, targetNoun))
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}


// ClaudeImageURL converts an Anthropic image block source into an image
// URL for OpenAI-style targets, encoding base64 sources as data URLs
// (FR-005 multimodal preservation). Strict validation shared by every
// call site: url sources need a url, base64 sources need data, and
// unknown source types are named descriptively — never a silently
// corrupt data: URL.
func ClaudeImageURL(srcType, url, mediaType, data string) (string, *errclass.Error) {
	switch srcType {
	case "url":
		if url == "" {
			return "", errclass.Translation("image source missing url")
		}
		return url, nil
	case "base64":
		if data == "" {
			return "", errclass.Translation("base64 image source missing data")
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return "data:" + mediaType + ";base64," + data, nil
	default:
		return "", errclass.Translation(fmt.Sprintf("unsupported image source type %q", srcType))
	}
}

// SSEEvent renders one named SSE frame.
func SSEEvent(name string, v any) []byte {
	b, _ := json.Marshal(v) // composed marshallable types only; cannot fail
	out := make([]byte, 0, len("event: ")+len(name)+len("\ndata: ")+len(b)+2)
	out = append(out, "event: "...)
	out = append(out, name...)
	out = append(out, "\ndata: "...)
	out = append(out, b...)
	return append(out, '\n', '\n')
}

// IsSSEDone reports whether a framer-stripped SSE data payload is the
// terminal [DONE] sentinel; surrounding whitespace and CR are ignored.
func IsSSEDone(payload string) bool {
	return strings.TrimSpace(payload) == "[DONE]"
}

// ChatChunkBuilder synthesizes chat.completion.chunk frames with one
// shared shape so every openai-target stream synthesizer renders
// identical envelopes (FR-006): role chunks carry role plus empty
// content, an empty finish renders null, and usage is attached only when
// provided. A zero Created defaults to render-time now.
type ChatChunkBuilder struct {
	ID      string
	Model   string
	Created int64
}

// RoleChunk renders the leading assistant-role chunk.
func (b ChatChunkBuilder) RoleChunk() []byte {
	return b.frame(map[string]any{"role": "assistant", "content": ""}, nil, nil)
}

// Delta renders an intermediate delta chunk (null finish_reason).
func (b ChatChunkBuilder) Delta(delta any) []byte {
	return b.frame(delta, nil, nil)
}

// Finish renders the terminal chunk; an empty finish renders null.
func (b ChatChunkBuilder) Finish(finish string, usage map[string]any) []byte {
	var fr any
	if finish != "" {
		fr = finish
	}
	return b.frame(map[string]any{}, fr, usage)
}

func (b ChatChunkBuilder) frame(delta, finish any, usage map[string]any) []byte {
	created := b.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	frame := map[string]any{
		"id":      b.ID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   b.Model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		frame["usage"] = usage
	}
	raw, _ := json.Marshal(frame)
	return raw
}

// ClaudeEventEmitter renders canonical Anthropic Messages SSE frames so
// every claude-target stream synthesizer emits byte-identical shapes
// regardless of serving route (FR-006): every frame carries a named
// event: line, message_start usage always includes output_tokens:0
// beside input_tokens, and message_delta omits stop_sequence entirely
// (a null member breaks event-line-dispatching clients). Native
// Anthropic frames carry no created/timestamp value, so none is stored.
type ClaudeEventEmitter struct {
	id    string
	model string
}

// NewClaudeEventEmitter binds the emitter to the stream identity.
func NewClaudeEventEmitter(id, model string) ClaudeEventEmitter {
	return ClaudeEventEmitter{id: id, model: model}
}

// MessageStart opens the stream with an empty content list.
func (e ClaudeEventEmitter) MessageStart(inputTokens int64) []byte {
	return SSEEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.id, "type": "message", "role": "assistant",
			"model": e.model, "content": []any{},
			"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})
}

// ContentBlockStart announces one content block; extra carries the
// block-type-specific fields (text, or id/name/input for tool_use).
func (e ClaudeEventEmitter) ContentBlockStart(index int, blockType string, extra map[string]any) []byte {
	block := make(map[string]any, len(extra)+1)
	block["type"] = blockType
	for k, v := range extra {
		block[k] = v
	}
	return SSEEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": index, "content_block": block,
	})
}

// ContentBlockDelta streams one partial delta (text_delta or
// input_json_delta).
func (e ClaudeEventEmitter) ContentBlockDelta(index int, delta map[string]any) []byte {
	return SSEEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index, "delta": delta,
	})
}

// ContentBlockStop closes one content block.
func (e ClaudeEventEmitter) ContentBlockStop(index int) []byte {
	return SSEEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
}

// MessageDelta renders the terminal delta; a nil stopReason omits
// stop_reason, and stop_sequence is never emitted. A nil usage map omits
// usage; both serving routes always attach it.
func (e ClaudeEventEmitter) MessageDelta(stopReason *string, usage map[string]any) []byte {
	delta := map[string]any{}
	if stopReason != nil {
		delta["stop_reason"] = *stopReason
	}
	payload := map[string]any{"type": "message_delta", "delta": delta}
	if usage != nil {
		payload["usage"] = usage
	}
	return SSEEvent("message_delta", payload)
}

// MessageStop terminates the stream.
func (e ClaudeEventEmitter) MessageStop() []byte {
	return SSEEvent("message_stop", map[string]any{"type": "message_stop"})
}

// ResponsesEventEmitter renders canonical OpenAI Responses SSE frames so
// every openai-response stream synthesizer emits byte-identical shapes
// regardless of serving route (FR-006) — the sibling of
// ClaudeEventEmitter/ChatChunkBuilder. Native Responses responses carry a
// creation timestamp synthesizers never observe, so Created falls back to
// render-time now (same policy as ChatChunkBuilder.Created); the terminal
// completed payload always carries the model, matching the non-stream
// ResponsesResult shape.
type ResponsesEventEmitter struct {
	ID    string // response identity carried by every event
	Model string // model rendered on the terminal completed payload
	Tools *ResponseTools
}

// Created renders the leading response.created announcement.
func (e ResponsesEventEmitter) Created() []byte {
	return SSEEvent("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": e.ID, "object": "response", "created_at": time.Now().Unix(), "status": "in_progress",
		},
	})
}

// ItemAdded announces one output item before its first delta; item carries
// the block-type-specific fields (message id/role/content, or function_call
// call_id/name/arguments per the canonical call_id-only shape).
func (e ResponsesEventEmitter) ItemAdded(outputIndex int, item map[string]any) []byte {
	if item["type"] == "function_call" {
		if name, ok := item["name"].(string); ok {
			id := e.Tools.Identity(name)
			if id.isCustom {
				item["type"] = "custom_tool_call"
				item["input"] = ""
				delete(item, "arguments")
			}
			item["name"] = id.name
			if id.namespace != "" {
				item["namespace"] = id.namespace
			}
		}
	}
	return SSEEvent("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": outputIndex, "item": item,
	})
}

// TextDelta streams one partial output_text delta referencing the
// announced message item by item_id and output_index.
func (e ResponsesEventEmitter) TextDelta(itemID string, outputIndex int, delta string) []byte {
	return SSEEvent("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": itemID, "output_index": outputIndex, "delta": delta,
	})
}

// ArgsDelta streams one partial function_call_arguments delta referencing
// the announced function_call item by item_id and output_index.
func (e ResponsesEventEmitter) ArgsDelta(itemID string, outputIndex int, delta string) []byte {
	return SSEEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": itemID, "output_index": outputIndex, "delta": delta,
	})
}

// InputDelta streams one partial custom_tool_call_input delta referencing
// the announced custom_tool_call item by item_id and output_index.
func (e ResponsesEventEmitter) InputDelta(itemID string, outputIndex int, delta string) []byte {
	return SSEEvent("response.custom_tool_call_input.delta", map[string]any{
		"type": "response.custom_tool_call_input.delta", "item_id": itemID, "output_index": outputIndex, "delta": delta,
	})
}

// InputDone renders the custom_tool_call_input completion for the
// announced custom_tool_call item, emitted before response.completed.
func (e ResponsesEventEmitter) InputDone(itemID string, outputIndex int, input string) []byte {
	return SSEEvent("response.custom_tool_call_input.done", map[string]any{
		"type": "response.custom_tool_call_input.done", "item_id": itemID, "output_index": outputIndex, "input": input,
	})
}

// CustomInputTail derives the newly-available custom tool input from the
// accumulated wrapped arguments: nothing is reportable until the
// accumulation unwraps past its JSON envelope, at which point only the
// tail beyond already-reported bytes returns. Callers advance *emitted
// past the returned tail, so raw {"input":" wrapper tokens never reach
// client input deltas while complete inputs still stream incrementally.
func CustomInputTail(accumulated string, emitted *int) string {
	unwrapped := UnwrapCustomToolInput(accumulated)
	if unwrapped == accumulated {
		return ""
	}
	if *emitted >= len(unwrapped) {
		return ""
	}
	tail := unwrapped[*emitted:]
	*emitted = len(unwrapped)
	return tail
}

// TextDone renders the output_text completion for the announced message
// item, emitted before response.completed.
func (e ResponsesEventEmitter) TextDone(itemID string, outputIndex int, text string) []byte {
	return SSEEvent("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "text": text,
	})
}

// ContentPartDone renders the content_part completion for the announced
// message item, emitted before response.completed.
func (e ResponsesEventEmitter) ContentPartDone(itemID string, outputIndex int, text string) []byte {
	return SSEEvent("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text},
	})
}

// ArgsDone renders the function_call_arguments completion for the
// announced function_call item, emitted before response.completed; name
// is included only when non-empty.
func (e ResponsesEventEmitter) ArgsDone(itemID string, outputIndex int, name, args string) []byte {
	if name != "" {
		name = e.Tools.Identity(name).name
	}
	payload := map[string]any{
		"type": "response.function_call_arguments.done", "item_id": itemID, "output_index": outputIndex, "arguments": args,
	}
	if name != "" {
		payload["name"] = name
	}
	return SSEEvent("response.function_call_arguments.done", payload)
}

// ItemDone renders the output_item completion for one rendered output
// item, emitted before response.completed.
func (e ResponsesEventEmitter) ItemDone(outputIndex int, item any) []byte {
	return SSEEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": outputIndex, "item": item,
	})
}

// Completed renders the terminal response.completed event: status from the
// route's status mapping, usage always attached (F-R6), and output items
// rendered by the caller through OutputAssembler.
func (e ResponsesEventEmitter) Completed(status string, usage ResponsesUsage, output []any) []byte {
	return SSEEvent("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": e.ID, "object": "response", "model": e.Model, "status": status,
			"usage": usage, "output": output,
		},
	})
}

// RedactedSnippet bearer-redacts and truncates a payload snippet for error
// messages — never an upstream body echo (FR-011).
func RedactedSnippet(s string) string {
	s = errclass.Redact(s)
	if len(s) > 256 {
		return s[:256] + "..."
	}
	return s
}

// snippetBound bounds redaction work: only the head of an oversized error
// body can reach the 256-char snippet, so every upstream >=400 site
// truncates to this size before scanning (FR-011).
const snippetBound = 4096

// extractErrorMessage attempts to extract a clean, human-readable error
// message from upstream JSON error bodies (e.g. OpenAI or Anthropic format).
func extractErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	switch v := probe.Error.(type) {
	case string:
		if v != "" {
			return strings.TrimSpace(v)
		}
	case map[string]any:
		if m, ok := v["message"].(string); ok && m != "" {
			return strings.TrimSpace(m)
		}
	}
	if probe.Message != "" {
		return strings.TrimSpace(probe.Message)
	}
	if probe.Detail != "" {
		return strings.TrimSpace(probe.Detail)
	}
	return ""
}

// UpstreamStatusError classifies an upstream >=400 response body into one
// kernel used by every adapter and the executor: the body is truncated to
// its head, decoded for human-readable error messages if JSON, or fallen
// back to a redacted snippet, then classified per §7 via errclass.FromStatus.
// One kernel keeps bounding and redaction from diverging across call sites.
func UpstreamStatusError(status int, body []byte) *errclass.Error {
	if len(body) > snippetBound {
		body = body[:snippetBound]
	}
	msg := extractErrorMessage(body)
	if msg == "" {
		msg = string(body)
	}
	return errclass.FromStatus(status, RedactedSnippet(msg))
}

// ResponsesUsage is the token-usage block of synthesized Responses
// results and terminal response.completed payloads.
type ResponsesUsage struct {
	InputTokens   int64                   `json:"input_tokens"`
	OutputTokens  int64                   `json:"output_tokens"`
	TotalTokens   int64                   `json:"total_tokens"`
	InputDetails  *ResponsesInputDetails  `json:"input_tokens_details,omitempty"`
	OutputDetails *ResponsesOutputDetails `json:"output_tokens_details,omitempty"`
}

type ResponsesInputDetails struct {
	CachedTokens     *int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
}

type ResponsesOutputDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

// UsageDetails carries only fields that were present in the source usage.
// Pointer values preserve an explicit zero from an absent field.
type UsageDetails struct {
	CachedTokens     *int64
	CacheWriteTokens *int64
	ReasoningTokens  *int64
}

func ClampSubtract(value int64, parts ...*int64) int64 {
	for _, part := range parts {
		if part != nil {
			value -= *part
		}
	}
	if value < 0 {
		return 0
	}
	return value
}

// NewResponsesUsageFrom builds the struct form of the Responses usage
// block with the majority sum rule: total_tokens is always input+output
// (FR-005/FR-006 consistency), never taken from or left zero beside
// nonzero counts. Shared by every adapter emitting a ResponsesUsage struct
// so the computation cannot diverge.
func NewResponsesUsageFrom(inputTokens, outputTokens int64, details UsageDetails) ResponsesUsage {
	usage := ResponsesUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}
	if details.CachedTokens != nil || details.CacheWriteTokens != nil {
		usage.InputDetails = &ResponsesInputDetails{
			CachedTokens: details.CachedTokens, CacheWriteTokens: details.CacheWriteTokens,
		}
	}
	if details.ReasoningTokens != nil {
		usage.OutputDetails = &ResponsesOutputDetails{ReasoningTokens: details.ReasoningTokens}
	}
	return usage
}

// CCUsageFrom renders the Chat Completions usage map with total_tokens
// computed as prompt+completion (FR-005/FR-006 consistency): the same
// majority sum rule as NewResponsesUsageFrom applied to the Chat
// Completions vocabulary. Shared by every adapter emitting a Chat
// Completions usage block so the computation cannot diverge.
func CCUsageFrom(promptTokens, completionTokens int64, details UsageDetails) map[string]any {
	usage := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	}
	if details.CachedTokens != nil {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": *details.CachedTokens}
	}
	if details.ReasoningTokens != nil {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": *details.ReasoningTokens}
	}
	return usage
}

// CCToolCallOpeningEntry renders the canonical OPENING tool_calls delta
// entry for a Chat Completions stream: OpenAI puts type on the FIRST
// delta entry only, so it carries index/id/type plus the function name
// and initial arguments fragment; continuation deltas for the same call
// are minimal ({index, function:{arguments}}) and must be built by the
// caller, not through this kernel.
func CCToolCallOpeningEntry(index int, callID, name, argsDelta string) map[string]any {
	return map[string]any{
		"index": index,
		"id":    callID,
		"type":  "function",
		"function": map[string]any{
			"name":      name,
			"arguments": argsDelta,
		},
	}
}

// CompletionEnvelope renders the top-level chat.completion envelope so
// every cross-format converter emits an identical shape (FR-006): one
// choice entry plus prompt/completion usage.
func CompletionEnvelope(id, model string, created int64, choices []map[string]any, usage map[string]any) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": choices,
		"usage":   usage,
	}
}

// NewClaudeResult renders the non-stream Anthropic Messages response
// envelope so every claude-target converter emits an identical shape
// regardless of serving route (FR-006) — the Messages sibling of
// CompletionEnvelope/ResponsesResult. One policy per field: content is
// always a JSON array (nil normalizes to []), usage always carries both
// counters, and stop_sequence is never emitted — neither cross-format
// source vocabulary (CC finish_reason, Responses status) carries a stop
// sequence, so the key stays absent instead of null, matching the
// streaming message_delta sibling that omits it entirely. Callers pass
// blocks whose builders already drop empty text parts, so no dead text
// block ships.
func NewClaudeResult(id, model, stopReason string, inputTokens, outputTokens int64, cacheRead, cacheCreation *int64, content []map[string]any) map[string]any {
	if content == nil {
		content = []map[string]any{}
	}
	return map[string]any{
		"id":          id,
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": stopReason,
		"usage":       ClaudeUsage(inputTokens, outputTokens, cacheRead, cacheCreation),
	}
}

func ClaudeUsage(input, output int64, cacheRead, cacheWrite *int64) map[string]any {
	usage := map[string]any{"input_tokens": input, "output_tokens": output}
	if cacheRead != nil {
		usage["cache_read_input_tokens"] = *cacheRead
	}
	if cacheWrite != nil {
		usage["cache_creation_input_tokens"] = *cacheWrite
	}
	return usage
}

// ResponsesResult is the non-stream Responses result body synthesized
// for openai-response clients; one shared shape keeps the Chat
// Completions and Messages routes byte-identical in structure
// (including the model field).
type ResponsesResult struct {
	ID     string         `json:"id"`
	Object string         `json:"object"`
	Model  string         `json:"model"`
	Status string         `json:"status"`
	Output []any          `json:"output"`
	Usage  ResponsesUsage `json:"usage"`
}

// RespTool is one Responses function tool.
type RespTool struct {
	Type        string          `json:"type"` // "function" or "namespace"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Tools       []RespTool      `json:"tools,omitempty"`
}

// ResponsesRequest decodes an inbound OpenAI Responses request body
// (superset of what each adapter reads). Instructions is a JSON string,
// Input a JSON string or item array.
type ResponsesRequest struct {
	Instructions      json.RawMessage `json:"instructions"`
	Input             json.RawMessage `json:"input"`
	MaxOutputTokens   *int64          `json:"max_output_tokens"`
	Tools             []RespTool      `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	Stream            bool            `json:"stream"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Reasoning         *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

// DecodeInstructions resolves the instructions field (FR-005): absent or
// null yields "", a JSON string decodes to its value, and any other
// shape is a translation failure. One kernel serves both Responses-source
// builders so the wording and policy cannot diverge again.
func (r *ResponsesRequest) DecodeInstructions() (string, *errclass.Error) {
	if !HasContent(r.Instructions) {
		return "", nil
	}
	var instr string
	if err := json.Unmarshal(r.Instructions, &instr); err != nil {
		return "", errclass.Translation("instructions must be a string")
	}
	return instr, nil
}

// DecodeInputItems decodes the input field into item form: an item array
// passes through, a plain JSON string surfaces as one synthesized
// user-message item carrying the raw string bytes — each builder's
// message pipeline renders it exactly as its former inline string branch
// did, including dropping the empty string — and absent or null decodes
// to zero items. Any other shape is a translation failure. One kernel owns
// the string-or-items discrimination for both Responses-source builders so
// it cannot diverge again.
func (r *ResponsesRequest) DecodeInputItems() ([]RespItem, *errclass.Error) {
	if !HasContent(r.Input) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(r.Input, &s); err == nil {
		return []RespItem{{Type: "message", Role: "user", Content: r.Input}}, nil
	}
	var items []RespItem
	if err := json.Unmarshal(r.Input, &items); err != nil {
		return nil, errclass.Translation("input must be a string or an array of items: " + err.Error())
	}
	for i := range items {
		if items[i].Type == "" && strings.TrimSpace(items[i].Role) != "" {
			items[i].Type = "message"
		}
	}
	return items, nil
}

// RespItem is one Responses input/output item (message, function_call,
// or function_call_output). ID carries the item identity of synthesized
// terminal message items (FR-006 sibling parity: the Chat Completions
// and Messages routes emit the same "id" the streaming
// response.output_item.added announcement used); function_call items stay
// call_id-only per the documented canonical shape.
type RespItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Input     string          `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Summary   []struct {
		Text string `json:"text"`
	} `json:"summary,omitempty"`
	Tools     []RespTool      `json:"tools,omitempty"`
}

// CCFunction is one Chat Completions tool function definition (decode and
// emit; omitempty keeps absent descriptions/parameters off the wire).
type CCFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// CCToolCall is one Chat Completions tool call (decode and emit).
type CCToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// CCMessage is one Chat Completions message.
type CCMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string or typed parts
	ToolCalls  []CCToolCall    `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// CCTool is one Chat Completions tool definition.
type CCTool struct {
	Type     string     `json:"type"`
	Function CCFunction `json:"function"`
}

// ChatCompletionsRequest decodes an inbound Chat Completions request
// (superset of what each adapter reads).
type ChatCompletionsRequest struct {
	Messages            []CCMessage     `json:"messages"`
	MaxTokens           *int64          `json:"max_tokens"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens"`
	Stop                json.RawMessage `json:"stop"`
	Tools               []CCTool        `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	Stream              bool            `json:"stream"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	ReasoningEffort     string          `json:"reasoning_effort"`
}

// JoinTexts concatenates the string values stored under "text" across
// content parts/blocks in order, skipping non-string members — one kernel
// for every adapter that flattens typed parts into plain text (FR-005).
func JoinTexts(parts []map[string]any) string {
	var b strings.Builder
	for _, p := range parts {
		if t, ok := p["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}

// ContentPart is one OpenAI-style content part decoded by
// DecodeStringOrParts: a text part (ImageURL empty) or an image part
// carrying its resolved URL.
type ContentPart struct {
	Text     string
	ImageURL string // resolved image URL; empty for text parts
}

// DecodeStringOrParts is THE shared string-or-parts kernel owning the
// empty-input policy for every target route (F5 route parity): an absent
// or null field, an empty JSON string, and empty-text parts all decode to
// zero parts, so identical inbound bodies drop identically instead of one
// route shipping dead parts upstream while siblings drop the message.
// Unknown part types are ClassUnsupported naming targetLabel; malformed
// shapes are ClassTranslation. One kernel serves responses/contentParts,
// messages/contentParts, and chatcompletions/respContent so the policy
// cannot diverge again.
func DecodeStringOrParts(raw json.RawMessage, targetLabel string) ([]ContentPart, *errclass.Error) {
	if !HasContent(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []ContentPart{{Text: s}}, nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errclass.Translation("malformed message content: " + err.Error())
	}
	var out []ContentPart
	for _, p := range parts {
		if !IsContentPartType(p.Type) {
			return nil, ValidateContentPart(p.Type, targetLabel)
		}
		if p.Type == "image_url" || p.Type == "input_image" {
			url, eErr := OpenAIImageURL(p.ImageURL)
			if eErr != nil {
				return nil, eErr
			}
			out = append(out, ContentPart{ImageURL: url})
			continue
		}
		if p.Text != "" { // uniform DROP policy: no dead text parts upstream
			out = append(out, ContentPart{Text: p.Text})
		}
	}
	return out, nil
}

// ClaudeBlocksFromOpenAI normalizes an OpenAI-style content field (JSON
// string or typed parts) into Anthropic content blocks preserving
// per-part order (FR-005 multimodal preservation): text parts become
// text blocks, image parts image sources. THE Claude-blocks kernel for
// every route emitting Anthropic blocks from openai-style content: it
// owns validation and the empty-input policy — empties dropped uniformly
// on every route (F5 parity), unknown types rejected descriptively
// naming target. One kernel serves messages/contentParts and
// chatcompletions/ccContentToBlocks so the policy cannot diverge again.
func ClaudeBlocksFromOpenAI(raw json.RawMessage, target string) ([]map[string]any, *errclass.Error) {
	parts, eErr := DecodeStringOrParts(raw, target)
	if eErr != nil {
		return nil, eErr
	}
	var blocks []map[string]any
	for _, p := range parts {
		if p.ImageURL != "" {
			src, eErr := ImageSource(p.ImageURL)
			if eErr != nil {
				return nil, eErr
			}
			blocks = append(blocks, src)
			continue
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
	}
	return blocks, nil
}

// ToolResultErrorPrefix marks tool results that arrived with is_error set;
// every Claude-source translator applies it so the flag is never silently
// erased (FR-005).
const ToolResultErrorPrefix = "[error] "

// ClaudeTool is one Anthropic Messages tool definition (decode-only).
type ClaudeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ClaudeThinking is the Anthropic extended-thinking request control
// (decode-only).
type ClaudeThinking struct {
	Type         string `json:"type"`
	BudgetTokens int64  `json:"budget_tokens"`
}

// ClaudeOutputConfig carries named effort independently of a thinking budget.
type ClaudeOutputConfig struct {
	Effort string `json:"effort"`
}

// NormalizedEffort returns the trimmed, lower-cased explicit effort, or ""
// when none was supplied.
func (c *ClaudeOutputConfig) NormalizedEffort() string {
	if c == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.Effort))
}

// ThinkingEnabled reports whether an Anthropic thinking control requests
// extended thinking: present with type "enabled" (FR-005). One kernel
// serves every adapter gating on the control so the check cannot diverge.
func ThinkingEnabled(t *ClaudeThinking) bool {
	return t != nil && t.Type == "enabled"
}

// ClaudeBlock is one normalized Anthropic Messages content block produced
// by DecodeClaudeMessages: Kind carries the wire "type" and only the
// fields that kind uses are populated. Image URLs arrive pre-resolved
// through ClaudeImageURL; tool_use Input and tool_result Result stay raw
// so each target renderer applies its own vocabulary and policies.
type ClaudeBlock struct {
	Kind    string          // text, image, tool_use, tool_result, thinking, redacted_thinking, or an unknown wire type
	Text    string          // text blocks
	URL     string          // image blocks: resolved http(s)/data URL
	CallID  string          // tool_use "id" / tool_result "tool_use_id"
	Name    string          // tool_use name
	Input   json.RawMessage // tool_use arguments (absent → nil)
	Result  json.RawMessage // tool_result payload (string or block array)
	IsError bool            // tool_result is_error flag
}

// ClaudeMessageRecord is one Anthropic Messages message turn: Content is
// set when the wire content was a JSON string, Blocks when it was a block
// array; absent/null content sets neither.
type ClaudeMessageRecord struct {
	Role    string
	Content string
	Blocks  []ClaudeBlock
}

// ClaudeRequestRecord is the normalized decode of an inbound Anthropic
// Messages request body (FR-005): envelope fields resolved once — max
// tokens defaulted per ClaudeMaxTokens, system flattened per
// ClaudeSystemText, tool_choice classified per DecodeToolChoice — so
// every Claude-source translator decodes identically and keeps only
// target-shape rendering local.
type ClaudeRequestRecord struct {
	MaxTokens      int64
	System         string
	Messages       []ClaudeMessageRecord
	StopSequences  []string
	Tools          []ClaudeTool
	ToolChoiceKind string
	ToolChoiceName string
	Thinking       *ClaudeThinking
	OutputConfig   *ClaudeOutputConfig
	Stream         bool
	Temperature    *float64
	TopP           *float64
}

// claudeWireMessage mirrors the inbound Anthropic message shape (decode-only).
type claudeWireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or block array
}

// claudeWireBlock mirrors the inbound Anthropic block shape (decode-only).
type claudeWireBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		Type      string `json:"type"` // base64 | url
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result payload: string or blocks
	IsError   bool            `json:"is_error"`
}

// DecodeClaudeMessages decodes an inbound Anthropic Messages request body
// into a ClaudeRequestRecord. It is THE Claude-request kernel for every
// adapter translating claude source (FR-005): one decoder owns malformed-
// input classification (all ClassTranslation) and wording — broken JSON,
// non-string/non-array message content, undecodable block elements, image
// sources (via ClaudeImageURL validation) — so the sibling routes cannot
// diverge again. Unknown block types and roles are recorded verbatim:
// rejecting them names the TARGET endpoint and stays with the renderer.
func DecodeClaudeMessages(body json.RawMessage) (*ClaudeRequestRecord, *errclass.Error) {
	var env struct {
		MaxTokens     int64               `json:"max_tokens"`
		System        json.RawMessage     `json:"system"`
		Messages      []claudeWireMessage `json:"messages"`
		StopSequences []string            `json:"stop_sequences"`
		Tools         []ClaudeTool        `json:"tools"`
		ToolChoice    json.RawMessage     `json:"tool_choice"`
		Thinking      *ClaudeThinking     `json:"thinking"`
		OutputConfig  *ClaudeOutputConfig `json:"output_config"`
		Stream        bool                `json:"stream"`
		Temperature   *float64            `json:"temperature"`
		TopP          *float64            `json:"top_p"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, errclass.Translation("malformed claude request JSON: " + err.Error())
	}
	kind, name, eErr := DecodeToolChoice(env.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	system, eErr := ClaudeSystemText(env.System)
	if eErr != nil {
		return nil, eErr
	}
	rec := &ClaudeRequestRecord{
		MaxTokens:      ClaudeMaxTokens(env.MaxTokens),
		System:         system,
		StopSequences:  env.StopSequences,
		Tools:          env.Tools,
		ToolChoiceKind: kind,
		ToolChoiceName: name,
		Thinking:       env.Thinking,
		OutputConfig:   env.OutputConfig,
		Stream:         env.Stream,
		Temperature:    env.Temperature,
		TopP:           env.TopP,
	}
	for _, wm := range env.Messages {
		m := ClaudeMessageRecord{Role: wm.Role}
		if HasContent(wm.Content) {
			var s string
			if json.Unmarshal(wm.Content, &s) == nil {
				m.Content = s
			} else {
				blocks, eErr := decodeClaudeBlocks(wm.Content)
				if eErr != nil {
					return nil, eErr
				}
				m.Blocks = blocks
			}
		}
		rec.Messages = append(rec.Messages, m)
	}
	return rec, nil
}

// decodeClaudeBlocks normalizes one block-array message content field.
// A well-formed JSON value that is neither string nor array fails with
// the canonical shape error; an undecodable element fails with the
// canonical block error — never silently dropped (FR-005).
func decodeClaudeBlocks(raw json.RawMessage) ([]ClaudeBlock, *errclass.Error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, errclass.Translation("message content must be a string or an array of blocks")
	}
	blocks := make([]ClaudeBlock, 0, len(elems))
	for _, elem := range elems {
		var wb claudeWireBlock
		if err := json.Unmarshal(elem, &wb); err != nil {
			return nil, errclass.Translation("malformed content block")
		}
		blk := ClaudeBlock{
			Kind: wb.Type, Text: wb.Text,
			Name: wb.Name, Input: wb.Input,
			Result: wb.Content, IsError: wb.IsError,
		}
		switch wb.Type {
		case "image":
			if wb.Source == nil {
				return nil, errclass.Translation("image block missing source")
			}
			url, eErr := ClaudeImageURL(wb.Source.Type, wb.Source.URL, wb.Source.MediaType, wb.Source.Data)
			if eErr != nil {
				return nil, eErr
			}
			blk.URL = url
		case "tool_use":
			blk.CallID = wb.ID
		case "tool_result":
			blk.CallID = wb.ToolUseID
		}
		blocks = append(blocks, blk)
	}
	return blocks, nil
}

// OpenAIImageURL extracts the image URL from an OpenAI-style content part:
// {"url": ...} object form or plain string form. An empty or absent URL is
// a translation failure.
func OpenAIImageURL(raw json.RawMessage) (string, *errclass.Error) {
	var url string
	if len(raw) > 0 {
		var obj struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.URL != "" {
			return obj.URL, nil
		}
		json.Unmarshal(raw, &url)
	}
	if url == "" {
		return "", errclass.Translation("image content part missing url")
	}
	return url, nil
}

// UnsupportedFormat reports a source protocol no translator in the calling
// adapter handles, naming the target endpoint.
func UnsupportedFormat(format, target string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("source protocol %q cannot be translated to %s", format, target),
	}
}

// UnsupportedPartType reports a client content-block/part type the target
// endpoint cannot represent (ClassUnsupported; FR-005 explicit policy).
func UnsupportedPartType(partType, target string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("unsupported content block type %q for %s", partType, target),
	}
}

// UnsupportedInputItemType reports an unrecognized Responses input item type
// (ClassUnsupported; FR-005 explicit policy).
func UnsupportedInputItemType(itemType string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("unsupported Responses input item type %q", itemType),
	}
}

// IsContentPartType reports whether an OpenAI-style content part type is
// accepted, covering both Chat Completions names (text, image_url) and
// their Responses-flavored aliases (input_text, output_text, input_image)
// so identical client bodies succeed on every route.
func IsContentPartType(partType string) bool {
	switch partType {
	case "text", "input_text", "output_text", "image_url", "input_image":
		return true
	}
	return false
}

// ValidateContentPart rejects an unknown OpenAI-style content part type
// as unsupported_protocol_or_parameter (FR-009; round-9 precedent:
// unsupported parameter values are unsupported, not malformed JSON),
// naming the target endpoint. One kernel serves all three adapters so
// the error class cannot diverge again.
func ValidateContentPart(partType, target string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("unsupported content part type %q for %s", partType, target),
	}
}

// ValidateRole rejects a message role no translator in the calling
// adapter handles, same FR-009 class and rationale as ValidateContentPart.
func ValidateRole(role, target string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("unsupported message role %q for %s", role, target),
	}
}

// SystemImageRejected reports image content inside a system/instruction
// message, which no target protocol can represent (FR-005). One kernel
// serves every adapter so the wording cannot diverge again.
func SystemImageRejected() *errclass.Error {
	return errclass.Translation("system messages cannot carry image content")
}

// outputTextPart is one content part of a synthesized assistant message
// item.
type outputTextPart struct {
	Type string `json:"type"` // always "output_text"
	Text string `json:"text"`
}

// OutputAssembler aggregates Responses output items — ONE assistant
// message item carrying all observed text plus function_call items in
// arrival order — for every Responses-output synthesis site, streaming and
// non-streaming, Messages-source and Chat-Completions-source alike
// (FR-006 mode/route parity). Placement collapses two former per-site
// mechanics into one rule: a site that called ReserveTextSlot gets the
// message at the first text observation's slot (filled even when the
// aggregated text is empty — Messages-source semantics); a site that
// never reserves gets the Chat Completions placement (message leads, and
// only when text is non-empty). Render materializes the array once, at
// terminal time.
type OutputAssembler struct {
	messageID string          // identity carried by the message item
	items     []any           // rendered items in insertion order
	textSlot  int             // reserved message position, -1 until reserved
	text      strings.Builder // aggregated message text
	tools     *ResponseTools  // wire-to-original tool identities for restoration
}

// NewOutputAssembler binds an assembler to the response identity the
// synthesized message item carries.
func NewOutputAssembler(messageID string, tools ...*ResponseTools) *OutputAssembler {
	a := &OutputAssembler{messageID: messageID, textSlot: -1, items: make([]any, 0)}
	if len(tools) > 0 && tools[0] != nil {
		a.tools = tools[0]
	}
	return a
}

// ReserveTextSlot pins the message item's position at the current end of
// the output array so later function_call items cannot displace it;
// idempotent — only the first call reserves.
func (a *OutputAssembler) ReserveTextSlot() {
	if a.textSlot >= 0 {
		return
	}
	a.textSlot = len(a.items)
	a.items = append(a.items, nil)
}

// AddText appends one text fragment to the aggregated message.
func (a *OutputAssembler) AddText(fragment string) {
	a.text.WriteString(fragment)
}

// AppendFunctionCall adds one function_call item in arrival order;
// arguments pass through verbatim — callers apply the absent-arguments
// policy themselves. Tools originally declared as type "custom" restore
// as custom_tool_call items with unwrapped input.
func (a *OutputAssembler) AppendFunctionCall(callID, name, args string) {
	id := a.tools.Identity(name)
	if id.isCustom {
		a.items = append(a.items, RespItem{
			Type: "custom_tool_call", CallID: callID, Name: id.name, Namespace: id.namespace, Input: UnwrapCustomToolInput(args),
		})
		return
	}
	a.items = append(a.items, RespItem{
		Type: "function_call", CallID: callID, Name: id.name, Namespace: id.namespace, Arguments: args,
	})
}

// Render fills the reserved slot (or leads with the message for sites
// that never reserved but observed non-empty text) and returns the
// output array.
func (a *OutputAssembler) Render() []any {
	t := a.text.String()
	if t != "" || a.textSlot >= 0 {
		content, _ := json.Marshal([]outputTextPart{{Type: "output_text", Text: t}})
		msg := RespItem{Type: "message", ID: a.messageID, Role: "assistant", Content: content}
		if a.textSlot >= 0 {
			a.items[a.textSlot] = msg
		} else {
			rest := a.items
			a.items = make([]any, 0, len(rest)+1)
			a.items = append(a.items, msg)
			a.items = append(a.items, rest...)
		}
	}
	return a.items
}

// FunctionTool validates one tool definition for translation. toolType
// must be "function" — anything else is unsupported_protocol_or_parameter
// (FR-009: a non-function tool type is an unsupported parameter) naming
// the target endpoint. Callers normalize or preserve the parameter schema
// themselves from their own raw field.
func FunctionTool(toolType, targetLabel string) *errclass.Error {
	if toolType != "function" {
		return &errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("unsupported tool type %q; only function tools translate to %s", toolType, targetLabel),
		}
	}
	return nil
}
