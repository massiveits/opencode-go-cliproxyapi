package messages

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/errclass"
)

func decodeReq(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("output not valid JSON object: %v", err)
	}
	return m
}

func TestAuthHeaders(t *testing.T) {
	h := AuthHeaders("secret-key")
	if got := h.Get("x-api-key"); got != "secret-key" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := h.Get("anthropic-version"); got != anthropicVersion {
		t.Errorf("anthropic-version = %q", got)
	}
	if h.Get("Authorization") != "" {
		t.Error("unexpected Authorization header")
	}
	if http.Header(nil).Get("x") != "" {
		t.Error("nil header sanity")
	}
}

func TestBuildRequestUnsupportedFormat(t *testing.T) {
	_, eErr := BuildRequest("m", "grpc", []byte(`{}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("want ClassUnsupported, got %+v", eErr)
	}
}

func TestBuildRequestClaude(t *testing.T) {
	body := []byte(`{"model":"opencode-go/minimax","max_tokens":10,"system":"s","stream":true}`)
	out, eErr := BuildRequest("minimax", "claude", []byte(body), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	m := decodeReq(t, out)
	if m["model"] != "minimax" {
		t.Errorf("model = %v", m["model"])
	}
	if m["max_tokens"] != float64(10) || m["system"] != "s" || m["stream"] != true {
		t.Errorf("passthrough fields lost: %v", m)
	}
}

func TestBuildRequestClaudeMalformed(t *testing.T) {
	_, eErr := BuildRequest("m", "claude", []byte(`{`), nil)
	if eErr == nil || eErr.Class != errclass.ClassTranslation {
		t.Fatalf("want ClassTranslation, got %+v", eErr)
	}
}

func chatReq(t *testing.T, body string) (map[string]any, *errclass.Error) {
	return chatReqTS(t, nil, body)
}

func chatReqTS(t *testing.T, ts *pluginapi.ThinkingSupport, body string) (map[string]any, *errclass.Error) {
	t.Helper()
	out, eErr := BuildRequest("minimax", "openai", []byte(body), ts)
	if eErr != nil {
		return nil, eErr
	}
	return decodeReq(t, out), nil
}

func TestChatCompletionsFull(t *testing.T) {
	temp := 0.5
	body := `{
		"messages":[
			{"role":"developer","content":"be terse"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"let me check","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},
				{"id":"call_2","type":"function","function":{"name":"calc","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"r1"},
			{"role":"tool","tool_call_id":"call_2","content":"r2"},
			{"role":"user","content":"thanks"}
		],
		"stop":"END",
		"tools":[{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object"}}}],
		"stream":true,"temperature":0.7,"top_p":0.9
	}`
	m, eErr := chatReq(t, body)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if m["model"] != "minimax" {
		t.Errorf("model = %v", m["model"])
	}
	if m["max_tokens"] != float64(shared.ClaudeMaxTokens(0)) {
		t.Errorf("max_tokens default = %v", m["max_tokens"])
	}
	if m["system"] != "be terse" {
		t.Errorf("system = %v", m["system"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 3 { // user, assistant(tool_use), user(tool_result x2 + text)
		t.Fatalf("messages = %d: %v", len(msgs), msgs)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("role = %v", asst["role"])
	}
	blocks := asst["content"].([]any)
	if len(blocks) != 3 { // text + 2 tool_use
		t.Fatalf("assistant blocks = %v", blocks)
	}
	if blocks[0].(map[string]any)["text"] != "let me check" {
		t.Errorf("text block = %v", blocks[0])
	}
	tu := blocks[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "lookup" {
		t.Errorf("tool_use = %v", tu)
	}
	input := tu["input"].(map[string]any)
	if input["q"] != "x" {
		t.Errorf("tool input = %v", input)
	}
	results := msgs[2].(map[string]any)["content"].([]any)
	if len(results) != 3 {
		t.Fatalf("expected merged tool_result blocks + trailing text, got %v", results)
	}
	tr0 := results[0].(map[string]any)
	if tr0["type"] != "tool_result" || tr0["tool_use_id"] != "call_1" || tr0["content"] != "r1" {
		t.Errorf("tool_result = %v", tr0)
	}
	last := results[2].(map[string]any)
	if last["type"] != "text" || last["text"] != "thanks" {
		t.Errorf("trailing user text not merged into tool_result turn: %v", results)
	}
	if m["stop_sequences"].([]any)[0] != "END" {
		t.Errorf("stop_sequences = %v", m["stop_sequences"])
	}
	tools := m["tools"].([]any)
	tt := tools[0].(map[string]any)
	if tt["name"] != "lookup" || tt["description"] != "d" {
		t.Errorf("tool = %v", tt)
	}
	if tt["input_schema"].(map[string]any)["type"] != "object" {
		t.Errorf("input_schema = %v", tt["input_schema"])
	}
	if m["stream"] != true || m["temperature"] != temp+0.2 || m["top_p"] != 0.9 {
		t.Errorf("sampling/stream passthrough = %v", m)
	}
}

func TestChatCompletionsMaxTokensAndStopVariants(t *testing.T) {
	m, eErr := chatReq(t, `{"messages":[{"role":"user","content":"a"}],"max_completion_tokens":77,"stop":["x","y"],"top_p":0.3}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if m["max_tokens"] != float64(77) {
		t.Errorf("max_completion_tokens fallback = %v", m["max_tokens"])
	}
	stop := m["stop_sequences"].([]any)
	if len(stop) != 2 || stop[0] != "x" {
		t.Errorf("stop_sequences = %v", stop)
	}

	m, eErr = chatReq(t, `{"messages":[{"role":"user","content":"a"}],"max_tokens":5,"max_completion_tokens":77}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if m["max_tokens"] != float64(5) {
		t.Errorf("max_tokens should win = %v", m["max_tokens"])
	}
}

func TestChatCompletionsSystemBlocksAndUserImages(t *testing.T) {
	body := `{
		"messages":[
			{"role":"system","content":"one"},
			{"role":"system","content":"two"},
			{"role":"user","content":[
				{"type":"text","text":"look "},
				{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}
			]}
		]
	}`
	m, eErr := chatReq(t, body)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	sys := m["system"].([]any)
	if len(sys) != 2 || sys[0].(map[string]any)["text"] != "one" {
		t.Errorf("system blocks = %v", sys)
	}
	blocks := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	img := blocks[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["url"] != "https://example.com/a.png" {
		t.Errorf("image block = %v", img)
	}
}

func TestChatCompletionsDataURLImage(t *testing.T) {
	m, eErr := chatReq(t, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	src := m["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AAAA" {
		t.Errorf("base64 source = %v", src)
	}
}

func TestChatCompletionsThinkingBudget(t *testing.T) {
	// Rank-based budget within [Min, Max] for a capability-aware ts.
	ts := &pluginapi.ThinkingSupport{Min: 1024, Max: 32000, Levels: []string{"low", "medium", "high"}}
	for _, effort := range []string{"low", "medium", "high"} {
		m, eErr := chatReqTS(t, ts, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"`+effort+`"}`)
		if eErr != nil {
			t.Fatalf("%s: unexpected error: %v", effort, eErr)
		}
		th := m["thinking"].(map[string]any)
		budget := th["budget_tokens"].(float64)
		if th["type"] != "enabled" || budget < float64(ts.Min) || budget > float64(ts.Max) {
			t.Errorf("%s thinking = %v (want enabled, [%d,%d])", effort, th, ts.Min, ts.Max)
		}
		if _, ok := m["temperature"]; ok {
			t.Errorf("%s temperature must be dropped with thinking", effort)
		}
	}
	// Table semantics: high → exact levelToBudgetMap value 24576, inside
	// [Min,Max] so no clamping applies.
	m, eErr := chatReqTS(t, ts, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"high"}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if m["thinking"].(map[string]any)["budget_tokens"] != float64(24576) {
		t.Errorf("high budget = %v", m["thinking"])
	}

	// xhigh/max accepted when the model declares them.
	declared := &pluginapi.ThinkingSupport{Max: 32000, Levels: []string{"minimal", "low", "medium", "high", "xhigh", "max"}}
	for _, effort := range []string{"xhigh", "max"} {
		m, eErr := chatReqTS(t, declared, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"`+effort+`"}`)
		if eErr != nil {
			t.Fatalf("%s declared: unexpected error: %v", effort, eErr)
		}
		if m["thinking"] == nil {
			t.Errorf("%s declared: thinking missing", effort)
		}
	}

	// nil ts defaults to low/medium/high with exact table values.
	for effort, want := range map[string]int64{"low": 1024, "high": 24576} {
		m, eErr := chatReq(t, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"`+effort+`"}`)
		if eErr != nil {
			t.Fatalf("%s default: unexpected error: %v", effort, eErr)
		}
		if got := m["thinking"].(map[string]any)["budget_tokens"]; got != float64(want) {
			t.Errorf("%s default budget = %v, want %d", effort, got, want)
		}
	}

	// budget clamped above max_tokens: high → 24576, so the 4096 default
	// must be raised to budget+1024.
	m, eErr = chatReqTS(t, ts, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"high"}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if m["max_tokens"].(float64) != float64(24576+1024) {
		t.Errorf("max_tokens not raised above budget: %v", m["max_tokens"])
	}
}

func TestChatCompletionsThinkingNoneAndAuto(t *testing.T) {
	// "none" with ZeroAllowed resolves to 0: no thinking block, sampling kept.
	ts := &pluginapi.ThinkingSupport{ZeroAllowed: true, DynamicAllowed: true}
	m, eErr := chatReqTS(t, ts, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"none","temperature":0.5}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if _, ok := m["thinking"]; ok {
		t.Errorf("thinking must be omitted for zero budget: %v", m["thinking"])
	}
	if m["temperature"] != 0.5 {
		t.Errorf("sampling controls must survive zero budget: %v", m["temperature"])
	}

	// dynamic "auto" has no Anthropic sentinel: omitted, sampling kept.
	m, eErr = chatReqTS(t, ts, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"auto","temperature":0.5}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if _, ok := m["thinking"]; ok {
		t.Errorf("dynamic auto must omit thinking: %v", m["thinking"])
	}
	if m["temperature"] != 0.5 {
		t.Errorf("sampling controls must survive auto: %v", m["temperature"])
	}
}

// F6 regression: with ZeroAllowed AND Min>0, effort "none" must stay at the
// zero off-sentinel — the Min clamp must not silently re-enable thinking
// (FR-005).
func TestChatCompletionsThinkingNoneIgnoresMin(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{ZeroAllowed: true, Min: 2048}
	m, eErr := chatReqTS(t, ts, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"none","temperature":0.5,"top_p":0.9}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if _, ok := m["thinking"]; ok {
		t.Errorf("effort none must not produce a thinking block despite Min: %v", m["thinking"])
	}
	if m["temperature"] != 0.5 || m["top_p"] != 0.9 {
		t.Errorf("sampling controls must survive effort none: temperature=%v top_p=%v", m["temperature"], m["top_p"])
	}
}

func TestChatCompletionsErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"bad stop", `{"messages":[],"stop":123}`},
		{"malformed tool args", `{"messages":[{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":"{oops"}}]}]}`},
		{"system image", `{"messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"https://e.com/x"}}]}]}`},
		{"assistant image", `{"messages":[{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"https://e.com/x"}}]}]}`},
		{"bad data url", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:nocomma"}}]}]}`},
		{"image missing url", `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`},
		{"malformed content", `{"messages":[{"role":"user","content":123}]}`},
		{"tool malformed content", `{"messages":[{"role":"tool","content":123}]}`},
	}
	for _, c := range cases {
		_, eErr := chatReq(t, c.body)
		if eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Errorf("%s: want ClassTranslation, got %+v", c.name, eErr)
		}
	}
	// Unknown roles and part types are unsupported_protocol_or_parameter
	// (FR-009 kernel), not translation failures.
	for _, c := range []struct{ name, body string }{
		{"unknown role", `{"messages":[{"role":"function","content":"x"}]}`},
		{"unknown part type", `{"messages":[{"role":"user","content":[{"type":"audio"}]}]}`},
		{"system bad part", `{"messages":[{"role":"system","content":[{"type":"audio"}]}]}`},
		{"assistant bad part", `{"messages":[{"role":"assistant","content":[{"type":"audio"}]}]}`},
	} {
		if _, eErr := chatReq(t, c.body); eErr == nil || eErr.Class != errclass.ClassUnsupported {
			t.Errorf("%s: want ClassUnsupported, got %+v", c.name, eErr)
		}
	}
	// A non-function tool type is unsupported_protocol_or_parameter (FR-009).
	_, eErr := chatReq(t, `{"tools":[{"type":"web_search"}],"messages":[]}`)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Errorf("unknown tool type: want ClassUnsupported, got %+v", eErr)
	}
}

func TestChatCompletionsEmptyContentDropped(t *testing.T) {
	m, eErr := chatReq(t, `{"messages":[{"role":"assistant"},{"role":"user","content":"hi"}]}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hi" {
		t.Errorf("messages = %v", msgs)
	}
}

func respReq(t *testing.T, body string) (map[string]any, *errclass.Error) {
	t.Helper()
	out, eErr := BuildRequest("qwen", "openai-response", []byte(body), nil)
	if eErr != nil {
		return nil, eErr
	}
	return decodeReq(t, out), nil
}

func TestResponsesStringInputAndInstructions(t *testing.T) {
	m, eErr := respReq(t, `{"instructions":"be nice","input":"hello","max_output_tokens":42,"stream":true}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if m["system"] != "be nice" {
		t.Errorf("system = %v", m["system"])
	}
	if m["max_tokens"] != float64(42) {
		t.Errorf("max_tokens = %v", m["max_tokens"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hello" {
		t.Errorf("messages = %v", msgs)
	}
}

func TestResponsesItems(t *testing.T) {
	body := `{
		"instructions":"sys",
		"input":[
			{"type":"message","role":"system","content":"extra sys"},
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"see "},
				{"type":"input_image","image_url":"https://e.com/i.png"}
			]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"calling"}]},
			{"type":"function_call","call_id":"fc_1","name":"f","arguments":"{\"a\":1}"},
			{"type":"function_call_output","call_id":"fc_1","output":"out"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thought A"},{"type":"summary_text","text":"thought B"}]}
		],
		"tools":[{"type":"function","name":"f","description":"fd","parameters":{"type":"object"}}],
		"reasoning":{"effort":"low"}
	}`
	m, eErr := respReq(t, body)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	sysBlocks := m["system"].([]any)
	if len(sysBlocks) != 2 ||
		sysBlocks[0].(map[string]any)["text"] != "sys" ||
		sysBlocks[1].(map[string]any)["text"] != "extra sys" {
		t.Errorf("system = %v", m["system"])
	}
	msgs := m["messages"].([]any)
	// user(img), assistant(text+tool_use merged? no: text then tool_use in same assistant bucket), user tool_result, assistant thinking
	if len(msgs) != 4 {
		t.Fatalf("messages = %d: %v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]any)
	blocks := first["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "see " {
		t.Errorf("user text = %v", blocks[0])
	}
	if blocks[1].(map[string]any)["source"].(map[string]any)["url"] != "https://e.com/i.png" {
		t.Errorf("image = %v", blocks[1])
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if asst[0].(map[string]any)["text"] != "calling" {
		t.Errorf("assistant text = %v", asst[0])
	}
	fc := asst[1].(map[string]any)
	if fc["type"] != "tool_use" || fc["id"] != "fc_1" || fc["name"] != "f" ||
		fc["input"].(map[string]any)["a"] != float64(1) {
		t.Errorf("function_call block = %v", fc)
	}
	out := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if out["type"] != "tool_result" || out["tool_use_id"] != "fc_1" || out["content"] != "out" {
		t.Errorf("function_call_output block = %v", out)
	}
	th := msgs[3].(map[string]any)["content"].([]any)[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "thought A\nthought B" {
		t.Errorf("reasoning block = %v", th)
	}
	thb := m["thinking"].(map[string]any)
	if thb["budget_tokens"] != float64(1024) { // nil ts: low → lowest anchor
		t.Errorf("thinking budget = %v", thb)
	}
	tool := m["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "f" || tool["description"] != "fd" {
		t.Errorf("tool = %v", tool)
	}
}

func TestResponsesReasoningOmittedWhenEmptySummary(t *testing.T) {
	m, eErr := respReq(t, `{"input":[{"type":"reasoning","summary":[]}]}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	if len(m["messages"].([]any)) != 0 {
		t.Errorf("messages = %v", m["messages"])
	}
}

func TestChatCompletionsEmptyArgsToolImagesDefaultSchema(t *testing.T) {
	body := `{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":""}}]},
			{"role":"tool","tool_call_id":"c","content":[
				{"type":"text","text":"t"},
				{"type":"image_url","image_url":{"url":"https://e.com/x.png"}}
			]}
		],
		"tools":[{"type":"function","function":{"name":"g"}}]
	}`
	m, eErr := chatReq(t, body)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	tu := m["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if len(tu["input"].(map[string]any)) != 0 {
		t.Errorf("empty args input = %v", tu["input"])
	}
	blocks := m["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("tool_result content = %v", blocks)
	}
	if blocks[0].(map[string]any)["text"] != "t" {
		t.Errorf("text block = %v", blocks[0])
	}
	if blocks[1].(map[string]any)["source"].(map[string]any)["url"] != "https://e.com/x.png" {
		t.Errorf("image block = %v", blocks[1])
	}
	schema := m["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)
	if fmt.Sprint(schema) != "map[properties:map[] type:object]" {
		t.Errorf("default input_schema = %v", schema)
	}
}

func TestChatCompletionsUnknownEffort(t *testing.T) {
	_, eErr := chatReq(t, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"maximum"}`)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("want ClassUnsupported, got %+v", eErr)
	}
	// undeclared canonical levels are rejected explicitly too (FR-005).
	_, eErr = chatReq(t, `{"messages":[{"role":"user","content":"a"}],"reasoning_effort":"max"}`)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("undeclared max: want ClassUnsupported, got %+v", eErr)
	}
}

func TestResponsesErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"instructions not string", `{"instructions":123}`},
		{"input neither string nor array", `{"input":123}`},
		{"assistant image", `{"input":[{"type":"message","role":"assistant","content":[{"type":"image_url","image_url":{"url":"u"}}]}]}`},
		{"system image", `{"input":[{"type":"message","role":"system","content":[{"type":"image_url","image_url":{"url":"u"}}]}]}`},
		{"bad function args", `{"input":[{"type":"function_call","call_id":"c","name":"f","arguments":"nope"}]}`},
	}
	for _, c := range cases {
		_, eErr := respReq(t, c.body)
		if eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Errorf("%s: want ClassTranslation, got %+v", c.name, eErr)
		}
	}
	// Unknown roles, part types, and input item types are
	// unsupported_protocol_or_parameter (FR-009 kernel), not translation failures.
	for _, c := range []struct{ name, body string }{
		{"unknown message role", `{"input":[{"type":"message","role":"tool","content":"x"}]}`},
		{"user item bad part", `{"input":[{"type":"message","role":"user","content":[{"type":"audio"}]}]}`},
		{"unknown item type", `{"input":[{"type":"web_search_call"}]}`},
	} {
		if _, eErr := respReq(t, c.body); eErr == nil || eErr.Class != errclass.ClassUnsupported {
			t.Errorf("%s: want ClassUnsupported, got %+v", c.name, eErr)
		}
	}
	// A non-function tool type is unsupported_protocol_or_parameter (FR-009).
	_, eErr := respReq(t, `{"tools":[{"type":"web_search"}],"input":[]}`)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Errorf("non-function tool: want ClassUnsupported, got %+v", eErr)
	}
}

func TestResponsesDefaultInputSchema(t *testing.T) {
	m, eErr := respReq(t, `{"input":[],"tools":[{"type":"function","name":"f"}]}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	schema := m["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)
	if fmt.Sprint(schema) != "map[properties:map[] type:object]" {
		t.Errorf("default input_schema = %v", schema)
	}
}

func TestDecodeStopNullAndAbsent(t *testing.T) {
	if got, eErr := decodeStop(nil); eErr != nil || got != nil {
		t.Errorf("nil stop = %v, %v", got, eErr)
	}
	if got, eErr := decodeStop(json.RawMessage("null")); eErr != nil || got != nil {
		t.Errorf("null stop = %v, %v", got, eErr)
	}
}

func TestMsgBuilderFlushEmpty(t *testing.T) {
	var b msgBuilder
	b.flush() // no-op on empty builder
	if b.msgs != nil {
		t.Errorf("unexpected messages %v", b.msgs)
	}
}

func TestChatCompletionsConsecutiveUserMerge(t *testing.T) {
	m, eErr := chatReq(t, `{"messages":[
		{"role":"user","content":"one"},
		{"role":"user","content":"two"}
	]}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("consecutive plain users must merge, got %d: %v", len(msgs), msgs)
	}
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 || content[0].(map[string]any)["text"] != "one" || content[1].(map[string]any)["text"] != "two" {
		t.Errorf("merged user blocks = %v", content)
	}
}

func TestResponsesToolResultThenUserMerges(t *testing.T) {
	m, eErr := respReq(t, `{"input":[
		{"type":"message","role":"user","content":"go"},
		{"type":"function_call","call_id":"fc_1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"fc_1","output":"out"},
		{"type":"message","role":"user","content":"thanks"}
	]}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 3 { // user, assistant(tool_use), user(tool_result + text)
		t.Fatalf("messages = %d: %v", len(msgs), msgs)
	}
	blocks := msgs[2].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("expected merged tool_result+text turn, got %v", blocks)
	}
	tr := blocks[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "fc_1" || tr["content"] != "out" {
		t.Errorf("tool_result = %v", tr)
	}
	if blocks[1].(map[string]any)["text"] != "thanks" {
		t.Errorf("text block = %v", blocks[1])
	}
}

func TestSystemFieldEmpty(t *testing.T) {
	if systemField(nil) != nil {
		t.Error("empty system must be omitted")
	}
}

func TestImageBlockPlainURL(t *testing.T) {
	block, eErr := shared.ImageSource("https://x/y.jpg")
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	want := anthropicBlock{"type": "image", "source": anthropicBlock{"type": "url", "url": "https://x/y.jpg"}}
	if !reflect.DeepEqual(block, want) {
		t.Errorf("block = %v", block)
	}
}

func TestContentPartsNull(t *testing.T) {
	blocks, eErr := contentParts(json.RawMessage("null"))
	if eErr != nil || blocks != nil {
		t.Errorf("null content = %v %v", blocks, eErr)
	}
	blocks, eErr = contentParts(nil)
	if eErr != nil || blocks != nil {
		t.Errorf("absent content = %v %v", blocks, eErr)
	}
}

// W5 pin: per-part sequence survives translation — [text a, image, text b]
// emits text a, image, text b in order instead of concatenating the texts
// and appending the image after (FR-005 multimodal preservation).
func TestContentPartsInterleavingPreserved(t *testing.T) {
	m, eErr := chatReq(t, `{"messages":[{"role":"user","content":[
		{"type":"text","text":"a"},
		{"type":"image_url","image_url":{"url":"https://e.com/i.png"}},
		{"type":"input_text","text":"b"}
	]}]}`)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d: %v", len(msgs), msgs)
	}
	blocks := msgs[0].(map[string]any)["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("content blocks = %v", blocks)
	}
	if got := blocks[0].(map[string]any)["text"]; got != "a" {
		t.Errorf("block 0 = %v", blocks[0])
	}
	img := blocks[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["url"] != "https://e.com/i.png" {
		t.Errorf("block 1 = %v", img)
	}
	if got := blocks[2].(map[string]any)["text"]; got != "b" {
		t.Errorf("block 2 must stay after the image: %v", blocks[2])
	}

	// Mixed tool_result parts keep their sequence too.
	m, eErr = chatReq(t, `{"messages":[
		{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c","content":[
			{"type":"text","text":"before"},
			{"type":"image_url","image_url":{"url":"https://e.com/j.png"}},
			{"type":"text","text":"after"}
		]}
	]}`)
	if eErr != nil {
		t.Fatalf("tool leg: unexpected error: %v", eErr)
	}
	tr := m["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	parts := tr["content"].([]any)
	if len(parts) != 3 ||
		parts[0].(map[string]any)["text"] != "before" ||
		parts[1].(map[string]any)["type"] != "image" ||
		parts[2].(map[string]any)["text"] != "after" {
		t.Errorf("tool_result part order lost: %v", parts)
	}
}

func TestDataURLDefaultMediaType(t *testing.T) {
	block, eErr := shared.ImageSource("data:,hello")
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	src := block["source"].(map[string]any)
	if src["type"] != "url" || src["url"] != "data:,hello" {
		t.Errorf("src = %v", src)
	}
}

// F-drift pin: tool_choice is preserved on every cross-format leg.
func TestChatCompletionsToolChoicePreserved(t *testing.T) {
	m, eErr := chatReq(t, `{"tool_choice":"auto","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if eErr != nil {
		t.Fatalf("auto: %v", eErr)
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "auto" {
		t.Fatalf("auto tool_choice = %v", tc)
	}

	m, eErr = chatReq(t, `{"tool_choice":{"type":"function","function":{"name":"lookup"}},"messages":[],"tools":[{"type":"function","function":{"name":"lookup"}}]}`)
	if eErr != nil {
		t.Fatalf("named: %v", eErr)
	}
	tc = m["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "lookup" {
		t.Fatalf("named tool_choice = %v", tc)
	}
}

func TestResponsesToolChoicePreserved(t *testing.T) {
	m, eErr := respReq(t, `{"tool_choice":"auto","input":[]}`)
	if eErr != nil {
		t.Fatalf("auto: %v", eErr)
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "auto" {
		t.Fatalf("auto tool_choice = %v", tc)
	}
}

func TestResponsesNoneToolChoiceRejected(t *testing.T) {
	_, eErr := respReq(t, `{"tool_choice":"none","input":[]}`)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
		!strings.Contains(eErr.Message, `"none"`) {
		t.Fatalf("err = %v", eErr)
	}
}

// Remaining tool_choice arms on the CC→Messages leg: "required" maps to
// Anthropic "any"; "none" fails descriptively; malformed fails translation.
func TestChatCompletionsToolChoiceArms(t *testing.T) {
	m, eErr := chatReq(t, `{"tool_choice":"required","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if eErr != nil {
		t.Fatalf("required: %v", eErr)
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "any" {
		t.Fatalf("required tool_choice = %v", tc)
	}

	_, eErr = chatReq(t, `{"tool_choice":"none","messages":[]}`)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
		!strings.Contains(eErr.Message, `"none"`) {
		t.Fatalf("none err = %v", eErr)
	}

	_, eErr = chatReq(t, `{"tool_choice":42,"messages":[]}`)
	if eErr == nil || !strings.Contains(eErr.Message, "malformed tool_choice") {
		t.Fatalf("malformed err = %v", eErr)
	}
}

func TestResponsesMalformedToolChoice(t *testing.T) {
	_, eErr := respReq(t, `{"tool_choice":42,"input":[]}`)
	if eErr == nil || !strings.Contains(eErr.Message, "malformed tool_choice") {
		t.Fatalf("err = %v", eErr)
	}
}

// FR-005 challenge-adjudicated table: an explicit parallel_tool_calls
// false must survive toward Messages upstreams as
// disable_parallel_tool_use on the tool_choice object — synthesized as
// {"type":"auto"} when no tool_choice was sent. Rows 1 and 6 (absent or
// true parallel flag) stay byte-identical to the pre-fix output.

func TestParallelFalseSynthesizesAutoToolChoice(t *testing.T) { // row 2
	m, eErr := chatReq(t, `{"messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"parallel_tool_calls":false}`)
	if eErr != nil {
		t.Fatalf("chat: %v", eErr)
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "auto" || tc["disable_parallel_tool_use"] != true || len(tc) != 2 {
		t.Fatalf("chat tool_choice = %v", tc)
	}

	m, eErr = respReq(t, `{"input":[],"parallel_tool_calls":false}`)
	if eErr != nil {
		t.Fatalf("responses: %v", eErr)
	}
	tc = m["tool_choice"].(map[string]any)
	if tc["type"] != "auto" || tc["disable_parallel_tool_use"] != true || len(tc) != 2 {
		t.Fatalf("responses tool_choice = %v", tc)
	}
}

func TestParallelFalseAttachesToDecodedToolChoice(t *testing.T) { // rows 3/4/5
	cases := []struct {
		choice string
		want   map[string]any
	}{
		{`"auto"`, map[string]any{"type": "auto", "disable_parallel_tool_use": true}},
		{`"required"`, map[string]any{"type": "any", "disable_parallel_tool_use": true}},
		{`{"type":"function","function":{"name":"lookup"}}`, map[string]any{"type": "tool", "name": "lookup", "disable_parallel_tool_use": true}},
	}
	for _, c := range cases {
		body := fmt.Sprintf(`{"tool_choice":%s,"messages":[],"tools":[{"type":"function","function":{"name":"lookup"}}],"parallel_tool_calls":false}`, c.choice)
		m, eErr := chatReq(t, body)
		if eErr != nil {
			t.Fatalf("%s: %v", c.choice, eErr)
		}
		if got := m["tool_choice"]; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s tool_choice = %v, want %v", c.choice, got, c.want)
		}
	}
}

func TestNoParallelFlagWhenAbsentOrTrue(t *testing.T) { // rows 1 + 6 byte pins
	ccCases := []struct {
		body             string
		forbidToolChoice bool // row 1: nothing decoded at all
	}{
		{`{"messages":[]}`, true},
		{`{"messages":[],"parallel_tool_calls":true}`, true},
		{`{"messages":[],"tool_choice":"auto","parallel_tool_calls":true}`, false},
		{`{"messages":[],"tool_choice":"required","parallel_tool_calls":true}`, false},
		{`{"messages":[],"tool_choice":{"type":"function","function":{"name":"f"}},"tools":[{"type":"function","function":{"name":"f"}}]}`, false},
	}
	for _, c := range ccCases {
		out, eErr := BuildRequest("minimax", "openai", []byte(c.body), nil)
		if eErr != nil {
			t.Fatalf("%s: %v", c.body, eErr)
		}
		s := string(out)
		if strings.Contains(s, "parallel") ||
			(c.forbidToolChoice && strings.Contains(s, "tool_choice")) {
			t.Errorf("%s leaked: %s", c.body, s)
		}
	}

	respCases := []struct {
		body             string
		forbidToolChoice bool
	}{
		{`{"input":[]}`, true},
		{`{"input":[],"parallel_tool_calls":true}`, true},
		{`{"input":[],"tool_choice":"auto","parallel_tool_calls":true}`, false},
	}
	for _, c := range respCases {
		out, eErr := BuildRequest("qwen", "openai-response", []byte(c.body), nil)
		if eErr != nil {
			t.Fatalf("responses %s: %v", c.body, eErr)
		}
		s := string(out)
		if strings.Contains(s, "parallel") ||
			(c.forbidToolChoice && strings.Contains(s, "tool_choice")) {
			t.Errorf("responses %s leaked: %s", c.body, s)
		}
	}
}

func TestParallelFalseNoneStillUnsupported(t *testing.T) { // row 7
	_, eErr := chatReq(t, `{"tool_choice":"none","messages":[],"parallel_tool_calls":false}`)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
		!strings.Contains(eErr.Message, `"none"`) {
		t.Fatalf("err = %v", eErr)
	}
}
