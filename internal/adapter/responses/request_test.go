package responses

import (
	"encoding/json"
	"fmt"
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

func wantErr(t *testing.T, eErr *errclass.Error, class errclass.Class) {
	t.Helper()
	if eErr == nil || eErr.Class != class {
		t.Fatalf("want %s, got %+v", class, eErr)
	}
}

func inputItems(t *testing.T, m map[string]any) []any {
	t.Helper()
	items, ok := m["input"].([]any)
	if !ok {
		t.Fatalf("missing input array: %v", m)
	}
	return items
}

func itemMap(t *testing.T, items []any, i int) map[string]any {
	t.Helper()
	m, ok := items[i].(map[string]any)
	if !ok {
		t.Fatalf("item %d not an object: %v", i, items[i])
	}
	return m
}

func partMap(t *testing.T, item map[string]any, i int) map[string]any {
	t.Helper()
	parts, ok := item["content"].([]any)
	if !ok {
		t.Fatalf("item content missing: %v", item)
	}
	p, ok := parts[i].(map[string]any)
	if !ok {
		t.Fatalf("part %d not an object: %v", i, parts[i])
	}
	return p
}

func TestEndpointPath(t *testing.T) {
	if EndpointPath != "/v1/responses" {
		t.Errorf("EndpointPath = %q", EndpointPath)
	}
}

func TestBuildRequestUnsupportedFormat(t *testing.T) {
	_, eErr := BuildRequest("m", "grpc", []byte(`{}`), nil)
	wantErr(t, eErr, errclass.ClassUnsupported)
}

// ---- openai-response passthrough ----------------------------------------

func TestPassthroughRewritesModelOnly(t *testing.T) {
	body := []byte(`{"model":"opencode-go/gpt-5.6-luna","input":"hi","stream":true,"reasoning":{"effort":"low"},"extra":{"nested":[1,2]}}`)
	m := decodeReq(t, mustBuild(t, "gpt-5.6-luna", "openai-response", body, nil))
	if m["model"] != "gpt-5.6-luna" {
		t.Errorf("model = %v", m["model"])
	}
	if m["stream"] != true || m["extra"].(map[string]any)["nested"].([]any)[1] != float64(2) {
		t.Errorf("passthrough fields lost: %v", m)
	}
	if r := m["reasoning"].(map[string]any); r["effort"] != "low" {
		t.Errorf("reasoning = %v", r)
	}
}

func TestPassthroughMalformed(t *testing.T) {
	_, eErr := BuildRequest("m", "openai-response", []byte(`{`), nil)
	wantErr(t, eErr, errclass.ClassTranslation)
}

func mustBuild(t *testing.T, model, format string, body []byte, ts *pluginapi.ThinkingSupport) []byte {
	t.Helper()
	out, eErr := BuildRequest(model, format, body, ts)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	return out
}

// ---- Chat Completions source --------------------------------------------

func TestFromChatCompletionsFull(t *testing.T) {
	body := `{
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"developer","content":"also safe"},
			{"role":"user","content":"hi"},
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://x/img.png"}}]},
			{"role":"assistant","content":"checking","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"result"}
		],
		"max_tokens":256,
		"tools":[{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"stream":true,
		"temperature":0.5,
		"top_p":0.9,
		"reasoning_effort":"high",
		"stop":["END"]
	}`
	m := decodeReq(t, mustBuild(t, "gpt-5.6-luna", "openai", []byte(body), nil))
	if m["model"] != "gpt-5.6-luna" {
		t.Errorf("model = %v", m["model"])
	}
	if m["instructions"] != "be terse\n\nalso safe" {
		t.Errorf("instructions = %v", m["instructions"])
	}
	if m["max_output_tokens"] != float64(256) || m["stream"] != true ||
		m["temperature"] != 0.5 || m["top_p"] != 0.9 ||
		m["parallel_tool_calls"] != true || m["tool_choice"] != "auto" {
		t.Errorf("envelope fields = %v", m)
	}
	if r := m["reasoning"].(map[string]any); r["effort"] != "high" {
		t.Errorf("reasoning = %v", r)
	}
	if _, has := m["stop"]; has {
		t.Error("stop must be omitted (FR-005 policy)")
	}
	items := inputItems(t, m)
	if len(items) != 5 {
		t.Fatalf("items = %d: %v", len(items), items)
	}
	u := itemMap(t, items, 0)
	if u["role"] != "user" || u["type"] != "message" {
		t.Errorf("item0 = %v", u)
	}
	if p := partMap(t, u, 0); p["type"] != "input_text" || p["text"] != "hi" {
		t.Errorf("item0 part = %v", p)
	}
	u2 := itemMap(t, items, 1)
	if p := partMap(t, u2, 0); p["type"] != "input_text" || p["text"] != "look" {
		t.Errorf("item1 text part = %v", p)
	}
	if p := partMap(t, u2, 1); p["type"] != "input_image" || p["image_url"] != "https://x/img.png" {
		t.Errorf("item1 image part = %v", p)
	}
	a := itemMap(t, items, 2)
	if a["role"] != "assistant" {
		t.Fatalf("item2 = %v", a)
	}
	if p := partMap(t, a, 0); p["type"] != "output_text" || p["text"] != "checking" {
		t.Errorf("assistant part = %v (want output_text)", p)
	}
	fc := itemMap(t, items, 3)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" ||
		fc["name"] != "lookup" || fc["arguments"] != `{"q":"x"}` {
		t.Errorf("function_call = %v", fc)
	}
	fco := itemMap(t, items, 4)
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" || fco["output"] != "result" {
		t.Errorf("function_call_output = %v", fco)
	}
	tools := m["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["type"] != "function" || tl["name"] != "lookup" || tl["description"] != "d" {
		t.Errorf("tool = %v", tl)
	}
	if tl["parameters"].(map[string]any)["type"] != "object" {
		t.Errorf("parameters = %v", tl["parameters"])
	}
}

