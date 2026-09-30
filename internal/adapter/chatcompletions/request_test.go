package chatcompletions

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/errclass"
)

func decodeOut(t *testing.T, out []byte, eErr *errclass.Error) map[string]any {
	t.Helper()
	if eErr != nil {
		t.Fatalf("unexpected error: %v", eErr)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	return m
}

func mustBuild(t *testing.T, sourceFormat, body string, ts *pluginapi.ThinkingSupport) map[string]any {
	t.Helper()
	out, eErr := BuildRequest("m", sourceFormat, []byte(body), ts)
	return decodeOut(t, out, eErr)
}

func TestAuthHeaders(t *testing.T) {
	h := AuthHeaders("sk-test")
	if got := h.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildRequestUnsupportedFormat(t *testing.T) {
	_, eErr := BuildRequest("m", "grpc", []byte(`{}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("want ClassUnsupported, got %+v", eErr)
	}
}

func TestBuildRequestOpenAI(t *testing.T) {
	body := `{"model":"pub/glm","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":32}`
	m := mustBuild(t, "openai", body, nil)
	if m["model"] != "m" || m["stream"] != true || m["max_tokens"] != float64(32) {
		t.Fatalf("passthrough broken: %v", m)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages changed: %v", msgs)
	}
	if _, eErr := BuildRequest("m", "openai", []byte(`{`), nil); eErr == nil || eErr.Class != errclass.ClassTranslation {
		t.Fatalf("want ClassTranslation, got %+v", eErr)
	}
}

func TestBuildRequest_OpenAISanitizeThinking(t *testing.T) {
	t.Run("untyped thinking metadata stripped", func(t *testing.T) {
		body := `{"model":"other","thinking":{"levels":["low","high","max"]},"messages":[{"role":"user","content":"hi"}],"stream":true}`
		out, eErr := BuildRequest("m", "openai", []byte(body), nil)
		m := decodeOut(t, out, eErr)
		if _, ok := m["thinking"]; ok {
			t.Fatalf("thinking should be stripped: %v", m)
		}
		if m["model"] != "m" || m["stream"] != true {
			t.Fatalf("expected fields missing or wrong: %v", m)
		}
		msgs, ok := m["messages"].([]any)
		if !ok || len(msgs) != 1 {
			t.Fatalf("messages altered: %v", m["messages"])
		}

		bodySameModel := `{"model":"m","thinking":{"levels":["low","high","max"]},"stream":true}`
		outSame, eErrSame := BuildRequest("m", "openai", []byte(bodySameModel), nil)
		mSame := decodeOut(t, outSame, eErrSame)
		if _, ok := mSame["thinking"]; ok {
			t.Fatalf("thinking should be stripped even if model matches: %v", mSame)
		}
		if mSame["model"] != "m" || mSame["stream"] != true {
			t.Fatalf("fields corrupted: %v", mSame)
		}
	})

	t.Run("non-object or malformed thinking stripped", func(t *testing.T) {
		cases := []struct {
			name     string
			thinking string
		}{
			{"string fast", `"fast"`},
			{"number 123", `123`},
			{"array low", `["low"]`},
			{"boolean true", `true`},
			{"null value", `null`},
			{"type is number", `{"type": 123}`},
			{"type is null", `{"type": null}`},
			{"type is array", `{"type": ["enabled"]}`},
			{"type is empty string", `{"type": ""}`},
			{"type is whitespace", `{"type": "   "}`},
			{"empty object", `{}`},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"other","thinking":%s,"stream":true}`, tc.thinking)
				out, eErr := BuildRequest("m", "openai", []byte(body), nil)
				m := decodeOut(t, out, eErr)
				if _, ok := m["thinking"]; ok {
					t.Fatalf("%s: thinking should be stripped, got %v", tc.name, m["thinking"])
				}
				if m["model"] != "m" || m["stream"] != true {
					t.Fatalf("%s: fields altered: %v", tc.name, m)
				}
			})
		}
	})

	t.Run("valid thinking object preserved", func(t *testing.T) {
		cases := []struct {
			name     string
			model    string
			thinking string
		}{
			{"enabled with budget model rewrite", "old-model", `{"type":"enabled","budget_tokens":1024}`},
			{"enabled with budget same model", "m", `{"type":"enabled","budget_tokens":1024}`},
			{"disabled same model", "m", `{"type":"disabled"}`},
			{"disabled model rewrite", "old-model", `{"type":"disabled"}`},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				body := fmt.Sprintf(`{"model":%q,"thinking":%s,"stream":true}`, tc.model, tc.thinking)
				out, eErr := BuildRequest("m", "openai", []byte(body), nil)
				if eErr != nil {
					t.Fatalf("unexpected error: %v", eErr)
				}
				if tc.model == "m" && string(out) != body {
					t.Fatalf("expected raw body passthrough, got: %s", string(out))
				}
				m := decodeOut(t, out, eErr)
				if m["model"] != "m" {
					t.Fatalf("model = %v, want m", m["model"])
				}
				th, ok := m["thinking"].(map[string]any)
				if !ok {
					t.Fatalf("thinking missing or not object: %v", m["thinking"])
				}
				if strings.Contains(tc.name, "enabled") {
					if th["type"] != "enabled" || th["budget_tokens"] != float64(1024) {
						t.Fatalf("thinking object content corrupted: %v", th)
					}
				} else {
					if th["type"] != "disabled" {
						t.Fatalf("thinking object content corrupted: %v", th)
					}
				}
			})
		}
	})

	t.Run("reasoning_effort preserved without thinking", func(t *testing.T) {
		body := `{"model":"old-model","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`
		out, eErr := BuildRequest("m", "openai", []byte(body), nil)
		m := decodeOut(t, out, eErr)
		if m["model"] != "m" {
			t.Fatalf("model = %v, want m", m["model"])
		}
		if m["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %v, want high", m["reasoning_effort"])
		}
		if _, ok := m["thinking"]; ok {
			t.Fatalf("thinking should not be present: %v", m)
		}

		bodySame := `{"model":"m","reasoning_effort":"high"}`
		outSame, eErrSame := BuildRequest("m", "openai", []byte(bodySame), nil)
		if eErrSame != nil {
			t.Fatalf("unexpected error: %v", eErrSame)
		}
		if string(outSame) != bodySame {
			t.Fatalf("expected raw body passthrough, got: %s", string(outSame))
		}
	})

	t.Run("clean model rewrite without thinking", func(t *testing.T) {
		body := `{"model":"old-model","temperature":0.7}`
		out, eErr := BuildRequest("m", "openai", []byte(body), nil)
		m := decodeOut(t, out, eErr)
		if m["model"] != "m" || m["temperature"] != 0.7 {
			t.Fatalf("model rewrite failed: %v", m)
		}
		if _, ok := m["thinking"]; ok {
			t.Fatalf("thinking should not be present: %v", m)
		}
	})

	t.Run("nil or empty json handling", func(t *testing.T) {
		if _, eErr := BuildRequest("m", "openai", []byte(""), nil); eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Fatalf("empty slice: want ClassTranslation, got %+v", eErr)
		}

		outNull, eErrNull := BuildRequest("m", "openai", []byte("null"), nil)
		if eErrNull == nil || eErrNull.Class != errclass.ClassTranslation {
			t.Fatalf("null JSON: want ClassTranslation, got %+v", eErrNull)
		}
		if !strings.Contains(eErrNull.Message, "JSON null is not a valid request") {
			t.Fatalf("null JSON: unexpected error message: %q", eErrNull.Message)
		}
		if outNull != nil {
			t.Fatalf("null JSON: expected nil output, got %s", string(outNull))
		}

		for _, invalid := range []string{"123", `"string"`, `[]`, `true`} {
			if _, eErr := BuildRequest("m", "openai", []byte(invalid), nil); eErr == nil || eErr.Class != errclass.ClassTranslation {
				t.Fatalf("%s: want ClassTranslation, got %+v", invalid, eErr)
			}
		}
	})
}

