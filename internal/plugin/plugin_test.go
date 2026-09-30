package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/config"
)

// ---- test doubles -------------------------------------------------------

const (
	testKey         = "sk-test-secret-1"
	testCatalogJSON = `{"data":[{"id":"glm-5.3"}]}`
	testValidYAML   = "api-keys:\n  - value: " + testKey + "\n"
	dummyKey        = "sk-test"
	dummyKeyYAML    = "api-keys:\n  - value: " + dummyKey + "\n"
)

type capturedCall struct {
	method  string
	payload []byte
}

type fakeCaller struct {
	mu        sync.Mutex
	calls     []capturedCall
	responder func(method string, payload []byte) ([]byte, error)
	authFiles map[string]string
}

func (f *fakeCaller) call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, capturedCall{method: method, payload: payload})
	f.mu.Unlock()
	var raw []byte
	var err error
	if f.responder != nil {
		raw, err = f.responder(method, payload)
	} else {
		raw = hostOK(map[string]any{})
	}
	if method == pluginabi.MethodHostAuthList && err == nil && !hasExplicitAuthList(raw) {
		return f.authListResponse(), nil
	}
	if method == pluginabi.MethodHostAuthSave && err == nil && hostEnvelopeOK(raw) {
		var req pluginapi.HostAuthSaveRequest
		if json.Unmarshal(payload, &req) == nil && strings.TrimSpace(req.Name) != "" {
			f.mu.Lock()
			if f.authFiles == nil {
				f.authFiles = make(map[string]string)
			}
			var record struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(req.JSON, &record)
			f.authFiles[req.Name] = record.ID
			f.mu.Unlock()
		}
	}
	return raw, err
}

func hostEnvelopeOK(raw []byte) bool {
	var env pluginabi.Envelope
	return json.Unmarshal(raw, &env) == nil && env.OK
}

func hasExplicitAuthList(raw []byte) bool {
	var env pluginabi.Envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		return true
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(env.Result, &result) != nil {
		return true
	}
	_, ok := result["files"]
	return ok
}

func (f *fakeCaller) authListResponse() []byte {
	f.mu.Lock()
	files := make([]pluginapi.HostAuthFileEntry, 0, len(f.authFiles))
	for name, id := range f.authFiles {
		files = append(files, pluginapi.HostAuthFileEntry{
			ID: id, Name: name, Source: "file", Path: name,
		})
	}
	f.mu.Unlock()
	return hostOK(hostAuthListResponse{Files: files})
}

func (f *fakeCaller) recorded() []capturedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeCaller) callsOf(method string) []capturedCall {
	var out []capturedCall
	for _, c := range f.recorded() {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func hostOK(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func hostErr(code, msg string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: msg}})
	return out
}

// catalogResponder answers host.http.do with a canned upstream response.
func catalogResponder(success bool, body string) func(string, []byte) ([]byte, error) {
	return func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		if !success {
			return hostErr("upstream_down", "simulated upstream failure"), nil
		}
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(body),
		}), nil
	}
}

func newTestManager(responder func(string, []byte) ([]byte, error)) (*Manager, *fakeCaller) {
	f := &fakeCaller{responder: responder}
	return NewManager(NewHostBridge(f.call)), f
}

func lifecycleRequestBody(yamlText string) []byte {
	b, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(yamlText)})
	return b
}

func decodeEnv(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	return env
}

