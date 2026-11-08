//go:build live

package tests_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestLiveCatalogContextLength checks that metadata from the dynamic
// OpenCode Go /models catalog (nested limit.context) reaches the CPA model
// list instead of being dropped as an unknown field.
func TestLiveCatalogContextLength(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, cpaHost+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+cpaKey)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
	}
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	want := modelID("glm-5.2")
	for _, m := range list.Data {
		if m.ID == want {
			if m.ContextLength <= 0 {
				t.Fatalf("%s has no context_length from catalog metadata", want)
			}
			return
		}
	}
	t.Fatalf("%s not listed", want)
}