func TestFromChatCompletionsTokenFallbacks(t *testing.T) {
	msg := `{"messages":[{"role":"user","content":"hi"}]`
	out := mustBuild(t, "m", "openai", []byte(msg+`,"max_tokens":10}`), nil)
	if m := decodeReq(t, out); m["max_output_tokens"] != float64(10) {
		t.Errorf("max_tokens lost: %v", m)
	}
	out = mustBuild(t, "m", "openai", []byte(msg+`,"max_tokens":0,"max_completion_tokens":20}`), nil)
	if m := decodeReq(t, out); m["max_output_tokens"] != float64(20) {
		t.Errorf("max_completion_tokens fallback lost: %v", m)
	}
	out = mustBuild(t, "m", "openai", []byte(msg+`}`), nil)
	m := decodeReq(t, out)
	if m["max_output_tokens"] != nil {
		t.Errorf("unexpected max_output_tokens: %v", m)
	}
	body := `{"messages":[{"role":"user","content":"hi"},{"role":"user","content":null},{"role":"assistant","tool_calls":[]}]}`
	if items := inputItems(t, decodeReq(t, mustBuild(t, "m", "openai", []byte(body), nil))); len(items) != 1 {
		t.Errorf("null/empty content must be skipped: %v", items)
	}
}

func TestFromChatCompletionsImageStringForm(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://y/p.jpg"}]}]}`
	m := decodeReq(t, mustBuild(t, "m", "openai", []byte(body), nil))
	items := inputItems(t, m)
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	if p := partMap(t, itemMap(t, items, 0), 0); p["type"] != "input_image" || p["image_url"] != "https://y/p.jpg" {
		t.Errorf("string-form image part = %v", p)
	}
}

// System/developer messages cannot carry images: rejected descriptively
// like the tool-result siblings instead of dropped by shared.JoinTexts (FR-005).
func TestFromChatCompletionsImageInSystemRejected(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		_, eErr := BuildRequest("m", "openai", []byte(fmt.Sprintf(
			`{"messages":[{"role":%q,"content":[{"type":"image_url","image_url":{"url":"https://x/i.png"}}]}]}`, role)), nil)
		if eErr == nil || eErr.Class != errclass.ClassTranslation ||
			!strings.Contains(eErr.Message, "cannot carry image content") {
			t.Fatalf("%s: not rejected descriptively: %+v", role, eErr)
		}
	}
}

