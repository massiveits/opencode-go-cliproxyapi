package plugin

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestResolveOpenCodeSessionIDPrecedence(t *testing.T) {
	const fallbackBody = `{"messages":[{"role":"user","content":"fallback"}]}`
	tests := []struct {
		name     string
		metadata map[string]any
		headers  http.Header
		want     string
	}{
		{name: "canonical wins", metadata: map[string]any{"canonical_session_id": "canonical"}, headers: http.Header{"X-Session-Affinity": []string{"header"}}, want: "canonical"},
		{name: "headers use documented order", headers: http.Header{"X-Session-Affinity": []string{"affinity"}, "X-Opencode-Session": []string{"opencode"}}, want: "affinity"},
		{name: "empty earlier header is skipped", headers: http.Header{"X-Session-Affinity": []string{""}, "X-Opencode-Session": []string{"opencode"}}, want: "opencode"},
		{name: "opaque values are preserved", headers: http.Header{"X-Session-Id": []string{"  opaque\tvalue  "}}, want: "  opaque\tvalue  "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Metadata: tc.metadata, Headers: tc.headers, SourceFormat: "openai", OriginalRequest: []byte(fallbackBody)}}
			got, eErr := resolveOpenCodeSessionID(req)
			if eErr != nil || got != tc.want {
				t.Fatalf("session = %q, error = %v, want %q", got, eErr, tc.want)
			}
		})
	}

	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{SourceFormat: "openai", OriginalRequest: []byte(fallbackBody)}}
	got, eErr := resolveOpenCodeSessionID(req)
	want, wantErr := deriveOpenCodeSessionID(req.SourceFormat, req.OriginalRequest)
	if eErr != wantErr || got != want {
		t.Fatalf("fallback session = %q, error = %v, want %q, error %v", got, eErr, want, wantErr)
	}
}

func TestDeriveOpenCodeSessionID(t *testing.T) {
	tests := []struct {
		name   string
		format string
		body   string
		want   string
	}{
		{
			name:   "chat initial turn and exact parts",
			format: "openai",
			body:   `{"messages":[{"role":"system","content":"ignore"},{"role":"developer","content":"ignore"},{"role":"user","content":[{"type":"text","text":"a\n"},{"type":"image_url","image_url":{"url":"https://img"}},{"type":"text","text":" b"}]},{"role":"user","content":"c"},{"role":"assistant","content":"later"},{"role":"user","content":"excluded"}]}`,
			want:   "05b5999d6f492e9fc6946f75b49755f518a71cf3919d3f3c69cc08b4b0786a55",
		},
		{
			name:   "claude initial turn with images and tool result",
			format: "claude",
			body:   `{"system":"ignore","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image","source":{"type":"url","url":"https://img"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}},{"type":"tool_result","content":[{"type":"text","text":"result"}]}]},{"role":"user","content":"b"},{"role":"assistant","content":"stop"},{"role":"user","content":"excluded"}]}`,
			want:   "868cbd9f629dce9789bc87cae81da70763b77662deec6bf682ca2ebd2cd129f1",
		},
		{
			name:   "responses plain string",
			format: "openai-response",
			body:   `{"instructions":"ignore","input":"hello"}`,
			want:   "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		},
		{
			name:   "responses initial user items",
			format: "openai-response",
			body:   `{"input":[{"type":"function_call","id":"mutable","status":"incomplete"},{"type":"message","role":"user","id":"one","content":[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"https://img"}]},{"type":"message","role":"user","id":"two","content":"b"},{"type":"function_call_output","call_id":"c","output":"later"},{"type":"message","role":"user","content":"excluded"}]}`,
			want:   "79b59876910fd6fd04746de91d3a437053246e6df7e9d7bc58cae17942bd18bd",
		},
		{
			name:   "valid no-user fallback",
			format: "openai",
			body:   `{"messages":[{"role":"assistant","content":"prefill"}]}`,
			want:   emptyOpenCodeSessionID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, eErr := deriveOpenCodeSessionID(tc.format, []byte(tc.body))
			if eErr != nil || got != tc.want {
				t.Fatalf("session = %q, error = %v, want %q", got, eErr, tc.want)
			}
		})
	}
}

