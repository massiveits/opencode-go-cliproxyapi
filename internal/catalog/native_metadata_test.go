package catalog

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// nativeCatalog is a sanitized sample of the current OpenCode Go /models
// shape: nested limit/modalities plus metadata the plugin does not act on
// (capabilities, cost, provider api fields).
const nativeCatalog = `{"data":[
{"id":"glm-5.3","name":"GLM-5.3","limit":{"context":204800,"output":131072},
 "modalities":{"input":["text","image"],"output":["text"]},
 "capabilities":{"reasoning":true,"tool_call":true},"cost":{"input":1,"output":3},
 "api":{"url":"https://example.invalid/v1","npm":"@ai-sdk/openai-compatible"}},
{"id":"kimi-k3","context_length":131072,"max_output_tokens":8192,"input_modes":["text"],"output_modes":["text"]},
{"id":"qwen3.7-max","limit":{"context":262144,"output":65536},"context_length":1,"max_output_tokens":2,
 "modalities":{"input":["text"]},"input_modes":["audio"],"output_modalities":["text"]},
{"id":"deepseek-v4","limit":{"context":-5,"output":1.5},"context_length":64000,"max_output_tokens":4096},
{"id":"minimax-m3","limit":"oops","modalities":{"input":[1]},"context_length":1000,"input_modalities":["text"]}
]}`

func nativeManager(t *testing.T, body string) *Manager {
	t.Helper()
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)
	return m
}

func TestNativeNestedMetadata(t *testing.T) {
	m := nativeManager(t, nativeCatalog)
	models := m.Models()
	if len(models) != 5 {
		t.Fatalf("every entry must stay routable, got %d: %+v (unsupported %+v)", len(models), models, m.Unsupported())
	}
	for _, tc := range []struct {
		id                   string
		context, output      int64
		inputModes, outModes []string
	}{
		// Current native shape.
		{"glm-5.3", 204800, 131072, []string{"text", "image"}, []string{"text"}},
		// Legacy flat shape remains supported.
		{"kimi-k3", 131072, 8192, []string{"text"}, []string{"text"}},
		// Valid nested values win over flat ones; a missing nested list
		// falls back to the flat field.
		{"qwen3.7-max", 262144, 65536, []string{"text"}, []string{"text"}},
		// Non-positive or fractional nested limits fall back to flat.
		{"deepseek-v4", 64000, 4096, nil, nil},
		// Malformed nested objects are ignored, not fatal.
		{"minimax-m3", 1000, 0, []string{"text"}, nil},
	} {
		got := findModel(t, models, tc.id)
		if got.ContextLimit != tc.context || got.OutputLimit != tc.output ||
			!reflect.DeepEqual(got.InputModes, tc.inputModes) || !reflect.DeepEqual(got.OutputModes, tc.outModes) {
			t.Errorf("%s: got context=%d output=%d in=%v out=%v", tc.id, got.ContextLimit, got.OutputLimit, got.InputModes, got.OutputModes)
		}
	}

	glm := findModel(t, models, "glm-5.3")
	if glm.Thinking != nil {
		t.Errorf("capabilities.reasoning must not synthesize thinking support: %+v", glm.Thinking)
	}
	if glm.EndpointPath != "/v1/chat/completions" || glm.Protocol != RouteChatCompletions {
		t.Errorf("native api metadata must not affect routing: %+v", glm)
	}

	warns := strings.Join(m.Warnings(), "\n")
	for _, want := range []string{
		`model "deepseek-v4": ignoring invalid limit.context`,
		`model "deepseek-v4": ignoring invalid limit.output`,
		`model "minimax-m3": ignoring malformed limit metadata`,
		`model "minimax-m3": ignoring malformed modalities metadata`,
	} {
		if !strings.Contains(warns, want) {
			t.Errorf("missing diagnostic %q in:\n%s", want, warns)
		}
	}
	if strings.Contains(warns, "glm-5.3") || strings.Contains(warns, "kimi-k3") || strings.Contains(warns, "qwen3.7-max") {
		t.Errorf("valid entries must not warn:\n%s", warns)
	}
}

func TestNativeMetadataSurvivesSeedAndStaleRefresh(t *testing.T) {
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(nativeCatalog)}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)

	seeded := newManager(testCfg(), fc)
	seeded.SeedFrom(m)
	if got := findModel(t, seeded.Models(), "glm-5.3"); got.ContextLimit != 204800 || len(got.InputModes) != 2 {
		t.Fatalf("seeded snapshot lost native metadata: %+v", got)
	}

	fc.set(pluginapi.HTTPResponse{StatusCode: 503}, nil)
	if err := m.Refresh(context.Background(), testKey); err == nil {
		t.Fatal("expected refresh failure")
	}
	if got := findModel(t, m.Models(), "glm-5.3"); got.OutputLimit != 131072 {
		t.Fatalf("stale snapshot lost native metadata: %+v", got)
	}
}