// Schemaless tools normalize to the shared empty object schema on every
// route (parity with the Messages/Chat Completions targets): absent and
// null parameter schemas ship {"type":"object","properties":{}} upstream.

func TestFromChatCompletionsToolWithoutParameters(t *testing.T) {
	body := `{"messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`
	m := decodeReq(t, mustBuild(t, "m", "openai", []byte(body), nil))
	tl := m["tools"].([]any)[0].(map[string]any)
	want, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{}})
	if got, _ := json.Marshal(tl["parameters"]); string(got) != string(want) {
		t.Errorf("absent parameters = %s", got)
	}
}

func TestFromClaudeMessagesNullInputSchema(t *testing.T) {
	body := `{"max_tokens":16,"messages":[],"tools":[{"name":"f","input_schema":null}]}`
	m := decodeReq(t, mustBuild(t, "m", "claude", []byte(body), nil))
	tl := m["tools"].([]any)[0].(map[string]any)
	want, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{}})
	if got, _ := json.Marshal(tl["parameters"]); string(got) != string(want) {
		t.Errorf("null input_schema = %s", got)
	}
}

func TestFromChatCompletionsAcceptsResponsesPartAliases(t *testing.T) {
	// Same body must succeed on every route: Responses-flavored part
	// aliases translate exactly like their Chat Completions names.
	body := `{"messages":[{"role":"user","content":[
		{"type":"input_text","text":"see "},
		{"type":"output_text","text":"this"},
		{"type":"input_image","image_url":{"url":"https://x/i.png"}}
	]}]}`
	m := decodeReq(t, mustBuild(t, "m", "openai", []byte(body), nil))
	parts := inputItems(t, m)[0].(map[string]any)["content"].([]any)
	if p := parts[0].(map[string]any); p["type"] != "input_text" || p["text"] != "see " {
		t.Errorf("part0 = %v", p)
	}
	if p := parts[1].(map[string]any); p["type"] != "input_text" || p["text"] != "this" {
		t.Errorf("part1 = %v", p)
	}
	if p := parts[2].(map[string]any); p["type"] != "input_image" || p["image_url"] != "https://x/i.png" {
		t.Errorf("part2 = %v", p)
	}
}