func decodeResult(t *testing.T, raw []byte, v any) {
	t.Helper()
	env := decodeEnv(t, raw)
	if !env.OK || env.Error != nil {
		t.Fatalf("expected OK envelope, got %s", raw)
	}
	if err := json.Unmarshal(env.Result, v); err != nil {
		t.Fatalf("decode result: %v (%s)", err, env.Result)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// installFastLoop rebuilds lifecycle state around a 5ms ticker. Validated
// configs floor catalog.refresh-interval at 1m, so tick-driven tests cannot
// obtain an observable interval through register/reconfigure; they install
// the production loop (startRefreshLoop) directly instead.
func installFastLoop(t *testing.T, m *Manager, yamlText string) {
	t.Helper()
	cfg, err := config.Load([]byte(yamlText))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
	mgr := catalog.New(cfg, m.bridge)
	m.mu.Lock()
	m.cfg, m.mgr = cfg, mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	m.mu.Unlock()
	m.startRefreshLoop(cfg, mgr, 5*time.Millisecond, stop, done)
}

// manualTick runs one tick body (refreshOnce) over the currently served
// state — exactly the work a background tick performs — without waiting out
// the 1m config floor on refresh-interval.
func manualTick(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.RLock()
	mgr, cfg := m.mgr, m.cfg
	m.mu.RUnlock()
	if mgr == nil {
		t.Fatal("no served manager to tick")
	}
	if err := refreshOnce(context.Background(), mgr, m.bridge, time.Second, cfg); err != nil {
		t.Fatalf("tick refresh: %v", err)
	}
}

// ---- HostBridge ---------------------------------------------------------

func TestBridgeDoRoundTrip(t *testing.T) {
	var got map[string]any
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Errorf("method = %q, want host.http.do", method)
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("payload not json: %v", err)
		}
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusCreated,
			Headers:    http.Header{"X-Lane": []string{"fast"}},
			Body:       []byte("payload-bytes"),
		}), nil
	}}
	bridge := NewHostBridge(f.call)
	req := pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     "https://upstream.test/v1/models",
		Headers: http.Header{"Authorization": []string{"Bearer " + testKey}},
		Body:    []byte(`{"a":1}`),
	}
	resp, err := bridge.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusCreated || string(resp.Body) != "payload-bytes" ||
		!reflect.DeepEqual(resp.Headers.Get("X-Lane"), "fast") {
		t.Fatalf("decoded response wrong: %+v", resp)
	}
	if got["method"] != http.MethodGet || got["url"] != "https://upstream.test/v1/models" {
		t.Fatalf("wire payload wrong: %v", got)
	}
	headers, _ := got["headers"].(map[string]any)
	if headers["Authorization"].([]any)[0].(string) != "Bearer "+testKey {
		t.Fatalf("Authorization header missing: %v", headers)
	}
	if got["body"] != "eyJhIjoxfQ==" { // std base64 padding of {"a":1}
		t.Fatalf("body encoding wrong: %v", got["body"])
	}
}

func TestBridgeDoFailures(t *testing.T) {
	cases := []struct {
		name     string
		respond  func(string, []byte) ([]byte, error)
		wantSub  string
		zeroResp bool
	}{
		{"caller error", func(string, []byte) ([]byte, error) { return nil, errors.New("boom") }, "boom", true},
		{"undecodable envelope", func(string, []byte) ([]byte, error) { return []byte("not-json"), nil }, "undecodable host response", true},
		{"envelope error with message", func(string, []byte) ([]byte, error) { return hostErr("denied", "nope"), nil }, "nope", true},
		{"envelope error without object", func(string, []byte) ([]byte, error) { return []byte(`{"ok":false}`), nil }, "host http do failed:", true},
		{"malformed result", func(string, []byte) ([]byte, error) { return []byte(`{"ok":true,"result":"{bad"}`), nil }, "undecodable response body", true},
		{"empty result decodes zero response", func(string, []byte) ([]byte, error) { return []byte(`{"ok":true}`), nil }, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge := NewHostBridge((&fakeCaller{responder: tc.respond}).call)
			resp, err := bridge.Do(context.Background(), pluginapi.HTTPRequest{})
			if tc.zeroResp {
				if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
				}
				if resp.StatusCode != 0 || resp.Body != nil {
					t.Fatalf("expected zero response, got %+v", resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBridgeLogSuccess(t *testing.T) {
	var got map[string]any
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostLog {
			t.Errorf("method = %q, want host.log", method)
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("payload not json: %v", err)
		}
		return hostOK(map[string]any{}), nil
	}}
	err := NewHostBridge(f.call).Log("warn", "something happened", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if got["level"] != "warn" || got["message"] != "something happened" {
		t.Fatalf("log payload wrong: %v", got)
	}
	fields, _ := got["fields"].(map[string]any)
	if fields["k"] != "v" {
		t.Fatalf("fields wrong: %v", fields)
	}
}

func TestBridgeLogFailures(t *testing.T) {
	cases := []struct {
		name    string
		respond func(string, []byte) ([]byte, error)
		wantSub string
	}{
		{"caller error", func(string, []byte) ([]byte, error) { return nil, errors.New("log boom") }, "log boom"},
		{"undecodable envelope", func(string, []byte) ([]byte, error) { return []byte("{"), nil }, "undecodable host response"},
		{"envelope error with message", func(string, []byte) ([]byte, error) { return hostErr("busy", "later"), nil }, "later"},
		{"envelope error without object", func(string, []byte) ([]byte, error) { return []byte(`{"ok":false}`), nil }, "host log failed:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewHostBridge((&fakeCaller{responder: tc.respond}).call).Log("warn", "m", nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestBridgeAuthSaveWireAndRedactsFailures(t *testing.T) {
	secret := "sk-auth-save-secret"
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthSave {
			t.Fatalf("method = %q, want %q", method, pluginabi.MethodHostAuthSave)
		}
		var wire pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(payload, &wire); err != nil {
			t.Fatalf("payload not json: %v", err)
		}
		if wire.Name != "opencode-go-key-test.json" || string(wire.JSON) != `{"type":"opencode-go","api_key":"`+secret+`"}` {
			t.Fatalf("wire request = %+v", wire)
		}
		return hostOK(pluginapi.HostAuthSaveResponse{Name: wire.Name}), nil
	}}
	if err := NewHostBridge(f.call).AuthSave(context.Background(), pluginapi.HostAuthSaveRequest{
		Name: "opencode-go-key-test.json",
		JSON: json.RawMessage(`{"type":"opencode-go","api_key":"` + secret + `"}`),
	}); err != nil {
		t.Fatalf("AuthSave: %v", err)
	}

	secretErr := "host rejected " + secret
	err := NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return hostErr("denied", secretErr), nil
	}}).call).AuthSave(context.Background(), pluginapi.HostAuthSaveRequest{Name: "x.json", JSON: []byte(`{}`)})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "host auth save failed") {
		t.Fatalf("redacted auth error = %v", err)
	}
	err = NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return []byte("not-json"), nil
	}}).call).AuthSave(context.Background(), pluginapi.HostAuthSaveRequest{Name: "x.json", JSON: []byte(`{}`)})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "undecodable host response") {
		t.Fatalf("malformed auth response = %v", err)
	}
}