func TestDeriveOpenCodeSessionIDCrossFormatParityAndErrors(t *testing.T) {
	bodies := map[string]string{
		"openai":          `{"messages":[{"role":"user","content":"same"}]}`,
		"claude":          `{"messages":[{"role":"user","content":"same"}]}`,
		"openai-response": `{"input":"same"}`,
	}
	want := "0967115f2813a3541eaef77de9d9d5773f1c0c04314b0bbfe4ff3b3b1c55b5d5"
	for format, body := range bodies {
		got, eErr := deriveOpenCodeSessionID(format, []byte(body))
		if eErr != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", format, got, eErr, want)
		}
	}
	if _, eErr := deriveOpenCodeSessionID("openai", []byte(`{"messages":[{"role":"user","content":[{"type":"audio"}]}]}`)); eErr == nil {
		t.Fatal("unsupported content must fail derivation")
	}
	if _, eErr := deriveOpenCodeSessionID("openai", []byte(`{"messages":`)); eErr == nil {
		t.Fatal("malformed content must fail derivation")
	}
}

func TestDeriveOpenCodeSessionIDInvariants(t *testing.T) {
	tests := []struct {
		name   string
		format string
		body   string
		want   string
	}{
		{
			name:   "whitespace and line endings are exact",
			format: "openai",
			body:   `{"messages":[{"role":"user","content":" a\r\n b "}]}`,
			want:   "17ef921eb5b4f584d115c898da58d7af2b47203f16d092b5038ea97bd153f1bd",
		},
		{
			name:   "anthropic no-user fallback",
			format: "claude",
			body:   `{"messages":[{"role":"assistant","content":"prefill"}]}`,
			want:   emptyOpenCodeSessionID,
		},
		{
			name:   "responses function output only fallback",
			format: "openai-response",
			body:   `{"input":[{"type":"function_call_output","call_id":"c","output":"done"}]}`,
			want:   emptyOpenCodeSessionID,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, eErr := deriveOpenCodeSessionID(tc.format, []byte(tc.body))
			if eErr != nil || got != tc.want {
				t.Fatalf("session = %q, error = %v, want %q", got, eErr, tc.want)
			}
		})
	}

	base := []byte(`{"messages":[{"role":"user","content":"same"},{"role":"assistant","content":"later"}]}`)
	withLater := []byte(`{"messages":[{"role":"user","content":"same"},{"role":"assistant","content":"later"},{"role":"user","content":"new"}]}`)
	gotBase, baseErr := deriveOpenCodeSessionID("openai", base)
	gotLater, laterErr := deriveOpenCodeSessionID("openai", withLater)
	if baseErr != nil || laterErr != nil || gotBase != gotLater {
		t.Fatalf("later history changed session: %q/%q, errors %v/%v", gotBase, gotLater, baseErr, laterErr)
	}

	cacheA := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"same","cache_control":{"type":"ephemeral"}}]}]}`)
	cacheB := []byte(`{"messages":[{"role":"user","content":"same"}]}`)
	a, aErr := deriveOpenCodeSessionID("openai", cacheA)
	b, bErr := deriveOpenCodeSessionID("openai", cacheB)
	if aErr != nil || bErr != nil || a != b {
		t.Fatalf("cache metadata changed session: %q/%q, errors %v/%v", a, b, aErr, bErr)
	}

	responsesA := []byte(`{"input":[{"type":"message","role":"user","id":"a","content":"same","status":"in_progress"}]}`)
	responsesB := []byte(`{"input":[{"type":"message","role":"user","id":"b","content":"same","status":"completed"}]}`)
	ra, raErr := deriveOpenCodeSessionID("openai-response", responsesA)
	rb, rbErr := deriveOpenCodeSessionID("openai-response", responsesB)
	if raErr != nil || rbErr != nil || ra != rb {
		t.Fatalf("response metadata changed session: %q/%q, errors %v/%v", ra, rb, raErr, rbErr)
	}

	different, dErr := deriveOpenCodeSessionID("openai", []byte(`{"messages":[{"role":"user","content":"different"}]}`))
	if dErr != nil || different == gotBase {
		t.Fatalf("distinct initial content did not change session: %q/%q, error %v", gotBase, different, dErr)
	}
}
