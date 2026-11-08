package chatcompletions

import (
	"encoding/json"
	"reflect"
	"testing"
)

func replayReasoning(t *testing.T, output []any) []map[string]any {
	t.Helper()
	input := append([]any{map[string]any{"type": "message", "role": "user", "content": "Use the test tool."}}, output...)
	input = append(input, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "42"})
	body, _ := json.Marshal(map[string]any{"input": input})
	got, e := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	if e != nil {
		t.Fatal(e)
	}
	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatal(err)
	}
	return req.Messages
}

func TestReasoningToolRoundTrip(t *testing.T) {
	const response = `{"id":"chat1","model":"deepseek-v4.1-flash","choices":[{"message":{"reasoning_content":"Inspect the test value.","content":"Reading it.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_value","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	raw, e := ConvertNonStreamResponse("openai-response", 200, []byte(response))
	if e != nil {
		t.Fatal(e)
	}
	var converted struct {
		Output []any `json:"output"`
	}
	json.Unmarshal(raw, &converted)
	messages := replayReasoning(t, converted.Output)
	if len(messages) != 3 || messages[1]["reasoning_content"] != "Inspect the test value." || messages[1]["content"] != "Reading it." || len(messages[1]["tool_calls"].([]any)) != 1 {
		t.Fatalf("reasoning/tool turn lost: %#v", messages)
	}

	sc := NewStreamConverter("openai-response")
	events := feedAll(t, sc,
		`data: {"id":"chat1","model":"deepseek-v4.1-flash","choices":[{"delta":{"reasoning_content":"Inspect "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"the test value."}}]}`,
		`data: {"choices":[{"delta":{"content":"Reading it."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_value","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	var done []any
	var terminal []any
	var indexes []int
	for _, event := range events {
		if event.Name == "response.output_item.done" {
			done = append(done, event.Data["item"])
			indexes = append(indexes, int(event.Data["output_index"].(float64)))
		}
		if event.Name == "response.completed" {
			terminal = event.Data["response"].(map[string]any)["output"].([]any)
		}
	}
	if last := events[len(events)-1]; last.Name != "response.completed" {
		t.Fatalf("output_item.done events must precede response.completed, last event %q", last.Name)
	}
	if !reflect.DeepEqual(indexes, []int{0, 1, 2}) || !reflect.DeepEqual(done, terminal) || !reflect.DeepEqual(done, converted.Output) {
		t.Fatalf("stream/nonstream mismatch: %#v / %#v", done, converted.Output)
	}
	if got := replayReasoning(t, done); !reflect.DeepEqual(got, messages) {
		t.Fatalf("stream replay differs: %#v", got)
	}
}

func TestReasoningDoesNotLeakAcrossUserTurns(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","id":"rs_cpa_chat_old","summary":[{"text":"old thought"}]},{"role":"assistant","content":"old answer"},{"role":"user","content":"new question"},{"type":"function_call","call_id":"new","name":"read","arguments":"{}"}]}`)
	raw, e := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	if e != nil {
		t.Fatal(e)
	}
	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	json.Unmarshal(raw, &req)
	if req.Messages[0]["reasoning_content"] != "old thought" || req.Messages[2]["reasoning_content"] != "" {
		t.Fatalf("wrong reasoning association: %#v", req.Messages)
	}
}

func TestMissingAndForeignReasoningCompatibility(t *testing.T) {
	for _, model := range []string{"deepseek-v4.1-flash", "other-model"} {
		raw, e := BuildRequest(model, "openai-response", []byte(`{"input":[{"type":"reasoning","id":"rs_foreign","summary":[{"text":"only a summary"}]},{"type":"function_call","name":"read","call_id":"1","arguments":"{}"}]}`), nil)
		if e != nil {
			t.Fatal(e)
		}
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		json.Unmarshal(raw, &req)
		value, present := req.Messages[0]["reasoning_content"]
		if model == "other-model" && present {
			t.Fatal("foreign summary replayed as full reasoning")
		}
		if model != "other-model" && (!present || value != "") {
			t.Fatal("older DeepSeek history cannot resume")
		}
	}
}

func TestReasoningConsecutiveToolCycles(t *testing.T) {
	body := []byte(`{"input":[` +
		`{"role":"user","content":"go"},` +
		`{"type":"reasoning","id":"rs_cpa_chat_1","summary":[{"type":"summary_text","text":"first"}]},` +
		`{"type":"function_call","call_id":"a","name":"read","arguments":"{}"},` +
		`{"type":"function_call","call_id":"b","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"a","output":"1"},` +
		`{"type":"function_call_output","call_id":"b","output":"2"},` +
		`{"type":"reasoning","id":"rs_cpa_chat_2","summary":[{"type":"summary_text","text":"second"}]},` +
		`{"type":"function_call","call_id":"c","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c","output":"3"},` +
		`{"role":"assistant","content":"done"}]}`)
	raw, e := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
	if e != nil {
		t.Fatal(e)
	}
	var req struct {
		Messages []struct {
			Role             string  `json:"role"`
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []any   `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	type turn struct {
		role, reasoning string
		calls           int
	}
	var got []turn
	for _, m := range req.Messages {
		r := "<absent>"
		if m.ReasoningContent != nil {
			r = *m.ReasoningContent
		}
		got = append(got, turn{m.Role, r, len(m.ToolCalls)})
	}
	want := []turn{
		{"user", "<absent>", 0},
		{"assistant", "first", 2},
		{"tool", "<absent>", 0},
		{"tool", "<absent>", 0},
		{"assistant", "second", 1},
		{"tool", "<absent>", 0},
		// The final answer has no reasoning of its own; DeepSeek still
		// receives the empty compatibility field.
		{"assistant", "", 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reasoning association across cycles:\n got %#v\nwant %#v", got, want)
	}
}

func TestClaudeHistoryDeepSeekReasoningFallback(t *testing.T) {
	body := []byte(`{"max_tokens":64,"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`)
	for model, want := range map[string]bool{"deepseek-v4.1-flash": true, "glm-5.2": false} {
		raw, e := BuildRequest(model, "claude", body, nil)
		if e != nil {
			t.Fatal(e)
		}
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		value, present := req.Messages[1]["reasoning_content"]
		if present != want || (want && value != "") {
			t.Fatalf("%s: reasoning_content present=%v value=%v: %s", model, present, value, raw)
		}
	}
}