// TestBuildRequestThinkingEffort proves budget→effort conversion is
// capability-aware (FR-005): tiers beyond the static low/medium/high
// buckets survive when the model declares them.
func TestBuildRequestThinkingEffort(t *testing.T) {
	cases := []struct {
		name   string
		ts     *pluginapi.ThinkingSupport
		budget int64
		want   string
	}{
		{"nil ts small budget anchors low", nil, 1024, "low"},
		{"nil ts mid budget anchors medium", nil, 8192, "medium"},
		{"nil ts unknown huge budget anchors high", nil, 131072, "high"},
		{"declared xhigh tier preserved", &pluginapi.ThinkingSupport{
			Levels: []string{"low", "medium", "high", "xhigh"}, Max: 64000,
		}, 60000, "xhigh"},
		{"zero allowed picks none", &pluginapi.ThinkingSupport{
			ZeroAllowed: true, Levels: []string{"none", "low", "high"},
		}, 0, "none"},
		{"zero allowed without none falls to lowest", &pluginapi.ThinkingSupport{
			ZeroAllowed: true, Levels: []string{"low", "high"},
		}, 0, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"model":"x","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":%d}}`,
				tc.budget)
			out, eErr := BuildRequest("m", "claude", []byte(body), tc.ts)
			m := decodeOut(t, out, eErr)
			if m["reasoning_effort"] != tc.want {
				t.Fatalf("reasoning_effort=%v want %q", m["reasoning_effort"], tc.want)
			}
		})
	}
	m := mustBuild(t, "claude", `{"max_tokens":64,"thinking":{"type":"disabled"}}`, nil)
	if _, ok := m["reasoning_effort"]; ok {
		t.Fatalf("disabled thinking must not set reasoning_effort: %v", m)
	}
}

func TestBuildRequestClaude(t *testing.T) {
	body := `{
		"model":"x","max_tokens":256,"stream":true,"temperature":0.5,"top_p":0.9,
		"system":[{"type":"text","text":"sys"}],
		"stop_sequences":["END"],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"text","text":"hey"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"res"},{"type":"text","text":"more"}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","id":"t2","name":"g"}]}
		],
		"tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}]
	}`
	m := mustBuild(t, "claude", body, nil)
	if m["model"] != "m" || m["stream"] != true || m["max_tokens"] != float64(256) ||
		m["temperature"] != 0.5 || m["top_p"] != 0.9 {
		t.Fatalf("envelope fields wrong: %v", m)
	}
	if fmt.Sprint(m["stop"]) != "[END]" {
		t.Fatalf("stop wrong: %v", m["stop"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("want 6 messages, got %d: %v", len(msgs), msgs)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "sys" {
		t.Fatalf("system message wrong: %v", sys)
	}
	asst := msgs[2].(map[string]any)
	if asst["role"] != "assistant" || asst["content"] != "hey" {
		t.Fatalf("assistant message wrong: %v", asst)
	}
	calls := asst["tool_calls"].([]any)
	c0 := calls[0].(map[string]any)
	if c0["id"] != "t1" || c0["type"] != "function" {
		t.Fatalf("tool call wrong: %v", c0)
	}
	fn := c0["function"].(map[string]any)
	if fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("tool call function wrong: %v", fn)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "t1" || tool["content"] != "res" {
		t.Fatalf("tool result message wrong: %v", tool)
	}
	user := msgs[4].(map[string]any)
	if user["content"] != "more" {
		t.Fatalf("post-result text wrong: %v", user)
	}
	// thinking block dropped, tool_use kept: content null + tool_calls.
	last := msgs[5].(map[string]any)
	if last["role"] != "assistant" || last["content"] != nil {
		t.Fatalf("thinking-only content wrong: %v", last)
	}
	lastCall := last["tool_calls"].([]any)[0].(map[string]any)
	if lastCall["id"] != "t2" || lastCall["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("missing-input tool call wrong: %v", lastCall)
	}
	tools := m["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["type"] != "function" {
		t.Fatalf("tool type wrong: %v", tl)
	}
	tlf := tl["function"].(map[string]any)
	if tlf["name"] != "f" || tlf["description"] != "d" ||
		fmt.Sprint(tlf["parameters"]) != "map[type:object]" {
		t.Fatalf("tool function wrong: %v", tlf)
	}
}

func TestBuildRequestClaudeEmptyToolSchema(t *testing.T) {
	m := mustBuild(t, "claude", `{"tools":[{"name":"f"}],"messages":[{"role":"user","content":null}]}`, nil)
	fn := m["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fmt.Sprint(fn["parameters"]) != "map[properties:map[] type:object]" {
		t.Fatalf("schema default missing: %v", fn)
	}
	if msgs := m["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("null content must not produce messages: %v", msgs)
	}
}

// FR-005 drift fix (shared.ClaudeMaxTokens): an absent or non-positive
// max_tokens defaults to 4096 instead of silently dropping the cap —
// the same policy the Responses leg applies.
func TestBuildRequestClaudeMaxTokensDefault(t *testing.T) {
	for _, body := range []string{
		`{"messages":[]}`,
		`{"max_tokens":0,"messages":[]}`,
		`{"max_tokens":-5,"messages":[]}`,
	} {
		m := mustBuild(t, "claude", body, nil)
		if m["max_tokens"] != float64(4096) {
			t.Fatalf("%s: max_tokens = %v, want 4096", body, m["max_tokens"])
		}
	}
}

func TestBuildRequestClaudeErrors(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed json", `{`},
		{"malformed block", `{"messages":[{"role":"user","content":[42]}]}`},
		{"user content wrong shape", `{"messages":[{"role":"user","content":42}]}`},
		{"assistant content wrong shape", `{"messages":[{"role":"assistant","content":42}]}`},
		{"image missing source", `{"messages":[{"role":"user","content":[{"type":"image"}]}]}`},
		{"source not an object", `{"messages":[{"role":"user","content":[{"type":"image","source":"x"}]}]}`},
		{"url source missing url", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url"}}]}]}`},
		{"base64 missing data", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`},
		{"unknown source type", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"s3"}}]}]}`},
		{"result bad block type", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"video"}]}]}]}`},
		{"result malformed block", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[42]}]}]}`},
		{"result content wrong shape", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":42}]}]}`},
		{"assistant malformed block", `{"messages":[{"role":"assistant","content":[42]}]}`},
		{"system bad block type", `{"system":[{"type":"doc"}]}`},
		{"system wrong shape", `{"system":123}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "claude", []byte(tc.body), nil)
			if eErr == nil || eErr.Class != errclass.ClassTranslation {
				t.Fatalf("want ClassTranslation, got %+v", eErr)
			}
		})
	}
	// Unknown content-block types and unknown roles are
	// unsupported_protocol_or_parameter (FR-009 kernel), matching the
	// Responses route.
	for _, c := range []struct{ name, body, typ string }{
		{"bad user block type", `{"messages":[{"role":"user","content":[{"type":"video"}]}]}`, `"video"`},
		{"bad assistant block type", `{"messages":[{"role":"assistant","content":[{"type":"video"}]}]}`, `"video"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "claude", []byte(c.body), nil)
			if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
				!strings.Contains(eErr.Message, c.typ) || !strings.Contains(eErr.Message, EndpointPath) {
				t.Fatalf("want ClassUnsupported naming type+endpoint, got %+v", eErr)
			}
		})
	}
	_, eErr := BuildRequest("m", "claude", []byte(`{"messages":[{"role":"robot","content":"x"}]}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
		!strings.Contains(eErr.Message, `"robot"`) {
		t.Fatalf("unsupported role: want ClassUnsupported, got %+v", eErr)
	}
}

func TestBuildRequestClaudeImageURLs(t *testing.T) {
	m := mustBuild(t, "claude",
		`{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image","source":{"type":"url","url":"https://x/i.png"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},{"type":"image","source":{"type":"base64","data":"QUJD"}}]}]}`, nil)
	parts := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 4 || parts[0].(map[string]any)["type"] != "text" {
		t.Fatalf("parts wrong: %v", parts)
	}
	if got := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]; got != "https://x/i.png" {
		t.Fatalf("plain url lost: %v", got)
	}
	if got := parts[2].(map[string]any)["image_url"].(map[string]any)["url"]; got != "data:image/png;base64,QUJD" {
		t.Fatalf("base64 not converted to data URL: %v", got)
	}
	if got := parts[3].(map[string]any)["image_url"].(map[string]any)["url"]; got != "data:application/octet-stream;base64,QUJD" {
		t.Fatalf("missing media_type not defaulted: %v", got)
	}
}

func TestBuildRequestClaudeVariants(t *testing.T) {
	t.Run("string system field", func(t *testing.T) {
		m := mustBuild(t, "claude", `{"system":"plain","messages":[{"role":"user","content":"hi"}]}`, nil)
		sys := m["messages"].([]any)[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "plain" {
			t.Fatalf("string system lost: %v", sys)
		}
	})
	t.Run("multi-block system joined with blank lines", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"system":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}],"messages":[{"role":"user","content":"hi"}]}`, nil)
		sys := m["messages"].([]any)[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "s1\n\ns2" {
			t.Fatalf("system join wrong: %v", sys)
		}
	})
	t.Run("string assistant content", func(t *testing.T) {
		m := mustBuild(t, "claude", `{"messages":[{"role":"assistant","content":"str"}]}`, nil)
		a := m["messages"].([]any)[0].(map[string]any)
		if a["role"] != "assistant" || a["content"] != "str" {
			t.Fatalf("assistant string content lost: %v", a)
		}
	})
	t.Run("thinking-only assistant dropped", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("thinking-only assistant not dropped: %v", msgs)
		}
	})
	// FR-005 omission policy parity: redacted_thinking is encrypted
	// reasoning metadata with no Chat Completions representable payload —
	// omitted like plain thinking, never a conversion failure (mirrors the
	// sibling Messages/Responses-route converters).
	t.Run("redacted_thinking omitted alongside thinking", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"redacted_thinking","data":"x"},{"type":"text","text":"answer"}]}]}`, nil)
		msgs := m["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("want one assistant message, got %d: %v", len(msgs), msgs)
		}
		a := msgs[0].(map[string]any)
		if a["content"] != "answer" {
			t.Fatalf("thinking blocks not omitted or text lost: %v", a)
		}
	})
	t.Run("redacted_thinking-only assistant dropped", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("redacted_thinking-only assistant not dropped: %v", msgs)
		}
	})
	t.Run("tool_result without content", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9"}]}]}`, nil)
		tool := m["messages"].([]any)[0].(map[string]any)
		if tool["role"] != "tool" || tool["tool_call_id"] != "t9" || tool["content"] != "" {
			t.Fatalf("contentless tool_result wrong: %v", tool)
		}
	})
	t.Run("tool_result block array content", func(t *testing.T) {
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}]}`, nil)
		tool := m["messages"].([]any)[0].(map[string]any)
		if tool["content"] != "ab" {
			t.Fatalf("block array result not flattened: %v", tool)
		}
	})
	t.Run("tool_result is_error marker", func(t *testing.T) {
		// is_error must survive as the "[error] " prefix like the
		// Responses-route twin (responses/request.go), never silently lost.
		m := mustBuild(t, "claude",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":"boom","is_error":true}]}]}`, nil)
		tool := m["messages"].([]any)[0].(map[string]any)
		if tool["content"] != "[error] boom" {
			t.Fatalf("is_error marker missing: %v", tool)
		}
	})
}

