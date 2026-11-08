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

// TestLiveClaudeOutputConfigEffort sends a Claude Messages request carrying
// output_config.effort to a Chat Completions route and a Responses route.
// The explicit effort is forwarded instead of being replaced by a
// budget-derived level; upstream must accept the translated request.
func TestLiveClaudeOutputConfigEffort(t *testing.T) {
	client := &http.Client{Timeout: 60 * time.Second}
	for _, model := range []string{"deepseek-v4.1-flash", "gpt-5.6-luna"} {
		t.Run(model, func(t *testing.T) {
			time.Sleep(500 * time.Millisecond)
			payload := map[string]any{
				"model":         modelID(model),
				"max_tokens":    64,
				"output_config": map[string]any{"effort": "low"},
				"messages": []map[string]string{
					{"role": "user", "content": "Say hello"},
				},
			}
			b, _ := json.Marshal(payload)
			req, _ := http.NewRequest(http.MethodPost, cpaHost+"/v1/messages", bytes.NewReader(b))
			req.Header.Set("x-api-key", cpaKey)
			req.Header.Set("anthropic-version", "2023-06-01")
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
				Role string `json:"role"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatalf("decode JSON: %v", err)
			}
			if result.Role != "assistant" {
				t.Fatalf("expected assistant role, got %q", result.Role)
			}
		})
	}
}