func TestFromChatCompletionsErrors(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		class errclass.Class
	}{
		{"malformed json", `{"messages":`, errclass.ClassTranslation},
		{"malformed content", `{"messages":[{"role":"user","content":12}]}`, errclass.ClassTranslation},
		{"malformed system content", `{"messages":[{"role":"system","content":12}]}`, errclass.ClassTranslation},
		{"malformed assistant content", `{"messages":[{"role":"assistant","content":12}]}`, errclass.ClassTranslation},
		{"malformed tool content", `{"messages":[{"role":"tool","tool_call_id":"t","content":12}]}`, errclass.ClassTranslation},
		{"unsupported part type", `{"messages":[{"role":"user","content":[{"type":"audio"}]}]}`, errclass.ClassUnsupported},
		{"image missing url", `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, errclass.ClassTranslation},
		{"unsupported role", `{"messages":[{"role":"function","content":"x"}]}`, errclass.ClassUnsupported},
		{"unsupported tool type", `{"messages":[],"tools":[{"type":"code_interpreter"}]}`, errclass.ClassUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "openai", []byte(tc.body), nil)
			wantErr(t, eErr, tc.class)
		})
	}
}

// ---- Claude Messages source ---------------------------------------------

func TestFromClaudeMessagesFull(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{
		Min:    1024,
		Max:    64000,
		Levels: []string{"minimal", "low", "medium", "high", "xhigh"},
	}
	body := `{
		"model":"opencode-go/gpt-5.6-luna","max_tokens":512,"stream":true,"temperature":0.3,"top_p":0.8,
		"system":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}],
		"thinking":{"type":"enabled","budget_tokens":60000},
		"tool_choice":{"type":"tool","name":"lookup"},
		"tools":[{"name":"lookup","description":"d","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[
				{"type":"text","text":"working"},
				{"type":"thinking","thinking":"hmm","signature":"sig"},
				{"type":"redacted_thinking","data":"xx"},
				{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}},
				{"type":"tool_use","id":"tu_2","name":"lookup","input":null},
				{"type":"tool_use","id":"tu_3","name":"lookup"}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"ok"},
				{"type":"tool_result","tool_use_id":"tu_2","content":[{"type":"text","text":"part"}]},
				{"type":"tool_result","tool_use_id":"tu_3","content":"boom","is_error":true},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
			]},
			{"role":"user","content":[{"type":"text","text":"done"}]}
		]
	}`
	m := decodeReq(t, mustBuild(t, "gpt-5.6-luna", "claude", []byte(body), ts))
	if m["model"] != "gpt-5.6-luna" {
		t.Errorf("model = %v", m["model"])
	}
	if m["instructions"] != "s1\n\ns2" {
		t.Errorf("instructions = %v", m["instructions"])
	}
	if m["max_output_tokens"] != float64(512) || m["stream"] != true ||
		m["temperature"] != 0.3 || m["top_p"] != 0.8 {
		t.Errorf("envelope fields = %v", m)
	}
	if r := m["reasoning"].(map[string]any); r["effort"] != "xhigh" {
		t.Errorf("big budget + xhigh capability: reasoning = %v, want xhigh", r)
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["name"] != "lookup" {
		t.Errorf("tool_choice = %v", tc)
	}
	tl := m["tools"].([]any)[0].(map[string]any)
	if tl["type"] != "function" || tl["name"] != "lookup" {
		t.Errorf("tool = %v", tl)
	}
	items := inputItems(t, m)
	if len(items) != 10 {
		t.Fatalf("items = %d: %v", len(items), items)
	}
	if p := partMap(t, itemMap(t, items, 0), 0); p["type"] != "input_text" || p["text"] != "hello" {
		t.Errorf("item0 = %v", p)
	}
	a := itemMap(t, items, 1)
	if a["role"] != "assistant" {
		t.Fatalf("item1 = %v", a)
	}
	if p := partMap(t, a, 0); p["type"] != "output_text" || p["text"] != "working" {
		t.Errorf("assistant part = %v", p)
	}
	for i, want := range []struct{ id, args string }{
		{"tu_1", `{"q":"x"}`}, {"tu_2", "{}"}, {"tu_3", "{}"},
	} {
		fc := itemMap(t, items, 2+i)
		if fc["type"] != "function_call" || fc["call_id"] != want.id || fc["arguments"] != want.args {
			t.Errorf("function_call %d = %v, want %+v", i, fc, want)
		}
	}
	fco := itemMap(t, items, 5)
	if fco["output"] != "ok" || fco["call_id"] != "tu_1" {
		t.Errorf("tool output 0 = %v", fco)
	}
	if got := itemMap(t, items, 6)["output"]; got != "part" {
		t.Errorf("tool output 1 = %v, want text only", got)
	}
	if got := itemMap(t, items, 7)["output"]; got != "[error] boom" {
		t.Errorf("is_error marker = %v", got)
	}
	img := itemMap(t, items, 8)
	if img["role"] != "user" {
		t.Fatalf("item8 = %v", img)
	}
	if p := partMap(t, img, 0); p["type"] != "input_image" || p["image_url"] != "data:image/png;base64,AAAA" {
		t.Errorf("base64 image part = %v", p)
	}
	if p := partMap(t, itemMap(t, items, 9), 0); p["text"] != "done" {
		t.Errorf("item9 = %v", p)
	}
}

func TestResponsesEffortMapping(t *testing.T) {
	build := func(ts *pluginapi.ThinkingSupport, thinking string) map[string]any {
		body := fmt.Sprintf(`{"max_tokens":64,"thinking":%s,"messages":[{"role":"user","content":"hi"}]}`, thinking)
		return decodeReq(t, mustBuild(t, "m", "claude", []byte(body), ts))
	}
	effort := func(m map[string]any) any {
		r, ok := m["reasoning"].(map[string]any)
		if !ok {
			return nil
		}
		return r["effort"]
	}
	// nil capability → default ladder low/medium/high.
	if got := effort(build(nil, `{"type":"enabled","budget_tokens":8192}`)); got != "medium" {
		t.Errorf("nil ts ladder = %v, want medium", got)
	}
	if got := effort(build(nil, `{"type":"enabled","budget_tokens":70000}`)); got != "high" {
		t.Errorf("nil ts ladder top = %v, want high", got)
	}
	// ZeroAllowed with "none" declared resolves to "none", which now
	// forwards verbatim like the CC-upstream leg (no silent upstream
	// default re-enabling reasoning).
	zeroTS := &pluginapi.ThinkingSupport{ZeroAllowed: true, Levels: []string{"none", "minimal", "low"}}
	if got := effort(build(zeroTS, `{"type":"enabled","budget_tokens":0}`)); got != "none" {
		t.Errorf("declared none budget = %v, want forwarded none", got)
	}
	// DynamicAllowed yields the "auto" sentinel; also omitted here.
	dynTS := &pluginapi.ThinkingSupport{DynamicAllowed: true, Levels: []string{"low", "medium", "high"}}
	if got := effort(build(dynTS, `{"type":"enabled","budget_tokens":0}`)); got != nil {
		t.Errorf("auto sentinel must omit reasoning entirely, got %v", got)
	}
	// Disabled thinking produces no reasoning block.
	m := build(nil, `{"type":"disabled","budget_tokens":2048}`)
	if got := effort(m); got != nil {
		t.Errorf("disabled thinking produced reasoning %v", got)
	}
}

func TestClaudeImageDefaultMediaType(t *testing.T) {
	body := `{"max_tokens":16,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","data":"QQ=="}}]}]}`
	m := decodeReq(t, mustBuild(t, "m", "claude", []byte(body), nil))
	p := partMap(t, itemMap(t, inputItems(t, m), 0), 0)
	if p["image_url"] != "data:application/octet-stream;base64,QQ==" {
		t.Errorf("default media type = %v", p)
	}
}

