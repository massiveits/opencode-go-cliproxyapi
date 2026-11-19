package plugin

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRegisterPublishesNativeNestedMetadata(t *testing.T) {
	const body = `{"data":[{"id":"glm-5.3","limit":{"context":204800,"output":131072},` +
		`"modalities":{"input":["text","image"],"output":["text"]},"capabilities":{"reasoning":true}}]}`
	m, _ := newTestManager(catalogResponder(true, body))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 {
		t.Fatalf("static = %+v", static)
	}
	got := static.Models[0]
	if got.ID != "opencode-go/glm-5.3" || got.ContextLength != 204800 || got.MaxCompletionTokens != 131072 ||
		!reflect.DeepEqual(got.SupportedInputModalities, []string{"text", "image"}) ||
		!reflect.DeepEqual(got.SupportedOutputModalities, []string{"text"}) {
		t.Fatalf("model info = %+v", got)
	}
	if got.Thinking != nil {
		t.Fatalf("capabilities.reasoning must not synthesize thinking levels: %+v", got.Thinking)
	}
}
