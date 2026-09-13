package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
	"opencode-go-cliproxyapi/resources"
)

func TestManagementRegistration(t *testing.T) {
	m := NewManager(nil)
	var got struct {
		Routes    []struct{ Method, Path string }            `json:"routes"`
		Resources []struct{ Path, Menu, Description string } `json:"resources"`
	}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodManagementRegister, []byte(`{}`)), &got)
	if len(got.Routes) != 1 || got.Routes[0].Method != http.MethodPost || got.Routes[0].Path != "/plugins/"+pluginName+"/quota-usage" {
		t.Fatalf("routes = %+v", got.Routes)
	}
	if len(got.Resources) != 1 || got.Resources[0].Path != "/quota" || got.Resources[0].Menu != "OpenCode Go Quota" {
		t.Fatalf("resources = %+v", got.Resources)
	}
	var registration registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)), &registration)
	if !registration.Capabilities.ManagementAPI {
		t.Fatal("registration did not advertise management_api")
	}
}

func TestQuotaListDoesNotCallHost(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{APIKeys: []config.APIKey{{Value: "quota-key-a"}, {Value: "quota-key-b"}}}
	var got quotaList
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != 2 || got.Cards[0].Label != "OpenCode Go credential "+mustHashPrefix("quota-key-a") || got.Cards[1].Label != "OpenCode Go credential "+mustHashPrefix("quota-key-b") {
		t.Fatalf("cards = %+v", got.Cards)
	}
	if strings.Contains(string(resp.Body), "quota-key-") || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("list leaked key or called upstream: %s calls=%v", resp.Body, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
}

func TestQuotaRefresh(t *testing.T) {
	const key = "quota-refresh-secret"
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil || wire.Method != http.MethodGet || wire.URL != "https://quota.test/v1/usage" || wire.Headers.Get("Authorization") != "Bearer "+key || wire.Headers.Get("Accept") != "application/json" {
			t.Fatalf("bad quota request: %s", payload)
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"rolling":{"status":"ok","percent":9,"resetsAt":"2026-09-05T12:20:04Z"},"weekly":{"status":"ok","percent":12,"resetsAt":"2026-09-10T00:00:00Z"},"monthly":{"status":"ok","percent":6,"resetsAt":"2026-10-01T00:00:00Z"}}}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/v1/", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}}
	id, _ := quotaIdentity(key)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got quotaCard
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage == nil || got.Usage.Rolling.Percent != 9 || got.Usage.Weekly.ResetsAt != "2026-09-10T00:00:00Z" || got.Usage.Monthly.Percent != 6 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestQuotaUnknownKeyAndResource(t *testing.T) {
	const key = "quota-known-secret"
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{APIKeys: []config.APIKey{{Value: key}}}
	unknown := "opencode-go-key-unknown"
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + unknown + `"}`)})
	if err != nil || resp.StatusCode != http.StatusNotFound || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), unknown) || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("unknown response = %+v err=%v calls=%v", resp, err, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
	resource, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/quota"})
	if err != nil || resource.StatusCode != 0 || resource.Headers.Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(string(resource.Body), "OpenCode Go Quota") || strings.Contains(string(resource.Body), key) {
		t.Fatalf("resource response = %+v err=%v", resource, err)
	}
}

func TestQuotaPageReadsRememberedManagementKey(t *testing.T) {
	if !strings.Contains(resources.QuotaPage, "parsed.state && parsed.state.managementKey") {
		t.Fatal("quota page does not read persisted state.managementKey")
	}
}

func TestQuotaPageDecodesCurrentCPAAuthStorage(t *testing.T) {
	for _, marker := range []string{"enc::v1::", "cli-proxy-api-webui::secure-storage", "TextDecoder"} {
		if !strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page does not decode current CPA auth storage: missing %q", marker)
		}
	}
}