func TestBridgeAuthListDecodesEntries(t *testing.T) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthList {
			t.Fatalf("method = %q, want %q", method, pluginabi.MethodHostAuthList)
		}
		var req map[string]any
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Fatalf("list payload not json: %v", err)
		}
		return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
			ID: "opencode-go-key-existing", Name: "opencode-go-key-existing.json", Priority: 7,
		}}}), nil
	}}
	entries, err := NewHostBridge(f.call).AuthList(context.Background())
	if err != nil {
		t.Fatalf("AuthList: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "opencode-go-key-existing" || entries[0].Name != "opencode-go-key-existing.json" || entries[0].Priority != 7 {
		t.Fatalf("auth entries = %+v", entries)
	}
}

// ---- dispatcher: registration ------------------------------------------

func TestRegisterSuccessPublishesModels(t *testing.T) {
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registrationResult
	decodeResult(t, resp, &reg)
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name != "opencode-go-cliproxyapi" || reg.Metadata.Version != pluginVersion ||
		len(reg.Metadata.ConfigFields) != 10 {
		t.Fatalf("metadata wrong: %+v", reg.Metadata)
	}
	if !reg.Capabilities.ModelProvider || !reg.Capabilities.AuthProvider {
		t.Fatalf("capabilities wrong: %+v", reg.Capabilities)
	}

	calls := f.callsOf(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 {
		t.Fatalf("host.http.do calls = %d, want 1", len(calls))
	}
	var wire map[string]any
	if err := json.Unmarshal(calls[0].payload, &wire); err != nil {
		t.Fatalf("bridge payload: %v", err)
	}
	if wire["method"] != http.MethodGet {
		t.Fatalf("bridged method = %v, want GET", wire["method"])
	}
	if wire["url"] != "https://opencode.ai/zen/go/v1/models" {
		t.Fatalf("bridged url = %v", wire["url"])
	}
	headers := wire["headers"].(map[string]any)
	auth := headers["Authorization"].([]any)[0].(string)
	if auth != "Bearer "+testKey {
		t.Fatalf("bridged Authorization = %q", auth)
	}

	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if static.Provider != ProviderID || len(static.Models) != 1 {
		t.Fatalf("static = %+v", static)
	}
	got := static.Models[0]
	if got.ID != "opencode-go/glm-5.3" || got.Object != "model" || got.OwnedBy != ProviderID ||
		got.DisplayName != "glm-5.3" || got.ContextLength != 0 || got.MaxCompletionTokens != 0 ||
		len(got.SupportedInputModalities) != 0 || len(got.SupportedOutputModalities) != 0 {
		t.Fatalf("model info = %+v", got)
	}

	var forAuth pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.for_auth", []byte("{}")), &forAuth)
	if forAuth.Provider != ProviderID || len(forAuth.Models) != 1 || forAuth.Models[0].ID != got.ID {
		t.Fatalf("for_auth = %+v", forAuth)
	}
}

func TestRegistrationConfigFields(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registrationResult
	decodeResult(t, resp, &reg)

	expectedFields := []struct {
		name string
		typ  pluginapi.ConfigFieldType
	}{
		{"api-keys", pluginapi.ConfigFieldTypeArray},
		{"base-url", pluginapi.ConfigFieldTypeString},
		{"catalog-url", pluginapi.ConfigFieldTypeString},
		{"model-prefix", pluginapi.ConfigFieldTypeObject},
		{"catalog", pluginapi.ConfigFieldTypeObject},
		{"protocols", pluginapi.ConfigFieldTypeObject},
		{"route-overrides", pluginapi.ConfigFieldTypeObject},
		{"request-timeout", pluginapi.ConfigFieldTypeString},
		{"max-response-bytes", pluginapi.ConfigFieldTypeInteger},
		{"allow-http", pluginapi.ConfigFieldTypeBoolean},
	}

	if len(reg.Metadata.ConfigFields) != len(expectedFields) {
		t.Fatalf("len(ConfigFields) = %d, want %d", len(reg.Metadata.ConfigFields), len(expectedFields))
	}

	for i, want := range expectedFields {
		got := reg.Metadata.ConfigFields[i]
		if got.Name != want.name {
			t.Errorf("field[%d].Name = %q, want %q", i, got.Name, want.name)
		}
		if got.Type != want.typ {
			t.Errorf("field[%d].Type = %q, want %q", i, got.Type, want.typ)
		}
		if got.Description == "" {
			t.Errorf("field[%d].Description is empty", i)
		}
	}
}

func TestLifecycleMaterializesDeterministicAuthRecords(t *testing.T) {
	first, second, third := "sk-materialize-a", "sk-materialize-b", "sk-materialize-c"
	yamlText := "api-keys:\n  - value: " + first + "\n  - value: " + second + "\n"
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody("api-keys:\n  - value: "+second+"\n  - value: "+first+"\n")); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	calls := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(calls) != 2 {
		t.Fatalf("unchanged reconfigure auth saves = %d, want 2", len(calls))
	}
	seen := map[string]bool{}
	for _, call := range calls {
		var wire pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(call.payload, &wire); err != nil {
			t.Fatalf("auth payload: %v", err)
		}
		var record struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Label  string `json:"label"`
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(wire.JSON, &record); err != nil {
			t.Fatalf("auth record: %v", err)
		}
		hash := sha256.Sum256([]byte(record.APIKey))
		wantHash := hex.EncodeToString(hash[:])
		if record.Type != ProviderID || record.ID != "opencode-go-key-"+wantHash || !strings.HasPrefix(record.Label, "OpenCode Go ") || !strings.HasPrefix(wire.Name, "OpenCode-Go-") {
			t.Fatalf("record identity = %+v name=%q", record, wire.Name)
		}
		if record.APIKey == "" || strings.Contains(wire.Name, record.APIKey) || strings.Contains(record.ID, record.APIKey) {
			t.Fatalf("secret leaked in identity: %+v name=%q", record, wire.Name)
		}
		seen[record.APIKey] = true
	}
	if len(seen) != 2 {
		t.Fatalf("materialized keys = %v", seen)
	}
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody("api-keys:\n  - value: "+first+"\n")); err != nil {
		t.Fatalf("removed-key reconfigure: %v", err)
	}
	// No delete callback exists in the pinned ABI: the old record is stale and
	// remains in CPA rather than being falsely reported as removed.
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 2 {
		t.Fatalf("removed-key reconfigure changed save count = %d, want 2", len(f.callsOf(pluginabi.MethodHostAuthSave)))
	}
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody("api-keys:\n  - value: "+first+"\n  - value: "+third+"\n")); err != nil {
		t.Fatalf("new-key reconfigure: %v", err)
	}
	calls = f.callsOf(pluginabi.MethodHostAuthSave)
	if len(calls) != 3 {
		t.Fatalf("new-key reconfigure auth saves = %d, want 3", len(calls))
	}
	var newRecord struct {
		APIKey string `json:"api_key"`
	}
	var wire pluginapi.HostAuthSaveRequest
	if err := json.Unmarshal(calls[2].payload, &wire); err != nil {
		t.Fatalf("new auth wire: %v", err)
	}
	if err := json.Unmarshal(wire.JSON, &newRecord); err != nil || newRecord.APIKey != third {
		t.Fatalf("new auth record = %+v, want key %q", newRecord, third)
	}
}