func TestClaudeToolChoiceMappings(t *testing.T) {
	build := func(choice string) map[string]any {
		body := fmt.Sprintf(`{"max_tokens":16,%s"messages":[{"role":"user","content":"hi"}]}`, choice)
		return decodeReq(t, mustBuild(t, "m", "claude", []byte(body), nil))
	}
	if got := build(`"tool_choice":{"type":"auto"},`)["tool_choice"]; got != "auto" {
		t.Errorf("auto = %v", got)
	}
	if got := build(`"tool_choice":{"type":"any"},`)["tool_choice"]; got != "required" {
		t.Errorf("any = %v", got)
	}
	if got := build(`"tool_choice":{"type":"none"},`)["tool_choice"]; got != "none" {
		t.Errorf("none = %v", got)
	}
	if _, has := build(``)["tool_choice"]; has {
		t.Error("absent tool_choice must be omitted")
	}
}

// The claude leg decodes tool_choice through the shared classifier, so its
// edge cases match the Chat Completions route: bare string forms decode,
// and a named tool without a name is a translation error rather than an
// empty-name function choice.
func TestClaudeToolChoiceSharedSemantics(t *testing.T) {
	m := decodeReq(t, mustBuild(t, "m", "claude",
		[]byte(`{"max_tokens":10,"tool_choice":"auto","messages":[{"role":"user","content":"hi"}]}`), nil))
	if m["tool_choice"] != "auto" {
		t.Fatalf("bare string auto = %v", m["tool_choice"])
	}
	_, eErr := BuildRequest("m", "claude", []byte(
		`{"max_tokens":10,"tool_choice":{"type":"tool"},"messages":[]}`), nil)
	wantErr(t, eErr, errclass.ClassTranslation)
}

