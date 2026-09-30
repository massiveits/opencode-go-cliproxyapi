package plugin

import (
	"context"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"net/http"
	"opencode-go-cliproxyapi/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuotaRegistration(t *testing.T) {
	m := NewManager(nil)
	var registration registrationResult
	decodeResult(t, registrationEnvelope(), &registration)
	if !registration.Capabilities.QuotaProvider || registration.Capabilities.ManagementAPI {
		t.Fatal("incorrect quota capabilities")
	}
	var desc pluginapi.QuotaDescribeResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaDescribe, []byte(`{}`)), &desc)
	if len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != ProviderID || desc.SupportsReset {
		t.Fatal("incorrect description")
	}
	for _, method := range []string{pluginabi.MethodManagementRegister, pluginabi.MethodManagementHandle, pluginabi.MethodQuotaReset} {
		var env pluginabi.Envelope
		json.Unmarshal(mustHandle(t, m, method, []byte(`{}`)), &env)
		if env.OK {
			t.Fatalf("obsolete/unsupported operation succeeded: %s", method)
		}
	}
}
func TestQuotaSelectedCredential(t *testing.T) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		var req pluginapi.HTTPRequest
		json.Unmarshal(payload, &req)
		if method != pluginabi.MethodHostHTTPDo || req.Headers.Get("Authorization") != "Bearer selected" || req.URL != "https://quota.test/usage" {
			t.Fatal("wrong credential or URL")
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"usage":{"rolling":{"percent":25,"resetsAt":"2026-10-01T00:00:00Z"},"weekly":{"percent":100},"monthly":{"percent":0}}}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/", APIKeys: []config.APIKey{{Value: "not-selected"}}}
	body, _ := json.Marshal(pluginapi.QuotaFetchRequest{Provider: ProviderID, Attributes: map[string]string{"api_key": "selected"}})
	var got pluginapi.QuotaFetchResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaFetch, body), &got)
	if len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 3 {
		t.Fatal("missing buckets")
	}
	for i, want := range []float64{0.75, 0, 1} {
		if got.Groups[0].Buckets[i].RemainingFraction != want {
			t.Fatal("wrong fraction")
		}
	}
	for _, req := range []pluginapi.QuotaFetchRequest{{Provider: "other", Attributes: map[string]string{"api_key": "selected"}}, {Provider: ProviderID}} {
		if _, err := m.FetchQuota(context.Background(), req); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
}
func TestQuotaValidation(t *testing.T) {
	for _, body := range []string{`{}`, `{"usage":{"rolling":{}}}`, `{"usage":{"rolling":{"percent":null}}}`, `{"usage":{"rolling":{"percent":-1}}}`, `{"usage":{"rolling":{"percent":101}}}`, `{"usage":{"rolling":{"percent":true}}}`, `{"usage":{"rolling":{"percent":1,"resetsAt":"invalid"}}}`} {
		if _, err := normalizeQuota([]byte(body)); err == nil {
			t.Fatalf("invalid response accepted: %s", body)
		}
	}
	got, err := normalizeQuota([]byte(`{"usage":{"weekly":{"percent":12.5}}}`))
	if err != nil || len(got.Groups[0].Buckets) != 1 || got.Groups[0].Buckets[0].RemainingFraction != 0.875 {
		t.Fatal("partial/fractional response failed")
	}
}
func TestQuotaErrorsRedacted(t *testing.T) {
	for _, response := range []pluginapi.HTTPResponse{{StatusCode: 401, Body: []byte("secret")}, {StatusCode: http.StatusOK, Body: []byte("secret")}} {
		m := NewManager(NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) { return hostOK(response), nil }}).call))
		m.cfg = config.Config{BaseURL: "https://quota.test"}
		_, err := m.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: ProviderID, Attributes: map[string]string{"api_key": "secret"}})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unredacted or missing error")
		}
	}
}
func TestAccountLabels(t *testing.T) {
	cfg, err := config.Load([]byte("api-keys:\n  - value: one\n    name: Personal\n  - value: two\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ key, old, want string }{{"one", "OpenCode Go credential hash", "Personal"}, {"two", "opencode-go-key-hash", "OpenCode Go 2"}, {"two", "Work", "Work"}} {
		if got := accountLabel(cfg, tt.key, tt.old, 0); got != tt.want {
			t.Fatalf("got %q want %q", got, tt.want)
		}
	}
	raw := []byte(`{"type":"opencode-go","id":"stable-id","label":"OpenCode Go credential hash","api_key":"one","disabled":true}`)
	result, err := (authProvider{cfg: cfg}).ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: ProviderID, RawJSON: raw})
	if err != nil || result.Auth.Label != "Personal" || result.Auth.ID != "stable-id" || string(result.Auth.StorageJSON) != string(raw) {
		t.Fatal("name migration changed credential storage")
	}
	if accountLabel(config.Config{}, "key", "", 0) != "OpenCode Go" {
		t.Fatal("bad default name")
	}
}

func TestReadableAuthFilenameAtColdStartDoesNotDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Personal.json")
	raw := []byte(`{"type":"opencode-go","api_key":"existing","label":"Personal","disabled":true}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{Name: "Personal.json", Provider: ProviderID, Path: path}}}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	if err := m.materializeAuthRecords(context.Background(), config.Config{APIKeys: []config.APIKey{{Value: "existing"}}}); err != nil {
		t.Fatal(err)
	}
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("duplicated existing credential")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("modified host metadata")
	}
}