func TestLifecycleUsesCPAAuthListAfterManagerRestart(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	first := NewManager(NewHostBridge(f.call))
	if _, err := first.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := first.HandleCall("plugin.shutdown", nil); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}

	second := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = second.HandleCall("plugin.shutdown", nil) })
	if _, err := second.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("second register: %v", err)
	}
	if got := len(f.callsOf(pluginabi.MethodHostAuthSave)); got != 1 {
		t.Fatalf("auth saves after manager restart = %d, want 1", got)
	}
}

func TestLifecycleAuthListFailureDoesNotWrite(t *testing.T) {
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostErr("auth_unavailable", "auth directory unavailable"), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != "auth_materialization_failed" {
		t.Fatalf("list failure envelope = %+v", env.Error)
	}
	if got := len(f.callsOf(pluginabi.MethodHostAuthSave)); got != 0 {
		t.Fatalf("auth saves after list failure = %d, want 0", got)
	}
	if got := len(f.callsOf(pluginabi.MethodHostHTTPDo)); got != 0 {
		t.Fatalf("catalog calls after list failure = %d, want 0", got)
	}
}

// TestRegisterMetadataCarriesGitHubRepository pins the host validity gate
// (pinned SDK host.go validPlugin): an empty GitHubRepository makes the real
// host drop the plugin on every register/reconfigure, so the field must be
// non-empty AND actually marshal into the envelope bytes.
func TestRegisterMetadataCarriesGitHubRepository(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registrationResult
	decodeResult(t, resp, &reg)
	if reg.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata.GitHubRepository empty: host validPlugin would drop the plugin: %+v", reg.Metadata)
	}
	// Metadata structs have no json tags; assert the Go field name marshals.
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v", err)
	}
	if !bytes.Contains(env.Result, []byte(`"GitHubRepository":`)) ||
		bytes.Contains(env.Result, []byte(`"GitHubRepository":""`)) {
		t.Fatalf("envelope metadata lacks non-empty GitHubRepository: %s", env.Result)
	}
}