func TestFromClaudeMessagesAbsentSystemAndNullContent(t *testing.T) {
	m := decodeReq(t, mustBuild(t, "m", "claude",
		[]byte(`{"max_tokens":10,"messages":[{"role":"user","content":null}]}`), nil))
	if _, has := m["instructions"]; has {
		t.Errorf("absent system must omit instructions: %v", m)
	}
	if _, has := m["input"]; has {
		t.Errorf("null content produced input: %v", m)
	}
}

// Unknown message roles are rejected descriptively via the shared kernel,
// naming the endpoint, exactly like every other translator leg — never
// forwarded verbatim upstream.
func TestClaudeUnknownRoleRejected(t *testing.T) {
	body := []byte(`{"max_tokens":10,"messages":[` +
		`{"role":"user","content":"hi"},{"role":"system","content":"mid-history"}]}`)
	_, eErr := BuildRequest("m", "claude", body, nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
		eErr.Message != `unsupported message role "system" for /v1/responses` {
		t.Fatalf("mid-history system = %+v", eErr)
	}

	_, eErr = BuildRequest("m", "claude",
		[]byte(`{"max_tokens":10,"messages":[{"role":"robot","content":"x"}]}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
		eErr.Message != `unsupported message role "robot" for /v1/responses` {
		t.Fatalf("garbage role = %+v", eErr)
	}
}

// FR-005 drift fix (shared.ClaudeMaxTokens): an absent or non-positive
// max_tokens defaults to 4096 instead of failing translation — the same
// policy the Chat Completions leg applies.
func TestFromClaudeMessagesMaxTokensDefault(t *testing.T) {
	for _, body := range []string{
		`{"messages":[]}`,
		`{"max_tokens":0,"messages":[]}`,
		`{"max_tokens":-5,"messages":[]}`,
	} {
		m := decodeReq(t, mustBuild(t, "m", "claude", []byte(body), nil))
		if m["max_output_tokens"] != float64(4096) {
			t.Fatalf("%s: max_output_tokens = %v, want 4096", body, m["max_output_tokens"])
		}
	}
}

func TestFromClaudeMessagesErrors(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		class errclass.Class
	}{
		{"malformed json", `{"max_tokens":`, errclass.ClassTranslation},
		{"malformed system", `{"max_tokens":10,"system":42}`, errclass.ClassTranslation},
		{"malformed message content", `{"max_tokens":10,"messages":[{"role":"user","content":42}]}`, errclass.ClassTranslation},
		{"image missing source", `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"image"}]}]}`, errclass.ClassTranslation},
		{"image url source empty", `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url"}}]}]}`, errclass.ClassTranslation},
		// Stricter shared converter: empty base64 data and unknown source
		// types are rejected instead of formatting a corrupt data: URL.
		{"image base64 source empty", `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`, errclass.ClassTranslation},
		{"image unknown source type", `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"s3"}}]}]}`, errclass.ClassTranslation},
		{"malformed tool_result payload", `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":42}]}]}`, errclass.ClassTranslation},
		{"image inside tool_result rejected", `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"https://x/i.png"}}]}]}]}`, errclass.ClassTranslation},
		{"unsupported block type", `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"document","source":{"type":"text"}}]}]}`, errclass.ClassUnsupported},
		{"malformed tool_choice", `{"max_tokens":10,"tool_choice":42}`, errclass.ClassTranslation},
		{"unknown tool_choice type", `{"max_tokens":10,"tool_choice":{"type":"blowup"}}`, errclass.ClassTranslation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "claude", []byte(tc.body), nil)
			wantErr(t, eErr, tc.class)
		})
	}
	// The tool_result image rejection names the target field, mirroring
	// the Chat Completions route's wording style — never silent loss.
	_, eErr := BuildRequest("m", "claude", []byte(
		`{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"https://x/i.png"}}]}]}]}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassTranslation ||
		!strings.Contains(eErr.Message, "function_call_output carries text only") {
		t.Fatalf("tool_result image not rejected descriptively: %+v", eErr)
	}
}

// Empty assistant tool_call arguments normalize to "{}" on the
// function_call item so downstream parsers always see valid JSON (FR-005).
func TestFromChatCompletionsEmptyToolArguments(t *testing.T) {
	out, eErr := BuildRequest("m", "openai", []byte(`{"messages":[
		{"role":"user","content":"ping"},
		{"role":"assistant","tool_calls":[{"id":"c9","type":"function","function":{"name":"ping","arguments":""}}]},
		{"role":"tool","tool_call_id":"c9","content":"pong"}
	]}`), nil)
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	var fnItem map[string]any
	for _, item := range inputItems(t, decodeReq(t, out)) {
		it := item.(map[string]any)
		if it["type"] == "function_call" && it["call_id"] == "c9" {
			fnItem = it
		}
	}
	if fnItem == nil {
		t.Fatal("function_call item missing")
	}
	if fnItem["arguments"] != "{}" || fnItem["name"] != "ping" {
		t.Fatalf("arguments = %v", fnItem["arguments"])
	}
}

// F-drift pin: CC's nested forced-tool tool_choice normalizes to the flat
// {type:function,name} shape /v1/responses expects; "none" maps to the
// Responses-native "none" exactly like the claude leg (protocol parity).
func TestFromChatCompletionsToolChoiceShapes(t *testing.T) {
	out, eErr := BuildRequest("m", "openai", []byte(`{"tool_choice":{"type":"function","function":{"name":"x"}},"messages":[]}`), nil)
	if eErr != nil {
		t.Fatalf("nested named: %v", eErr)
	}
	tc := decodeReq(t, out)["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["name"] != "x" {
		t.Fatalf("flat named form = %v", tc)
	}

	mNone, eErr := BuildRequest("m", "openai", []byte(`{"tool_choice":"none","messages":[]}`), nil)
	if eErr != nil {
		t.Fatalf("none: %v", eErr)
	}
	if tc := decodeReq(t, mNone)["tool_choice"]; tc != "none" {
		t.Fatalf("none tool_choice = %v", tc)
	}

	// any/required → "required"; malformed tool_choice JSON → translation.
	mAny, eErr := BuildRequest("m", "openai", []byte(`{"tool_choice":"any","messages":[]}`), nil)
	if eErr != nil {
		t.Fatalf("any: %v", eErr)
	}
	if tc := decodeReq(t, mAny)["tool_choice"]; tc != "required" {
		t.Fatalf("any tool_choice = %v", tc)
	}
	out, eErr = BuildRequest("m", "openai", []byte(`{"tool_choice":"required","messages":[]}`), nil)
	if eErr != nil {
		t.Fatalf("required: %v", eErr)
	}
	if tc := decodeReq(t, out)["tool_choice"]; tc != "required" {
		t.Fatalf("required tool_choice = %v", tc)
	}
	_, eErr = BuildRequest("m", "openai", []byte(`{"tool_choice":42,"messages":[]}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassTranslation ||
		!strings.Contains(eErr.Message, "malformed tool_choice") {
		t.Fatalf("malformed err = %v", eErr)
	}
}

// Exhaustiveness guard: respToolChoice is infallible because
// shared.DecodeToolChoice emits a closed set. Every kind in
// shared.AllToolChoiceKinds except absent must render a wire value; a new
// kind added to the slice without extending the renderer falls through to
// nil and fails here.
func TestRespToolChoiceKindsExhaustive(t *testing.T) {
	for _, kind := range shared.AllToolChoiceKinds {
		raw := respToolChoice(kind, "probe")
		if kind == shared.ToolChoiceAbsent {
			if raw != nil {
				t.Fatalf("%s: wire value = %s, want absent", kind, raw)
			}
			continue
		}
		if raw == nil {
			t.Fatalf("kind %q rendered no wire value; extend respToolChoice", kind)
		}
	}
}

// Images in Chat Completions tool results are rejected descriptively like
// the claude leg's tool_result policy — never silently dropped (FR-005).
func TestFromChatCompletionsImageInToolContentRejected(t *testing.T) {
	_, eErr := BuildRequest("m", "openai", []byte(
		`{"messages":[{"role":"tool","tool_call_id":"t","content":[
			{"type":"text","text":"a"},
			{"type":"image_url","image_url":{"url":"https://x/i.png"}}]}]}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassTranslation ||
		!strings.Contains(eErr.Message, "function_call_output carries text only") {
		t.Fatalf("tool image not rejected descriptively: %+v", eErr)
	}
}

// reasoning_effort is capability-gated like the reverse Responses→CC leg:
// unsupported levels fail descriptively, supported ones forward normalized.
func TestFromChatCompletionsEffortCapability(t *testing.T) {
	body := []byte(`{"messages":[],"reasoning_effort":" XHIGH "}`)
	if _, eErr := BuildRequest("m", "openai", body, nil); eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("unsupported effort = %v, want ClassUnsupported", eErr)
	}
	ts := &pluginapi.ThinkingSupport{Levels: []string{"low", "high", "xhigh"}}
	m := decodeReq(t, mustBuild(t, "m", "openai", body, ts))
	r := m["reasoning"].(map[string]any)
	if r["effort"] != "xhigh" {
		t.Fatalf("supported effort = %v, want xhigh", r)
	}
}

// Sentinels are capability-gated: a "none" the model does not declare and
// the dynamic "auto" sentinel are omitted — Responses has no off-switch, so
// omission is the no-forced-reasoning policy (matches the Messages-target
// leg); declared levels forward.
func TestFromChatCompletionsEffortSentinelsOmitted(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{ZeroAllowed: true, DynamicAllowed: true}
	m := decodeReq(t, mustBuild(t, "m", "openai",
		[]byte(`{"messages":[],"reasoning_effort":"none"}`), ts))
	if _, has := m["reasoning"]; has {
		t.Fatalf("none must omit reasoning: %v", m["reasoning"])
	}

	m = decodeReq(t, mustBuild(t, "m", "openai",
		[]byte(`{"messages":[],"reasoning_effort":"auto"}`), ts))
	if _, has := m["reasoning"]; has {
		t.Fatalf("auto must omit reasoning: %v", m["reasoning"])
	}

	tsHigh := &pluginapi.ThinkingSupport{Levels: []string{"low", "high"}}
	m = decodeReq(t, mustBuild(t, "m", "openai",
		[]byte(`{"messages":[],"reasoning_effort":"high"}`), tsHigh))
	if r := m["reasoning"].(map[string]any); r["effort"] != "high" {
		t.Fatalf("high effort = %v, want forwarded", r)
	}
}

// A validated effort "none" forwards verbatim when the model declares it
// (either via ZeroAllowed+Levels or Levels alone), matching the CC-upstream
// leg which forwards the identical validated value as-is (FR-005
// no-silent-loss). The undeclared case stays pinned by
// TestFromChatCompletionsEffortSentinelsOmitted.
func TestFromChatCompletionsEffortNoneDeclaredForwarded(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{ZeroAllowed: true, Levels: []string{"none", "low"}}
	m := decodeReq(t, mustBuild(t, "m", "openai",
		[]byte(`{"messages":[],"reasoning_effort":"none"}`), ts))
	if r := m["reasoning"].(map[string]any); r["effort"] != "none" {
		t.Fatalf("declared none = %v, want forwarded", r)
	}

	tsLevel := &pluginapi.ThinkingSupport{Levels: []string{"none", "high"}}
	m = decodeReq(t, mustBuild(t, "m", "openai",
		[]byte(`{"messages":[],"reasoning_effort":" NONE "}`), tsLevel))
	if r := m["reasoning"].(map[string]any); r["effort"] != "none" {
		t.Fatalf("level-declared none = %v, want normalized forward", r)
	}
}
