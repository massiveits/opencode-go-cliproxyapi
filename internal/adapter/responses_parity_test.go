// Package adapters hosts cross-route tests that compare the synthesized
// wire bytes of sibling protocol adapters against each other (FR-006).
package adapters

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/chatcompletions"
	"opencode-go-cliproxyapi/internal/adapter/messages"
	"opencode-go-cliproxyapi/internal/adapter/responses"
	"opencode-go-cliproxyapi/internal/errclass"
)

func TestReasoningEffortNativeAndTranslatedParity(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{Levels: []string{"minimal", "low", "medium", "high", "xhigh", "max"}, ZeroAllowed: true, DynamicAllowed: true}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "auto"} {
		t.Run(effort, func(t *testing.T) {
			cc := []byte(`{"model":"m","messages":[],"reasoning_effort":"` + effort + `"}`)
			resp := []byte(`{"model":"m","input":[],"reasoning":{"effort":"` + effort + `"}}`)
			for _, tc := range []struct {
				name   string
				build  func(string, string, []byte, *pluginapi.ThinkingSupport) ([]byte, *errclass.Error)
				format string
				body   []byte
				field  string
			}{
				{"native chat", chatcompletions.BuildRequest, "openai", cc, "reasoning_effort"},
				{"translated chat", chatcompletions.BuildRequest, "openai-response", resp, "reasoning_effort"},
				{"native responses", responses.BuildRequest, "openai-response", resp, "reasoning"},
				{"translated responses", responses.BuildRequest, "openai", cc, "reasoning"},
			} {
				out, eErr := tc.build("m", tc.format, tc.body, ts)
				if eErr != nil {
					t.Fatalf("%s: %v", tc.name, eErr)
				}
				var wire map[string]any
				if err := json.Unmarshal(out, &wire); err != nil {
					t.Fatal(err)
				}
				if tc.field == "reasoning_effort" && wire[tc.field] != effort {
					t.Errorf("%s: %s = %v", tc.name, tc.field, wire[tc.field])
				}
				if tc.field == "reasoning" {
					if effort == "auto" && wire[tc.field] != nil {
						t.Errorf("%s: auto must omit Responses reasoning: %v", tc.name, wire[tc.field])
					} else if effort != "auto" {
						got, ok := wire[tc.field].(map[string]any)
						if !ok || got["effort"] != effort {
							t.Errorf("%s: reasoning = %v", tc.name, wire[tc.field])
						}
					}
				}
			}
			for _, source := range []struct {
				format string
				body   []byte
			}{{"openai", cc}, {"openai-response", resp}} {
				out, eErr := messages.BuildRequest("m", source.format, source.body, ts)
				if eErr != nil {
					t.Fatalf("Messages from %s: %v", source.format, eErr)
				}
				var wire map[string]any
				if err := json.Unmarshal(out, &wire); err != nil {
					t.Fatal(err)
				}
				if (wire["thinking"] != nil) != (effort != "none" && effort != "auto") {
					t.Errorf("Messages from %s: thinking = %v", source.format, wire["thinking"])
				}
			}
		})
	}
	limited := &pluginapi.ThinkingSupport{Levels: []string{"high"}}
	for _, body := range []struct {
		format string
		data   []byte
		build  func(string, string, []byte, *pluginapi.ThinkingSupport) ([]byte, *errclass.Error)
	}{
		{"openai", []byte(`{"model":"m","reasoning_effort":"max"}`), chatcompletions.BuildRequest},
		{"openai-response", []byte(`{"model":"m","reasoning":{"effort":"max"}}`), responses.BuildRequest},
	} {
		if _, eErr := body.build("m", body.format, body.data, limited); eErr == nil || eErr.Class != errclass.ClassUnsupported {
			t.Errorf("native %s accepted unsupported max: %v", body.format, eErr)
		}
	}
	claudeOff := []byte(`{"model":"m","max_tokens":1024,"messages":[],"thinking":{"type":"disabled"}}`)
	if _, eErr := messages.BuildRequest("m", "claude", claudeOff, limited); eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Errorf("native Messages accepted unsupported reasoning off: %v", eErr)
	}
	if _, eErr := messages.BuildRequest("m", "claude", claudeOff, ts); eErr != nil {
		t.Errorf("native Messages rejected supported reasoning off: %v", eErr)
	}
}