func TestRegisterIgnoresInjectedHostKeys(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	yamlText := testValidYAML + "enabled: true\npriority: 42\n"
	resp, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(yamlText))
	if err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("host-injected keys rejected: %s", resp)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 || static.Models[0].ID != "opencode-go/glm-5.3" {
		t.Fatalf("models = %+v", static.Models)
	}
}

func TestMalformedLifecycleAndUnknownMethods(t *testing.T) {
	m, _ := newTestManager(nil)
	for name, body := range map[string][]byte{
		"truncated json": []byte("{"),
		"bad base64":     []byte(`{"config_yaml":"!!!not-base64!!!","schema_version":3}`),
	} {
		resp, err := m.HandleCall("plugin.register", body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
			t.Fatalf("%s: envelope = %s", name, resp)
		}
	}
	resp, err := m.HandleCall("totally.bogus", nil)
	if err != nil {
		t.Fatalf("unknown method: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" ||
		!strings.Contains(env.Error.Message, "totally.bogus") {
		t.Fatalf("envelope = %s", resp)
	}
}

func TestReconfigureSwapsPrefixIDs(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	noPrefix := testValidYAML + "model-prefix:\n  enabled: false\n"
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(noPrefix)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 || static.Models[0].ID != "glm-5.3" {
		t.Fatalf("after prefix-off reconfigure: %+v", static.Models)
	}
}

func TestRefreshFailureStillRegistersAndWarnsWithoutSecrets(t *testing.T) {
	m, f := newTestManager(catalogResponder(false, ""))
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("refresh failure must not fail registration (FR-002): %s", resp)
	}
	logCalls := f.callsOf(pluginabi.MethodHostLog)
	if len(logCalls) != 1 {
		t.Fatalf("warn logs = %d, want 1", len(logCalls))
	}
	var entry map[string]any
	if err := json.Unmarshal(logCalls[0].payload, &entry); err != nil {
		t.Fatalf("log payload: %v", err)
	}
	if entry["level"] != "warn" || !strings.Contains(entry["message"].(string), "catalog refresh failed") {
		t.Fatalf("log entry = %v", entry)
	}
	blob := string(logCalls[0].payload)
	if strings.Contains(blob, testKey) || strings.Contains(blob, "Bearer") {
		t.Fatalf("log leaks credential material: %s", blob)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 0 {
		t.Fatalf("failed refresh must yield empty routable set: %+v", static.Models)
	}
}

func TestRegisterRefreshesWithConfiguredBearer(t *testing.T) {
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(dummyKeyYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("register rejected: %s", resp)
	}
	calls := f.callsOf(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 {
		t.Fatalf("host.http.do calls = %d, want 1", len(calls))
	}
	var wire map[string]any
	if err := json.Unmarshal(calls[0].payload, &wire); err != nil {
		t.Fatalf("bridge payload: %v", err)
	}
	headers := wire["headers"].(map[string]any)
	if headers["Authorization"].([]any)[0].(string) != "Bearer "+dummyKey {
		t.Fatalf("expected configured bearer key, got %v", headers)
	}
}

// TestRegisterWithBlockingCatalogReturnsQuickly pins the dedicated register
// timeout: the synchronous lifecycle refresh must expire at
// registerRefreshTimeout (10s), never at request-timeout (default 15m), and
// registration must still succeed with an empty routable set (FR-002).
func TestRegisterWithBlockingCatalogReturnsQuickly(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		var wire map[string]any
		if method == pluginabi.MethodHostHTTPDo && json.Unmarshal(payload, &wire) == nil {
			if url, _ := wire["url"].(string); strings.HasSuffix(url, "/models") {
				<-release // block far past the register timeout
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
			}
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	start := time.Now()
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("registration must succeed despite blocked catalog: %s", resp)
	}
	if elapsed < 9*time.Second || elapsed > 13*time.Second {
		t.Fatalf("register elapsed = %v, want ~registerRefreshTimeout (10s), not 15m", elapsed)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 0 {
		t.Fatalf("blocked refresh must yield empty routable set: %+v", static.Models)
	}
}

// ---- dispatcher: models before register, shutdown, panic ---------------

func TestModelsBeforeRegisterIsEmptyNotError(t *testing.T) {
	m, _ := newTestManager(nil)
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if static.Provider != ProviderID || len(static.Models) != 0 {
		t.Fatalf("pre-register static = %+v", static)
	}
}

func TestShutdownClearsStateAndStopsTicker(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	installFastLoop(t, m, testValidYAML)
	waitFor(t, "periodic refresh ticks", func() bool {
		return len(f.callsOf(pluginabi.MethodHostHTTPDo)) >= 3
	})
	resp := mustHandle(t, m, "plugin.shutdown", nil)
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("shutdown envelope = %s", resp)
	}
	m.mu.Lock()
	stopped := m.stop == nil && m.done == nil && m.mgr == nil && m.cfg.BaseURL == ""
	m.mu.Unlock()
	if !stopped {
		t.Fatal("shutdown left state behind")
	}
	before := len(f.callsOf(pluginabi.MethodHostHTTPDo))
	time.Sleep(40 * time.Millisecond)
	if after := len(f.callsOf(pluginabi.MethodHostHTTPDo)); after != before {
		t.Fatalf("ticks continued after shutdown: %d -> %d", before, after)
	}
	resp2 := mustHandle(t, m, "plugin.shutdown", nil)
	if env := decodeEnv(t, resp2); !env.OK {
		t.Fatalf("double shutdown must be idempotent: %s", resp2)
	}
}

// TestShutdownDrainsOrphanedHostCallbacks pins the Unix unload-safety fix:
// handleShutdown waits (bounded) for host-callback goroutines orphaned by
// timeouts, warns when any are still alive at the deadline, and a follow-up
// shutdown once the orphan unwinds reports nothing in flight.
func TestShutdownDrainsOrphanedHostCallbacks(t *testing.T) {
	oldDrain := shutdownDrainTimeout
	shutdownDrainTimeout = 60 * time.Millisecond
	defer func() { shutdownDrainTimeout = oldDrain }()

	release := make(chan struct{})
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostLog:
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostHTTPDo:
			<-release // wedged host callback, no abandon cleanup attached
			return hostOK(map[string]any{}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if _, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{}); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected wedged Do to time out, got %v", err)
	}
	cancel()

	inFlightWarn := func() int {
		n := 0
		for _, c := range f.callsOf(pluginabi.MethodHostLog) {
			if strings.Contains(string(c.payload), "still in flight") {
				n++
			}
		}
		return n
	}

	resp := mustHandle(t, m, "plugin.shutdown", nil)
	if env := decodeEnv(t, resp); !env.OK || string(env.Result) != "{}" {
		t.Fatalf("shutdown envelope = %s", resp)
	}
	if n := inFlightWarn(); n != 1 {
		t.Fatalf("in-flight warns = %d, want exactly the stalled-shutdown warn", n)
	}

	close(release)
	resp2 := mustHandle(t, m, "plugin.shutdown", nil)
	if env := decodeEnv(t, resp2); !env.OK {
		t.Fatalf("second shutdown envelope = %s", resp2)
	}
	if n := inFlightWarn(); n != 1 {
		t.Fatalf("warns after orphan released = %d, want no repeat", n)
	}
}

func TestShutdownDrainTimeoutPolicy(t *testing.T) {
	if shutdownDrainTimeout != 15*time.Second {
		t.Fatalf("shutdown drain timeout = %s, want 15s", shutdownDrainTimeout)
	}
}

// TestRegisterSurvivesNilHostBridge pins F5 degradation: a zero-value
// Manager (nil host bridge, only reachable via direct construction) makes
// the initial refresh fail with the catalog package's classified
// "host client unavailable" error instead of panicking; registration still
// succeeds under FR-002 stale semantics.
func TestRegisterSurvivesNilHostBridge(t *testing.T) {
	m := &Manager{}
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register must not surface as Go error: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("registration must survive unavailable host client: %s", resp)
	}
	var reg registrationResult
	if err := json.Unmarshal(env.Result, &reg); err != nil || reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("degraded registration malformed: %s", resp)
	}
}

