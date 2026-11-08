//go:build live

package tests_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestLiveDeepSeekReasoningToolContinuation runs a Responses tool cycle on a
// DeepSeek Chat Completions route: the first turn's output (including the
// reasoning item) is replayed with the tool result. DeepSeek rejects the
// continuation when reasoning_content is missing from the assistant turn.
func TestLiveDeepSeekReasoningToolContinuation(t *testing.T) {
	client := &http.Client{Timeout: 90 * time.Second}
	tools := []map[string]any{{
		"type":        "function",
		"name":        "get_weather",
		"description": "Get current weather for a city",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"location": map[string]string{"type": "string"}},
			"required":   []string{"location"},
		},
	}}
	post := func(input []any) []json.RawMessage {
		t.Helper()
		b, _ := json.Marshal(map[string]any{
			"model":             modelID("deepseek-v4.1-flash"),
			"input":             input,
			"tools":             tools,
			"max_output_tokens": 512,
		})
		req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/responses", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cpaKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
		}
		var result struct {
			Output []json.RawMessage `json:"output"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		return result.Output
	}

	input := []any{map[string]any{"role": "user", "content": "Use get_weather for Tokyo, then answer in one sentence."}}
	output := post(input)
	callID := ""
	for _, item := range output {
		var v struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		json.Unmarshal(item, &v)
		if v.Type == "function_call" {
			callID = v.CallID
		}
		input = append(input, item)
	}
	if callID == "" {
		t.Skip("model answered without calling the tool; continuation not exercised")
	}
	time.Sleep(500 * time.Millisecond)
	input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": "Sunny, 22C"})
	if out := post(input); len(out) == 0 {
		t.Fatal("continuation returned no output")
	}
}