type feeder interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
}

// F5 route parity: one logical turn — a single user message whose content
// is "" — must receive the IDENTICAL empty-input policy on every
// cross-route normalizer: DROP it. Divergence pin: the Responses target
// used to emit a live empty input_text part while the Messages and Chat
// Completions targets dropped the whole message.
func TestEmptyStringContentParityAcrossTargets(t *testing.T) {
	ccBody := []byte(`{"model":"x","messages":[{"role":"user","content":""}]}`)
	respBody := []byte(`{"model":"x","input":[{"type":"message","role":"user","content":""}]}`)

	checkDropped := func(t *testing.T, out []byte, field string) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("output not json: %v", err)
		}
		switch v := m[field].(type) {
		case nil:
		case []any:
			if len(v) != 0 {
				t.Fatalf("empty-content message must be dropped uniformly, %s = %v", field, v)
			}
		default:
			t.Fatalf("%s has unexpected shape %T", field, v)
		}
	}

	t.Run("responses target", func(t *testing.T) {
		out, eErr := responses.BuildRequest("m", "openai", ccBody, nil)
		if eErr != nil {
			t.Fatalf("unexpected error: %v", eErr)
		}
		checkDropped(t, out, "input")
	})
	t.Run("messages target", func(t *testing.T) {
		out, eErr := messages.BuildRequest("m", "openai", ccBody, nil)
		if eErr != nil {
			t.Fatalf("unexpected error: %v", eErr)
		}
		checkDropped(t, out, "messages")
	})
	t.Run("chatcompletions target", func(t *testing.T) {
		out, eErr := chatcompletions.BuildRequest("m", "openai-response", respBody, nil)
		if eErr != nil {
			t.Fatalf("unexpected error: %v", eErr)
		}
		checkDropped(t, out, "messages")
	})
}

// FR-006 route parity: equivalent upstream streams — Anthropic Messages
// and Chat Completions — must synthesize byte-equal openai-response event
// sequences through the shared ResponsesEventEmitter kernel. Only
// created_at (render-time fallback, never observed upstream) is
// normalized before the byte comparison.
func TestResponsesSynthesisRouteParity(t *testing.T) {
	claudeUp := []string{
		"event: message_start\n" + `data: {"type":"message_start","message":{"id":"resp_1","model":"m1","usage":{"input_tokens":2}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"index":0,"content_block":{"type":"text"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"index":0,"delta":{"type":"text_delta","text":"He"}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"index":1,"content_block":{"type":"tool_use","id":"call_1","name":"f"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}
	ccUp := []string{
		"data: {\"id\":\"resp_1\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"He\"}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n",
		"data: [DONE]\n",
	}

	run := func(t *testing.T, f feeder, chunks []string) string {
		t.Helper()
		var b strings.Builder
		for _, c := range chunks {
			evs, done, eErr := f.Feed([]byte(c))
			if eErr != nil {
				t.Fatalf("feed %q: %v", c, eErr)
			}
			for _, ev := range evs {
				b.Write(ev)
			}
			if done {
				break
			}
		}
		return b.String()
	}

	fromClaude := run(t, messages.NewStreamConverter("openai-response"), claudeUp)
	fromCC := run(t, chatcompletions.NewStreamConverter("openai-response"), ccUp)

	norm := func(s string) string {
		return regexp.MustCompile(`"created_at":\d+`).ReplaceAllString(s, `"created_at":0`)
	}
	a, b := norm(fromClaude), norm(fromCC)
	if a != b {
		t.Fatalf("routes diverge:\nclaude-source: %s\ncc-source:     %s", a, b)
	}
	for _, want := range []string{
		`response.created`, `response.output_item.added`, `response.output_text.delta`,
		`response.function_call_arguments.delta`, `"model":"m1"`, `"status":"completed"`,
		`"total_tokens":5`,
	} {
		if !strings.Contains(a, want) {
			t.Errorf("synthesized stream missing %s", want)
		}
	}
	if strings.Count(a, "data: ") != 7 {
		t.Errorf("event count = %d, want 7 (created, 2 added, text delta, 2 args deltas, completed)",
			strings.Count(a, "data: "))
	}
}