// ---- concurrency --------------------------------------------------------

func TestConcurrentHandleCalls(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	methods := []string{"model.static", "model.for_auth", "bogus.method"}
	var wg sync.WaitGroup
	wg.Add(len(methods) + 1)
	for _, method := range methods {
		go func(method string) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := m.HandleCall(method, nil); err != nil {
					t.Errorf("%s: %v", method, err)
					return
				}
			}
		}(method)
	}
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			body := lifecycleRequestBody(testValidYAML)
			if i%2 == 1 {
				body = lifecycleRequestBody(testValidYAML + "model-prefix:\n  enabled: false\n")
			}
			if _, err := m.HandleCall("plugin.reconfigure", body); err != nil {
				t.Errorf("reconfigure: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

func TestOverlappingLifecyclesLeaveSingleTicker(t *testing.T) {
	// F2/F4 regression: concurrent register/reconfigure must serialize the
	// stop-wait-install sequence so exactly one ticker survives, and it must
	// exit on stop (no orphaned loops keep refreshing). Validated configs
	// floor refresh-interval at 1m, so the survivor is checked structurally
	// — one tracked loop that exits on stop — instead of by counting ticks.
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			method := "plugin.register"
			if i == 1 {
				method = "plugin.reconfigure"
			}
			if _, err := m.HandleCall(method, lifecycleRequestBody(testValidYAML)); err != nil {
				t.Errorf("%s: %v", method, err)
			}
		}(i)
	}
	wg.Wait()
	if got := len(f.callsOf(pluginabi.MethodHostAuthSave)); got != 1 {
		t.Fatalf("overlapping lifecycle auth saves = %d, want 1", got)
	}
	done := m.closeStop()
	if done == nil {
		t.Fatal("concurrent lifecycles left no tracked ticker")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("surviving ticker did not exit on stop")
	}
	if again := m.closeStop(); again != nil {
		t.Fatal("more than one ticker was left tracked")
	}
}