func TestBuildRequestResponses(t *testing.T) {
	body := `{
		"model":"x","max_output_tokens":99,"temperature":0.7,
		"instructions":"be nice",
		"input":[
			{"type":"message","role":"developer","content":"dev"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"q"},{"type":"input_image","image_url":{"url":"https://x/i.png"}}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"x\":1}"},
			{"type":"function_call","call_id":"c2","name":"g","arguments":""},
			{"type":"function_call_output","call_id":"c1","output":"ok"},
			{"type":"reasoning","summary":[]}
		],
		"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],
		"reasoning":{"effort":"HIGH"}
	}`
	m := mustBuild(t, "openai-response", body, nil)
	if m["max_tokens"] != float64(99) || m["reasoning_effort"] != "high" || m["temperature"] != 0.7 {
		t.Fatalf("envelope fields wrong: %v", m)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("want 6 messages, got %d: %v", len(msgs), msgs)
	}
	if m0 := msgs[0].(map[string]any); m0["role"] != "system" || m0["content"] != "be nice" {
		t.Fatalf("instructions message wrong: %v", m0)
	}
	if m1 := msgs[1].(map[string]any); m1["role"] != "system" || m1["content"] != "dev" {
		t.Fatalf("developer item wrong: %v", m1)
	}
	user := msgs[2].(map[string]any)["content"].([]any)
	if len(user) != 2 || user[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("user parts wrong: %v", user)
	}
	if msgs[3].(map[string]any)["content"] != "a" {
		t.Fatalf("assistant item wrong: %v", msgs[3])
	}
	merged := msgs[4].(map[string]any)
	calls := merged["tool_calls"].([]any)
	if merged["role"] != "assistant" || len(calls) != 2 {
		t.Fatalf("consecutive function_calls not merged: %v", merged)
	}
	c1 := calls[0].(map[string]any)
	if c1["id"] != "c1" || c1["function"].(map[string]any)["arguments"] != `{"x":1}` {
		t.Fatalf("first call wrong: %v", c1)
	}
	if calls[1].(map[string]any)["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("empty arguments not defaulted: %v", calls[1])
	}
	outMsg := msgs[5].(map[string]any)
	if outMsg["role"] != "tool" || outMsg["tool_call_id"] != "c1" || outMsg["content"] != "ok" {
		t.Fatalf("function_call_output wrong: %v", outMsg)
	}
	tools := m["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["type"] != "function" || tl["function"].(map[string]any)["name"] != "f" {
		t.Fatalf("tool wrapping wrong: %v", tl)
	}
}