func TestQuotaErrorsAreRedacted(t *testing.T) {
	const key = "quota-error-secret"
	id, _ := quotaIdentity(key)
	for name, responder := range map[string]func(string, []byte) ([]byte, error){
		"bridge": func(string, []byte) ([]byte, error) { return nil, context.Canceled },
		"status": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(key + " upstream body")}), nil
		},
		"json": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(key + " not json")}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCaller{responder: responder}
			m := NewManager(NewHostBridge(f.call))
			m.cfg = config.Config{BaseURL: "https://quota.test/v1", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}}
			resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
			if err != nil || resp.StatusCode != http.StatusBadGateway || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), "upstream body") {
				t.Fatalf("response = %+v err=%v", resp, err)
			}
		})
	}
}

func TestQuotaPageIsStaticAndSecretFree(t *testing.T) {
	if resources.QuotaPage == "" || strings.Contains(resources.QuotaPage, testKey) || strings.Contains(resources.QuotaPage, "setInterval") || strings.Contains(resources.QuotaPage, "reset button") {
		t.Fatal("quota page contains a secret, polling, or reset action")
	}
	if !strings.Contains(resources.QuotaPage, "textContent") || !strings.Contains(resources.QuotaPage, "Remember password") {
		t.Fatal("quota page missing safe rendering or login guidance")
	}
}

func TestQuotaPageUsesManualSessionCache(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{
		`const storageKey = "opencode-go-cliproxyapi:quota"`,
		"sessionStorage.getItem(storageKey)",
		"sessionStorage.setItem(storageKey, JSON.stringify(cache))",
		"JSON.parse(stored)",
		"typeof parsed === \"object\"",
		"Array.isArray(parsed)",
		"catch (_) {}",
		"Object.entries(cache)",
		"values.set(keyID, entry.usage)",
		"cache[card.key_id] = {usage: result.usage, fetched_at: timestamp}",
		"delete cache[keyID]",
		"new Set(cards.map(card => card.key_id))",
		"toLocaleString",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("quota page missing session cache marker %q", marker)
		}
	}
	for _, marker := range []string{
		"setTimeout",
		"visibilitychange",
		"document.hidden",
		"window.onfocus",
		"window.addEventListener(\"focus\"",
		"TTL",
		"expiry",
		"expiration",
		"bulk refresh",
	} {
		if strings.Contains(page, marker) {
			t.Fatalf("quota page contains prohibited automatic or expiry behavior %q", marker)
		}
	}
}

func TestQuotaPageUsesNativeQuotaStylesAndThemeBridge(t *testing.T) {
	for _, marker := range []string{
		"quota-page",
		"quota-grid",
		"quota-card",
		"quota-track",
		"quota-fill",
		"repeat(auto-fill, minmax(380px, 1fr))",
		"@media (max-width: 768px)",
		`[data-theme="white"]`,
		`[data-theme="dark"]`,
		`window.parent.document`,
		`data-theme`,
		`--bg-secondary`,
		`frameElement.style.backgroundColor`,
		`frameElement.parentElement.style.backgroundColor`,
		".quota-refresh",
		"--bg-secondary: #faf9f5",
		"--bg-primary: #f0eee8",
		"--bg-tertiary: #e9e6df",
		"--text-primary: #2d2a26",
		"--text-secondary: #6d6760",
		"--text-tertiary: #a29c95",
		"--border-color: #e3e1db",
		"--primary-color: #8b8680",
		"--primary-hover: #7f7a74",
		"border-radius: 8px; padding: 8px 10px",
		`MutationObserver`,
		`catch (_) {}`,
	} {
		if !strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page missing styling marker %q", marker)
		}
	}
	if strings.Count(resources.QuotaPage, `document.createElement("button")`) != 1 || strings.Contains(resources.QuotaPage, `textContent = "Refresh card"`) || strings.Contains(resources.QuotaPage, "quota-button") || strings.Contains(resources.QuotaPage, "quota-refresh-small") {
		t.Fatal("quota page does not have exactly one secondary refresh button path")
	}
	for _, marker := range []string{"querySelectorAll('head link[rel=\"stylesheet\"], head style')", "cloneNode(true)", "dataset.cpaStyle"} {
		if strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page still clones parent styles: %q", marker)
		}
	}
}

func mustHashPrefix(key string) string {
	_, label := quotaIdentity(key)
	return strings.TrimPrefix(label, "OpenCode Go credential ")
}