func TestReconfigureFailedRefreshServesViaTicks(t *testing.T) {
	// F3 regression: when a reconfigure's synchronous refresh fails, the
	// stale-check keeps the SERVED manager; background ticks must refresh
	// that same manager so recovered upstream data becomes visible.
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	catalogV1 := `{"data":[{"id":"glm-5.3"}]}`
	catalogV2 := `{"data":[{"id":"minimax-m3"}]}`
	var fetches atomic.Int64
	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if !strings.HasSuffix(url, "/models") {
			return hostOK(map[string]any{}), nil
		}
		switch fetches.Add(1) {
		case 1:
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV1)}), nil
		case 2:
			return hostErr("upstream_down", "simulated refresh failure"), nil
		default:
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV2)}), nil
		}
	}

	// Validated configs floor refresh-interval at 1m, so ticks are driven
	// manually via manualTick (the exact body a background tick runs).
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	assertStaticModel(t, m, "opencode-go/glm-5.3")
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	assertStaticModel(t, m, "opencode-go/glm-5.3")
	manualTick(t, m)
	assertStaticModel(t, m, "opencode-go/minimax-m3")
}

// TestShutdownAbortsInFlightTickRefresh pins the F4 fix: a tick's refresh
// context derives from stop, so close(stop) aborts an in-flight Refresh
// immediately and shutdown cannot sit holding lifeMu for up to
// request-timeout behind a hung upstream.
func TestShutdownAbortsInFlightTickRefresh(t *testing.T) {
	// The orphaned tick callback stays parked until cleanup releases it;
	// shrink the unload-safety drain so it cannot dominate this timing
	// assertion (the ctx-abort property under test is orthogonal).
	oldDrain := shutdownDrainTimeout
	shutdownDrainTimeout = 50 * time.Millisecond
	defer func() { shutdownDrainTimeout = oldDrain }()

	var syncServed atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		if err := json.Unmarshal(payload, &wire); err != nil {
			return hostOK(map[string]any{}), nil
		}
		if url, _ := wire["url"].(string); strings.HasSuffix(url, "/models") {
			if !syncServed.CompareAndSwap(false, true) {
				close(started) // a tick is now in flight against a hung upstream
				<-release      // block far past any sane shutdown wait
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
			}
			// Serve the synchronous register refresh instantly.
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Validated configs floor refresh-interval at 1m; install the production
	// loop directly at 5ms so a tick goes in flight immediately.
	installFastLoop(t, m, testValidYAML)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tick refresh never started")
	}

	shutdownStart := time.Now()
	resp := mustHandle(t, m, "plugin.shutdown", nil)
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("shutdown envelope = %s", resp)
	}
	if elapsed := time.Since(shutdownStart); elapsed > 2*time.Second {
		t.Fatalf("shutdown waited %v on the in-flight tick; stop-derived ctx must abort it", elapsed)
	}
}