func TestBuildRequestResponsesVariants(t *testing.T) {
	t.Run("empty string input yields no messages", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"input":""}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("unexpected messages: %v", msgs)
		}
	})
	t.Run("no input and no instructions", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("unexpected messages: %v", msgs)
		}
	})
	t.Run("string input becomes user message", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"input":"hi"}`, nil)
		msgs := m["messages"].([]any)
		if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hi" {
			t.Fatalf("wrong: %v", msgs)
		}
	})
	t.Run("zero max_output_tokens ignored", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"max_output_tokens":0,"input":"hi"}`, nil)
		if _, ok := m["max_tokens"]; ok {
			t.Fatalf("zero max_output_tokens must be omitted: %v", m)
		}
	})
	t.Run("parallel_tool_calls false forwarded", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"input":"hi","parallel_tool_calls":false}`, nil)
		if v, ok := m["parallel_tool_calls"].(bool); !ok || v {
			t.Fatalf("false parallel_tool_calls not forwarded: %v", m)
		}
	})
	t.Run("absent parallel_tool_calls omitted", func(t *testing.T) {
		m := mustBuild(t, "openai-response", `{"input":"hi"}`, nil)
		if _, ok := m["parallel_tool_calls"]; ok {
			t.Fatalf("absent parallel_tool_calls must be omitted: %v", m)
		}
	})
	t.Run("plain string image url", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/i.png"}]}]}`, nil)
		content := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if content[0].(map[string]any)["image_url"].(map[string]any)["url"] != "https://x/i.png" {
			t.Fatalf("plain-string image_url lost: %v", content)
		}
	})
	t.Run("null message content skipped", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"user","content":null},{"type":"message","role":"system","content":null}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("null content produced messages: %v", msgs)
		}
	})
	t.Run("empty string content skipped", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"user","content":""}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("empty content produced messages: %v", msgs)
		}
	})
	t.Run("empty parts array skipped", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"user","content":[]}]}`, nil)
		if msgs := m["messages"].([]any); len(msgs) != 0 {
			t.Fatalf("empty parts produced messages: %v", msgs)
		}
	})
	t.Run("system text parts joined", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"a"},{"type":"output_text","text":"b"}]}]}`, nil)
		sys := m["messages"].([]any)[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "ab" {
			t.Fatalf("system parts wrong: %v", sys)
		}
	})
	t.Run("tool without parameters gets default schema", func(t *testing.T) {
		m := mustBuild(t, "openai-response",
			`{"tools":[{"type":"function","name":"f"}],"input":"hi"}`, nil)
		fn := m["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
		if fmt.Sprint(fn["parameters"]) != "map[properties:map[] type:object]" {
			t.Fatalf("schema default missing: %v", fn)
		}
	})
}