// TestReconfigureFailedRefreshSeedsCarryoverThenFetchesNewURL pins the F5
// fix: a failed-refresh reconfigure must adopt the NEW manager (seeded with
// the old last-good records) so carried-over models stay visible between the
// failure and the next success, and background ticks fetch the NEW
// catalog-url instead of the old one forever.
func TestReconfigureFailedRefreshSeedsCarryoverThenFetchesNewURL(t *testing.T) {
	oldURL := "https://opencode.ai/zen/go/v1/models"
	newURL := "https://mirror.test/api/models"
	catalogV1 := `{"data":[{"id":"glm-5.3"}]}`
	catalogV2 := `{"data":[{"id":"minimax-m3"}]}`
	var allowOldURL atomic.Bool
	allowOldURL.Store(true)
	var failNewSync atomic.Bool
	failNewSync.Store(true)
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if !strings.HasSuffix(url, "/models") {
			return hostOK(map[string]any{}), nil
		}
		switch url {
		case oldURL:
			// Every /models fetch after the reconfigure returned must target
			// the NEW url; old-loop ticks all complete before it returns.
			if !allowOldURL.Load() {
				t.Error("tick used the OLD catalog-url after reconfigure")
				return hostErr("old_url", url), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV1)}), nil
		case newURL:
			if failNewSync.CompareAndSwap(true, false) {
				return hostErr("upstream_down", "simulated reconfigure refresh failure"), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV2)}), nil
		default:
			t.Errorf("unexpected catalog url %q", url)
			return hostErr("unexpected_url", url), nil
		}
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	assertStaticModel(t, m, "opencode-go/glm-5.3")

	newBaseYAML := testValidYAML + "base-url: https://mirror.test/api\n"
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(newBaseYAML)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	allowOldURL.Store(false)

	// Carried-over last-good model stays visible between the failed
	// reconfigure refresh and the next successful refresh.
	assertStaticModel(t, m, "opencode-go/glm-5.3")

	// Validated configs floor refresh-interval at 1m; run the tick body
	// directly over the served state instead of waiting for the loop.
	manualTick(t, m)
	assertStaticModel(t, m, "opencode-go/minimax-m3")
}

// TestReconfigureFailedRefreshHonorsStalePolicy pins the seed-gate fix: on a
// failed-refresh reconfigure the seed consults the NEW config's
// stale-while-unavailable — fail-closed serves nothing immediately (matching
// Manager.fail's ticker behavior), stale-enabled keeps carrying over.
func TestReconfigureFailedRefreshHonorsStalePolicy(t *testing.T) {
	catalogV1 := `{"data":[{"id":"glm-5.3"}]}`
	catalogV2 := `{"data":[{"id":"minimax-m3"}]}`
	for _, tc := range []struct {
		name          string
		policyYAML    string
		wantImmediate bool // model visible right after the failed reconfigure
	}{
		{name: "stale enabled keeps carryover", policyYAML: "", wantImmediate: true},
		{name: "fail closed empties immediately", policyYAML: "catalog:\n  stale-while-unavailable: false\n", wantImmediate: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fetches atomic.Int64
			f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
				if method != pluginabi.MethodHostHTTPDo {
					return hostOK(map[string]any{}), nil
				}
				var wire map[string]any
				_ = json.Unmarshal(payload, &wire)
				if url, _ := wire["url"].(string); !strings.HasSuffix(url, "/models") {
					return hostOK(map[string]any{}), nil
				}
				switch fetches.Add(1) {
				case 1:
					return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV1)}), nil
				case 2:
					return hostErr("upstream_down", "simulated refresh failure"), nil
				default:
					return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV2)}), nil
				}
			}}
			m := NewManager(NewHostBridge(f.call))
			t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

			if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
				t.Fatalf("register: %v", err)
			}
			assertStaticModel(t, m, "opencode-go/glm-5.3")

			if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(testValidYAML+tc.policyYAML)); err != nil {
				t.Fatalf("reconfigure: %v", err)
			}

			var static pluginapi.ModelResponse
			decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
			got := len(static.Models)
			if tc.wantImmediate && (got != 1 || static.Models[0].ID != "opencode-go/glm-5.3") {
				t.Fatalf("carryover models = %+v, want glm-5.3", static.Models)
			}
			if !tc.wantImmediate && got != 0 {
				t.Fatalf("fail-closed served %d models immediately, want 0", got)
			}

			// Both policies recover via the next successful tick against the
			// NEW manager/config.
			manualTick(t, m)
			assertStaticModel(t, m, "opencode-go/minimax-m3")
		})
	}
}

func staticModelID(t *testing.T, m *Manager) string {
	t.Helper()
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 {
		t.Fatalf("static models = %+v", static.Models)
	}
	return static.Models[0].ID
}

func assertStaticModel(t *testing.T, m *Manager, wantID string) {
	t.Helper()
	if got := staticModelID(t, m); got != wantID {
		t.Fatalf("static model = %q, want %q", got, wantID)
	}
}

func mustHandle(t *testing.T, m *Manager, method string, req []byte) []byte {
	t.Helper()
	resp, err := m.HandleCall(method, req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return resp
}