func TestBuildRequestResponsesErrors(t *testing.T) {
	cases := []struct{ name, body string }{
		{"malformed json", `{`},
		{"instructions not a string", `{"instructions":5}`},
		{"input wrong shape", `{"input":5}`},
		{"system with image part", `{"input":[{"type":"message","role":"system","content":[{"type":"input_image","image_url":{"url":"https://x"}}]}]}`},
		{"image missing url", `{"input":[{"type":"message","role":"user","content":[{"type":"input_image"}]}]}`},
		{"malformed content", `{"input":[{"type":"message","role":"user","content":{"bad":1}}]}`},
		{"developer content wrong shape", `{"input":[{"type":"message","role":"developer","content":{"bad":1}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "openai-response", []byte(tc.body), nil)
			if eErr == nil || eErr.Class != errclass.ClassTranslation {
				t.Fatalf("want ClassTranslation, got %+v", eErr)
			}
		})
	}
	// Unknown roles, content part types, and input item types are
	// unsupported_protocol_or_parameter (FR-009 kernel), not translation.
	for _, tc := range []struct{ name, body string }{
		{"unsupported role", `{"input":[{"type":"message","role":"robot","content":"x"}]}`},
		{"unsupported part type", `{"input":[{"type":"message","role":"user","content":[{"type":"audio"}]}]}`},
		{"unsupported item type", `{"input":[{"type":"web_search"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, eErr := BuildRequest("m", "openai-response", []byte(tc.body), nil)
			if eErr == nil || eErr.Class != errclass.ClassUnsupported {
				t.Fatalf("want ClassUnsupported, got %+v", eErr)
			}
		})
	}
	// A non-function tool type is unsupported_protocol_or_parameter (FR-009).
	_, eErr := BuildRequest("m", "openai-response", []byte(`{"tools":[{"type":"web_search"}]}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("unsupported tool type: want ClassUnsupported, got %+v", eErr)
	}
}

func TestResponsesReasoningEffortValidated(t *testing.T) {
	// ts nil → default levels {low,medium,high}: unsupported value rejected
	// naming it; a supported level forwards verbatim.
	_, eErr := BuildRequest("m", "openai-response",
		[]byte(`{"reasoning":{"effort":"xhigh"},"input":"hi"}`), nil)
	if eErr == nil || eErr.Class != errclass.ClassUnsupported ||
		!strings.Contains(eErr.Message, `"xhigh"`) {
		t.Fatalf("unsupported effort err = %+v", eErr)
	}
	out := mustBuild(t, "openai-response", `{"reasoning":{"effort":"high"},"input":"hi"}`, nil)
	if out["reasoning_effort"] != "high" {
		t.Fatalf("supported effort = %v", out["reasoning_effort"])
	}

	// ts declaring xhigh admits it.
	ts := &pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}
	out = mustBuild(t, "openai-response", `{"reasoning":{"effort":"XHigh"},"input":"hi"}`, ts)
	if out["reasoning_effort"] != "xhigh" {
		t.Fatalf("capability-aware effort = %v", out["reasoning_effort"])
	}
}

// F-drift pins: tool_choice preserved on both CC-target cross-format legs.
func TestClaudeSourceToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "claude",
		`{"tool_choice":{"type":"auto","name":""},"messages":[{"role":"user","content":"hi"}]}`, nil)
	if m["tool_choice"] != "auto" {
		t.Fatalf("auto tool_choice = %v", m["tool_choice"])
	}
	m = mustBuild(t, "claude",
		`{"tool_choice":{"type":"tool","name":"lookup"},"messages":[{"role":"user","content":"hi"}]}`, nil)
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" ||
		tc["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("named tool_choice = %v", m["tool_choice"])
	}
	m = mustBuild(t, "claude",
		`{"tool_choice":{"type":"any"},"messages":[]}`, nil)
	if m["tool_choice"] != "required" {
		t.Fatalf("any tool_choice = %v", m["tool_choice"])
	}
}

func TestResponsesSourceToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "openai-response",
		`{"tool_choice":"auto","input":"hi"}`, nil)
	if m["tool_choice"] != "auto" {
		t.Fatalf("auto tool_choice = %v", m["tool_choice"])
	}
	m = mustBuild(t, "openai-response",
		`{"tool_choice":{"type":"function","name":"lookup"},"input":"hi"}`, nil)
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" ||
		tc["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("named tool_choice = %v", m["tool_choice"])
	}
}

// tool_choice "none" is natively representable on CC-target legs and is
// preserved instead of rejected (FR-005).

func TestClaudeSourceNoneToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "claude",
		`{"tool_choice":{"type":"none","name":""},"messages":[]}`, nil)
	if m["tool_choice"] != "none" {
		t.Fatalf("none tool_choice = %v", m["tool_choice"])
	}
}

func TestResponsesSourceNoneToolChoicePreserved(t *testing.T) {
	m := mustBuild(t, "openai-response",
		`{"tool_choice":"none","input":"hi"}`, nil)
	if m["tool_choice"] != "none" {
		t.Fatalf("none tool_choice = %v", m["tool_choice"])
	}
}

// applyToolChoiceCC maps every normalized kind onto the CC wire format.

func TestApplyToolChoiceCC(t *testing.T) {
	cases := []struct {
		kind, name string
		want       any // nil asserts the field stays absent
	}{
		{shared.ToolChoiceAbsent, "", nil},
		{shared.ToolChoiceAuto, "", "auto"},
		{shared.ToolChoiceAny, "", "required"},
		{shared.ToolChoiceNone, "", "none"},
	}
	for _, tc := range cases {
		req := &ccRequest{}
		applyToolChoiceCC(req, tc.kind, tc.name)
		b, _ := json.Marshal(req)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		got, ok := m["tool_choice"]
		if got != tc.want || ok != (tc.want != nil) {
			t.Fatalf("%s: tool_choice = %v (present=%t), want %v", tc.kind, got, ok, tc.want)
		}
	}

	req := &ccRequest{}
	applyToolChoiceCC(req, shared.ToolChoiceNamed, "lookup")
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(req.ToolChoice, &named); err != nil ||
		named.Type != "function" || named.Function.Name != "lookup" {
		t.Fatalf("named tool_choice = %s", req.ToolChoice)
	}
}

// Exhaustiveness guard: applyToolChoiceCC is infallible because
// shared.DecodeToolChoice emits a closed set. Every kind in
// shared.AllToolChoiceKinds except absent must render a wire value; a new
// kind added to the slice without extending the renderer falls through to
// nothing and fails here.
func TestApplyToolChoiceCCKindsExhaustive(t *testing.T) {
	for _, kind := range shared.AllToolChoiceKinds {
		req := &ccRequest{}
		applyToolChoiceCC(req, kind, "probe")
		if kind == shared.ToolChoiceAbsent {
			if req.ToolChoice != nil {
				t.Fatalf("%s: tool_choice = %s, want absent", kind, req.ToolChoice)
			}
			continue
		}
		if req.ToolChoice == nil {
			t.Fatalf("kind %q rendered no tool_choice; extend applyToolChoiceCC", kind)
		}
	}
}

// Malformed tool_choice fails translation on both CC-target legs instead
// of being silently dropped.
func TestMalformedToolChoiceRejected(t *testing.T) {
	for _, format := range []string{"claude", "openai-response"} {
		body := `{"tool_choice":42,"messages":[]}`
		if format == "openai-response" {
			body = `{"tool_choice":42,"input":"hi"}`
		}
		_, eErr := BuildRequest("m", format, []byte(body), nil)
		if eErr == nil || !strings.Contains(eErr.Message, "malformed tool_choice") {
			t.Fatalf("%s: err = %+v", format, eErr)
		}
	}
}

func TestBuildOpenAIRequestDeveloperRole(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"developer","content":"system instructions"},{"role":"user","content":"hi"}]}`
	out, eErr := BuildRequest("deepseek-v4.1-flash", "openai", []byte(body), nil)
	m := decodeOut(t, out, eErr)
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) < 1 {
		t.Fatalf("invalid messages: %v", m["messages"])
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("invalid message format: %v", msgs[0])
	}
	if got := first["role"]; got != "system" {
		t.Fatalf("expected role %q, got %q", "system", got)
	}
}

