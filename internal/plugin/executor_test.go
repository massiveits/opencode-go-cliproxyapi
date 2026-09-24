package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/errclass"
)

const (
	multiRouteCatalog = `{"data":[
		{"id":"glm-5.3"},
		{"id":"minimax-m3"},
		{"id":"gpt-5.6-luna"}
	]}`
	ccRequestBody        = `{"model":"whatever","messages":[{"role":"user","content":"hi"}]}`
	ccResponseBody       = `{"id":"r1","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	claudeRequestBody    = `{"model":"x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	claudeResponseBody   = `{"id":"msg_1","model":"minimax-m3","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`
	responsesPassthrough = `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-luna","output":[]}`
)

func execReqBody(model, format string, body []byte, stream bool) []byte {
	b, _ := json.Marshal(executorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey},
			Model: model, SourceFormat: format, OriginalRequest: body, Stream: stream,
		},
	})
	return b
}

func execStreamReqBody(model, format string, body []byte, downStreamID string) []byte {
	return execStreamReqBodyForKey(model, format, body, downStreamID, testKey)
}

func execStreamReqBodyForKey(model, format string, body []byte, downStreamID, key string) []byte {
	b, _ := json.Marshal(executorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": key},
			Model: model, SourceFormat: format, OriginalRequest: body, Stream: true,
		},
		StreamID: downStreamID,
	})
	return b
}

// streamScript scripts the streaming host callbacks for one execute_stream.
type streamScript struct {
	startStatus  int   // 400+ value fails the open pre-first-byte
	startErr     error // transport failure on the open itself
	upstreamID   string
	frames       []string // upstream SSE chunks served in order
	readErr      error    // transport failure on stream_read
	readErrorMsg string   // upstream-reported error label
}

func streamResponder(s streamScript) func(string, []byte) ([]byte, error) {
	i := 0
	return func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			if s.startErr != nil {
				return nil, s.startErr
			}
			if s.startStatus >= 400 {
				// The real host allocates the stream entry before the
				// status is surfaced, so the id exists even on 4xx.
				return hostOK(hostStreamStartResp{StatusCode: s.startStatus, StreamID: s.upstreamID}), nil
			}
			return hostOK(hostStreamStartResp{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				StreamID:   s.upstreamID,
			}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			if s.readErr != nil {
				return nil, s.readErr
			}
			if s.readErrorMsg != "" {
				return hostOK(hostStreamReadResp{Error: s.readErrorMsg, Done: true}), nil
			}
			if i < len(s.frames) {
				p := s.frames[i]
				i++
				return hostOK(hostStreamReadResp{Payload: []byte(p)}), nil
			}
			return hostOK(hostStreamReadResp{Done: true}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	}
}

func newStreamManager(t *testing.T, script streamScript) (*Manager, *fakeCaller) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(multiRouteCatalog, streamResponder(script))
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f
}

// upstreamRouter answers each bridged call with the canned body matching
// the request URL suffix.
func upstreamRouter(t *testing.T, bodies map[string]string) func(string, []byte) ([]byte, error) {
	return func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if url == "https://models.dev/api.json" {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable}), nil
		}
		for suffix, body := range bodies {
			if strings.HasSuffix(url, suffix) {
				return hostOK(pluginapi.HTTPResponse{
					StatusCode: http.StatusOK,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       []byte(body),
				}), nil
			}
		}
		t.Errorf("no canned upstream for url %v", url)
		return hostErr("test", "unrouted"), nil
	}
}

// wrapWithCatalog answers catalog fetches (/models) itself and delegates
// everything else to next.
func wrapWithCatalog(catalogBody string, next func(string, []byte) ([]byte, error)) func(string, []byte) ([]byte, error) {
	return func(method string, payload []byte) ([]byte, error) {
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		if method == pluginabi.MethodHostHTTPDo {
			if url, _ := wire["url"].(string); url == "https://models.dev/api.json" {
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable}), nil
			}
			if url, _ := wire["url"].(string); strings.HasSuffix(url, "/models") {
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogBody)}), nil
			}
		}
		return next(method, payload)
	}
}

func lastWire(t *testing.T, f *fakeCaller, method string) map[string]any {
	t.Helper()
	calls := f.callsOf(method)
	if len(calls) == 0 {
		t.Fatalf("no %s calls recorded", method)
	}
	return decodePayload(t, calls[len(calls)-1])
}

// wireBody decodes the base64 []byte field `key` of a bridged payload.
func wireBody(t *testing.T, wire map[string]any, key string) []byte {
	t.Helper()
	raw, _ := json.Marshal(wire[key])
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s not base64 string: %v (%v)", key, err, wire[key])
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("%s base64 decode: %v", key, err)
	}
	return b
}

func newExecManager(t *testing.T) (*Manager, *fakeCaller) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(multiRouteCatalog, upstreamRouter(t, map[string]string{
		"/v1/chat/completions": ccResponseBody,
		"/v1/messages":         claudeResponseBody,
		"/v1/responses":        responsesPassthrough,
	}))
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f
}

func mustExecute(t *testing.T, m *Manager, model, format string, body []byte) pluginabi.Envelope {
	t.Helper()
	m.mu.RLock()
	key := ""
	if len(m.cfg.APIKeys) > 0 {
		key = m.cfg.APIKeys[0].Value
	}
	m.mu.RUnlock()
	resp, err := m.HandleCall("executor.execute", execReqBodyWithKey(model, format, body, false, key))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return decodeEnv(t, resp)
}

func execReqBodyWithKey(model, format string, body []byte, stream bool, key string) []byte {
	b, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": key},
		Model: model, SourceFormat: format, OriginalRequest: body, Stream: stream,
	}})
	return b
}

func execReqBodyWithSessionInputs(model, format string, body []byte, stream bool, key, canonical, header string) []byte {
	b, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": key},
		Model: model, SourceFormat: format, OriginalRequest: body, Stream: stream,
		Metadata: map[string]any{"canonical_session_id": canonical},
		Headers:  http.Header{"X-Claude-Code-Session-Id": []string{header}},
	}})
	return b
}

func assertSessionHeaders(t *testing.T, wire map[string]any, wantSession, wantAuth string) {
	t.Helper()
	headers, ok := wire["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers missing: %v", wire)
	}
	if got := headers["X-Opencode-Session"].([]any)[0].(string); got != wantSession {
		t.Fatalf("session = %q, want %q", got, wantSession)
	}
	if got := headers["Authorization"]; wantAuth != "" {
		if got.([]any)[0].(string) != wantAuth {
			t.Fatalf("Authorization = %v, want %q", got, wantAuth)
		}
	} else if got, ok := headers["X-Api-Key"]; !ok || got.([]any)[0].(string) != testKey {
		t.Fatalf("x-api-key missing: %v", headers)
	}
	for _, forbidden := range []string{"X-Opencode-Client", "X-Opencode-Request", "X-Opencode-Project", "User-Agent"} {
		if _, ok := headers[forbidden]; ok {
			t.Fatalf("forbidden header %q present: %v", forbidden, headers)
		}
	}
}

func TestExecutorSessionHeaderMatrix(t *testing.T) {
	// AC-H: every source format reaches every route in both
	// execution modes with the same opaque digest and route authentication.
	formats := []struct {
		name string
		body []byte
	}{
		{"openai", []byte(`{"model":"x","messages":[{"role":"user","content":"matrix"}]}`)},
		{"claude", []byte(`{"model":"x","max_tokens":16,"messages":[{"role":"user","content":"matrix"}]}`)},
		{"openai-response", []byte(`{"model":"x","input":"matrix"}`)},
	}
	routes := []struct {
		name  string
		model string
		auth  string
	}{
		{"chat", "opencode-go/glm-5.3", "Bearer " + testKey},
		{"messages", "opencode-go/minimax-m3", ""},
		{"responses", "opencode-go/gpt-5.6-luna", "Bearer " + testKey},
	}
	const wantSession = "6e00cd562cc2d88e238dfb81d9439de7ec843ee9d0c9879d549cb1436786f975"
	for _, mode := range []string{"non-stream", "stream"} {
		for _, format := range formats {
			for _, route := range routes {
				t.Run(mode+"/"+format.name+"/"+route.name, func(t *testing.T) {
					var f *fakeCaller
					var m *Manager
					if mode == "non-stream" {
						m, f = newExecManager(t)
						resp, err := m.HandleCall("executor.execute", execReqBody(route.model, format.name, format.body, false))
						if err != nil || !decodeEnv(t, resp).OK {
							t.Fatalf("execute: %v %s", err, resp)
						}
						assertSessionHeaders(t, lastWire(t, f, pluginabi.MethodHostHTTPDo), wantSession, route.auth)
					} else {
						m, f = newStreamManager(t, streamScript{upstreamID: "matrix-up"})
						resp, err := m.HandleCall("executor.execute_stream", execStreamReqBody(route.model, format.name, format.body, "matrix-down"))
						if err != nil || !decodeEnv(t, resp).OK {
							t.Fatalf("execute_stream: %v %s", err, resp)
						}
						calls := f.callsOf(pluginabi.MethodHostHTTPDoStream)
						if len(calls) != 1 {
							t.Fatalf("do_stream calls = %d, want 1", len(calls))
						}
						assertSessionHeaders(t, decodePayload(t, calls[0]), wantSession, route.auth)
					}
				})
			}
		}
	}
}

func TestExecutorSessionFallbackAndMalformedInput(t *testing.T) {
	t.Run("no user is forwarded in both modes", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"assistant","content":"prefill"}]}`)

		m, f := newExecManager(t)
		if env := mustExecute(t, m, "opencode-go/glm-5.3", "openai", body); !env.OK {
			t.Fatalf("non-stream envelope = %+v", env.Error)
		}
		assertSessionHeaders(t, lastWire(t, f, pluginabi.MethodHostHTTPDo), emptyOpenCodeSessionID, "Bearer "+testKey)

		m, f = newStreamManager(t, streamScript{upstreamID: "fallback-up"})
		resp, err := m.HandleCall("executor.execute_stream", execStreamReqBody("opencode-go/glm-5.3", "openai", body, "fallback-down"))
		if err != nil || !decodeEnv(t, resp).OK {
			t.Fatalf("stream envelope = %v %s", err, resp)
		}
		calls := f.callsOf(pluginabi.MethodHostHTTPDoStream)
		if len(calls) != 1 {
			t.Fatalf("do_stream calls = %d, want 1", len(calls))
		}
		assertSessionHeaders(t, decodePayload(t, calls[0]), emptyOpenCodeSessionID, "Bearer "+testKey)
	})

	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream]+" malformed original request", func(t *testing.T) {
			m, f := newExecManager(t)
			beforeDo, beforeStream := len(f.callsOf(pluginabi.MethodHostHTTPDo)), len(f.callsOf(pluginabi.MethodHostHTTPDoStream))
			var resp []byte
			var err error
			if stream {
				resp, err = m.HandleCall("executor.execute_stream", execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(`{"messages":`), "bad-down"))
			} else {
				resp, err = m.HandleCall("executor.execute", execReqBody("opencode-go/glm-5.3", "openai", []byte(`{"messages":`), false))
			}
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			env := decodeEnv(t, resp)
			if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassTranslation) {
				t.Fatalf("envelope = %s", resp)
			}
			if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != beforeDo || len(f.callsOf(pluginabi.MethodHostHTTPDoStream)) != beforeStream {
				t.Fatalf("malformed request made inference callback: do=%d/%d stream=%d/%d", len(f.callsOf(pluginabi.MethodHostHTTPDo)), beforeDo, len(f.callsOf(pluginabi.MethodHostHTTPDoStream)), beforeStream)
			}
		})
	}
}

func TestExecutorSessionInputsPropagateInBothModes(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"session"}]}`)

	m, f := newExecManager(t)
	resp, err := m.HandleCall("executor.execute", execReqBodyWithSessionInputs("opencode-go/glm-5.3", "openai", body, false, testKey, "canonical-non-stream", "header-non-stream"))
	if err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("non-stream execute: %v %s", err, resp)
	}
	assertSessionHeaders(t, lastWire(t, f, pluginabi.MethodHostHTTPDo), "canonical-non-stream", "Bearer "+testKey)

	m, f = newStreamManager(t, streamScript{upstreamID: "session-up"})
	resp, err = m.HandleCall("executor.execute_stream", execReqBodyWithSessionInputs("opencode-go/glm-5.3", "openai", body, true, testKey, "", "header-stream"))
	if err != nil || !decodeEnv(t, resp).OK {
		t.Fatalf("stream execute: %v %s", err, resp)
	}
	calls := f.callsOf(pluginabi.MethodHostHTTPDoStream)
	if len(calls) != 1 {
		t.Fatalf("do_stream calls = %d, want 1", len(calls))
	}
	assertSessionHeaders(t, decodePayload(t, calls[0]), "header-stream", "Bearer "+testKey)
}

func TestExecuteChatRoute(t *testing.T) {
	f := &fakeCaller{responder: wrapWithCatalog(testCatalogJSON, upstreamRouter(t, map[string]string{
		"/v1/chat/completions": ccResponseBody,
	}))}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}

	env := mustExecute(t, m, "opencode-go/glm-5.3", "openai", []byte(ccRequestBody))
	if !env.OK || env.Error != nil {
		t.Fatalf("execute envelope error: %+v", env.Error)
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	if !bytes.Contains(out.Payload, []byte(`"finish_reason":"stop"`)) {
		t.Fatalf("payload not converted CC: %s", out.Payload)
	}

	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	if wire["method"] != http.MethodPost {
		t.Fatalf("bridged method = %v", wire["method"])
	}
	if wire["url"] != "https://opencode.ai/zen/go/v1/chat/completions" {
		t.Fatalf("bridged url = %v", wire["url"])
	}
	headers := wire["headers"].(map[string]any)
	if headers["Authorization"].([]any)[0].(string) != "Bearer "+testKey {
		t.Fatalf("auth header wrong: %v", headers)
	}
	if headers["X-Opencode-Session"].([]any)[0].(string) != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("session header wrong: %v", headers)
	}
	var upstream map[string]any
	if err := json.Unmarshal(wireBody(t, wire, "body"), &upstream); err != nil {
		t.Fatalf("upstream body decode: %v", err)
	}
	if upstream["model"] != "glm-5.3" {
		t.Fatalf("upstream model = %v, want rewritten upstream id", upstream["model"])
	}

	env = mustExecute(t, m, "glm-5.3", "openai", []byte(ccRequestBody))
	if !env.OK {
		t.Fatalf("bare-id execute failed: %+v", env.Error)
	}
}

func TestExecuteUnknownModelIs404Envelope(t *testing.T) {
	m, _ := newExecManager(t)
	env := mustExecute(t, m, "opencode-go/nope", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil || env.Error.Code != "invalid_model" || env.Error.HTTPStatus != http.StatusNotFound {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

func TestExecuteUnsupportedSourceFormat(t *testing.T) {
	m, _ := newExecManager(t)
	env := mustExecute(t, m, "opencode-go/glm-5.3", "bogus-format", []byte(ccRequestBody))
	if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassUnsupported) {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

func TestExecuteMessagesRouteNativeClaudePassesThrough(t *testing.T) {
	m, f := newExecManager(t)
	env := mustExecute(t, m, "opencode-go/minimax-m3", "claude", []byte(claudeRequestBody))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	if string(out.Payload) != claudeResponseBody {
		t.Fatalf("native passthrough altered body:\n%s\nwant\n%s", out.Payload, claudeResponseBody)
	}
	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	if !strings.HasSuffix(wire["url"].(string), "/v1/messages") {
		t.Fatalf("url = %v", wire["url"])
	}
	headers := wire["headers"].(map[string]any)
	// http.Header.Set canonicalizes keys on the wire.
	if headers["X-Api-Key"].([]any)[0].(string) != testKey {
		t.Fatalf("X-Api-Key wrong: %v", headers)
	}
	if headers["Anthropic-Version"].([]any)[0].(string) == "" {
		t.Fatalf("Anthropic-Version missing: %v", headers)
	}
	if headers["X-Opencode-Session"].([]any)[0].(string) != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("session header wrong: %v", headers)
	}
	if _, has := headers["Authorization"]; has {
		t.Fatalf("messages route must not send Bearer header: %v", headers)
	}
}

func TestExecuteMessagesUpstreamStatusClassified(t *testing.T) {
	f := &fakeCaller{responder: wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(map[string]any{}), nil
		}
		if method == pluginabi.MethodHostAuthSave {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if method == pluginabi.MethodHostHTTPDo && strings.HasSuffix(url, "/messages") {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable, Body: []byte("nope")}), nil
		}
		if method == pluginabi.MethodHostLog {
			return hostOK(map[string]any{}), nil
		}
		t.Fatalf("unexpected call %v %v", method, url)
		return nil, nil
	})}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	env := mustExecute(t, m, "opencode-go/minimax-m3", "claude", []byte(claudeRequestBody))
	if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassUpstream) || !env.Error.Retryable {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

func TestExecuteResponsesRouteNativePassesThrough(t *testing.T) {
	m, f := newExecManager(t)
	env := mustExecute(t, m, "gpt-5.6-luna", "openai-response", []byte(`{"model":"x","input":"hi"}`))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	if string(out.Payload) != responsesPassthrough {
		t.Fatalf("passthrough altered body: %s", out.Payload)
	}
	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	if !strings.HasSuffix(wire["url"].(string), "/v1/responses") {
		t.Fatalf("url = %v", wire["url"])
	}
	headers := wire["headers"].(map[string]any)
	if headers["Authorization"].([]any)[0].(string) != "Bearer "+testKey {
		t.Fatalf("bearer missing: %v", headers)
	}
	if headers["X-Opencode-Session"].([]any)[0].(string) != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("session header wrong: %v", headers)
	}
}

func TestExecuteStreamRoutedFromBothMethods(t *testing.T) {
	t.Run("from execute with stream flag", func(t *testing.T) {
		m, f := newStreamManager(t, streamScript{startStatus: http.StatusTooManyRequests})
		resp, err := m.HandleCall("executor.execute", execReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), true))
		if err != nil {
			t.Fatalf("execute(stream flag): %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "rate_limit" || env.Error.HTTPStatus != http.StatusTooManyRequests {
			t.Fatalf("execute(stream flag) envelope = %+v", env.Error)
		}
		// Pre-first-byte failure: nothing opened, nothing emitted, nothing closed.
		if got := len(f.callsOf(pluginabi.MethodHostStreamEmit)); got != 0 {
			t.Fatalf("emits before status known good = %d", got)
		}
		if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamRead)); got != 0 {
			t.Fatalf("reads after failed start = %d", got)
		}
		if got := len(f.callsOf(pluginabi.MethodHostStreamClose)); got != 0 {
			t.Fatalf("downstream closes without open = %d", got)
		}
	})
	t.Run("from execute_stream", func(t *testing.T) {
		m, _ := newStreamManager(t, streamScript{startStatus: http.StatusTooManyRequests})
		resp, err := m.HandleCall("executor.execute_stream", execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "down-1"))
		if err != nil {
			t.Fatalf("execute_stream: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "rate_limit" {
			t.Fatalf("execute_stream envelope = %+v", env.Error)
		}
	})
	t.Run("malformed body", func(t *testing.T) {
		m, _ := newStreamManager(t, streamScript{})
		resp, err := m.HandleCall("executor.execute_stream", []byte("{"))
		if err != nil {
			t.Fatalf("malformed execute_stream: %v", err)
		}
		if env := decodeEnv(t, resp); env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
			t.Fatalf("malformed execute_stream envelope = %s", resp)
		}
	})
	t.Run("unsupported source format", func(t *testing.T) {
		m, _ := newStreamManager(t, streamScript{})
		resp, err := m.HandleCall("executor.execute_stream",
			execStreamReqBody("opencode-go/glm-5.3", "bogus-format", []byte(ccRequestBody), "down-1"))
		if err != nil {
			t.Fatalf("execute_stream bad format: %v", err)
		}
		if env := decodeEnv(t, resp); env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassUnsupported) {
			t.Fatalf("bad-format execute_stream envelope = %s", resp)
		}
	})
}

func TestExecuteStreamHappyPathClaudeSource(t *testing.T) {
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-1",
		frames: []string{
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}` + "\n\n",
			"data: [DONE]\n\n",
		},
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("envelope = %s", resp)
	}
	streamWire := decodePayload(t, f.callsOf(pluginabi.MethodHostHTTPDoStream)[0])
	streamHeaders := streamWire["headers"].(map[string]any)
	if streamHeaders["X-Opencode-Session"].([]any)[0].(string) != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("stream session header wrong: %v", streamHeaders)
	}
	m.bridge.WaitForInFlight(5 * time.Second)

	emits := f.callsOf(pluginabi.MethodHostStreamEmit)
	if len(emits) < 3 {
		t.Fatalf("emits = %d, want translated event sequence", len(emits))
	}
	var blob strings.Builder
	for i, e := range emits {
		var wire map[string]any
		if err := json.Unmarshal(e.payload, &wire); err != nil {
			t.Fatalf("emit %d payload: %v", i, err)
		}
		if wire["stream_id"] != "down-9" {
			t.Fatalf("emit %d downstream id = %v", i, wire["stream_id"])
		}
		blob.Write(wireBody(t, wire, "payload"))
	}
	if !strings.Contains(blob.String(), "message_start") ||
		!strings.Contains(blob.String(), "content_block_delta") ||
		!strings.Contains(blob.String(), "message_stop") {
		t.Fatalf("missing anthropic events in stream:\n%s", blob.String())
	}

	upCloses := f.callsOf(pluginabi.MethodHostHTTPStreamClose)
	if len(upCloses) != 1 || !strings.Contains(string(upCloses[0].payload), `"up-1"`) {
		t.Fatalf("upstream closes = %v", upCloses)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 {
		t.Fatalf("downstream closes = %d, want 1", len(downCloses))
	}
	if strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("clean close must not carry error: %s", downCloses[0].payload)
	}
}

func TestExecuteStreamCleanCloseWithoutTerminalFrame(t *testing.T) {
	// Upstream ends (bridge done) without sending [DONE]: the executor must
	// flush the converter, close both streams cleanly, and report success.
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-5",
		frames: []string{
			`data: {"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}` + "\n\n",
		},
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("envelope = %s", resp)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream closes = %d", got)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream closes = %v", downCloses)
	}
}

// F5 pin: upstream closes right after the finish_reason chunk without
// [DONE]; the deferred terminal must still reach the client before the
// clean close.
func TestExecuteStreamCloseAfterFinishFlushesTerminal(t *testing.T) {
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-f5",
		frames: []string{
			`data: {"id":"c3","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n",
			`data: {"id":"c3","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}` + "\n\n",
		},
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-f5"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	streamWire := decodePayload(t, f.callsOf(pluginabi.MethodHostHTTPDoStream)[0])
	streamHeaders := streamWire["headers"].(map[string]any)
	if streamHeaders["X-Opencode-Session"].([]any)[0].(string) != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("stream session header wrong: %v", streamHeaders)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	var blob strings.Builder
	for _, e := range f.callsOf(pluginabi.MethodHostStreamEmit) {
		blob.Write(wireBody(t, decodePayload(t, e), "payload"))
	}
	if !strings.Contains(blob.String(), "message_delta") || !strings.Contains(blob.String(), "message_stop") {
		t.Fatalf("terminal events missing after close-without-DONE:\n%s", blob.String())
	}
	if got := strings.Count(blob.String(), "event: message_delta"); got != 1 {
		t.Fatalf("message_delta emitted %d times, want 1", got)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream closes = %v", downCloses)
	}
}

// F5 companion: a flush emit failure fails the stream like any other
// mid-stream emit failure instead of reporting a clean close.
func TestExecuteStreamFlushEmitFailureFailsStream(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	base := streamResponder(streamScript{
		upstreamID: "up-f5x",
		frames: []string{
			`data: {"id":"c4","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"x"}}]}` + "\n\n",
			`data: {"id":"c4","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		},
	})
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostStreamEmit {
			// Fail only the flushed terminal emit (the wire body is
			// base64-encoded), not the earlier Feed-emitted events.
			var wire map[string]any
			_ = json.Unmarshal(payload, &wire)
			raw, err := base64.StdEncoding.DecodeString(wire["payload"].(string))
			if err == nil && bytes.Contains(raw, []byte("event: message_delta")) {
				return hostErr("detached", "client vanished"), nil
			}
		}
		return base(method, payload)
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-f5x"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial headers envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream stream must be closed on flush emit failure")
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream must close with error: %v", downCloses)
	}
}

func TestExecuteStream4xxClosesUpstreamEntry(t *testing.T) {
	// F1 regression: a 400+ open leaves a registered upstream entry that
	// must be closed before the classified error is returned.
	m, f := newStreamManager(t, streamScript{startStatus: http.StatusUnauthorized, upstreamID: "up-4xx"})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != "auth_failure" || env.Error.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("envelope = %+v", env.Error)
	}
	upCloses := f.callsOf(pluginabi.MethodHostHTTPStreamClose)
	if len(upCloses) != 1 || !strings.Contains(string(upCloses[0].payload), `"up-4xx"`) {
		t.Fatalf("upstream closes = %v, want exactly one close of up-4xx", upCloses)
	}
	if got := len(f.callsOf(pluginabi.MethodHostStreamClose)); got != 0 {
		t.Fatalf("downstream must stay host-owned pre-first-byte, closes = %d", got)
	}
}

func TestExecuteStreamOpenTransportError(t *testing.T) {
	// Pre-first-byte network failure produces no downstream bytes and no stream
	// lifecycle to clean up.
	m, f := newStreamManager(t, streamScript{startErr: errors.New("connection refused")})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassNetwork) || !env.Error.Retryable {
		t.Fatalf("envelope = %+v", env.Error)
	}
	if got := len(f.callsOf(pluginabi.MethodHostStreamEmit)); got != 0 {
		t.Fatalf("emits after failed open = %d", got)
	}
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 0 {
		t.Fatalf("closes without open = %d", got)
	}
}

func TestExecuteStreamMessagesRouteNative(t *testing.T) {
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-6",
		frames: []string{
			"event: message_start\n" + `data: {"type":"message_start","message":{"id":"m1","role":"assistant","content":[]}}` + "\n\n",
			"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n",
		},
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/minimax-m3", "claude", []byte(claudeRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	var blob strings.Builder
	for _, e := range f.callsOf(pluginabi.MethodHostStreamEmit) {
		blob.Write(wireBody(t, decodePayload(t, e), "payload"))
	}
	if !strings.Contains(blob.String(), "message_start") {
		t.Fatalf("messages-route stream not translated:\n%s", blob.String())
	}
}

func TestExecuteStreamResponsesRouteNative(t *testing.T) {
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-7",
		frames: []string{
			"event: response.created\n" + `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n",
		},
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("gpt-5.6-luna", "openai-response", []byte(`{"model":"x","input":"hi","stream":true}`), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("envelope = %s", resp)
	}
	streamWire := decodePayload(t, f.callsOf(pluginabi.MethodHostHTTPDoStream)[0])
	streamHeaders := streamWire["headers"].(map[string]any)
	if streamHeaders["X-Opencode-Session"].([]any)[0].(string) != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("stream session header wrong: %v", streamHeaders)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream closes = %d", got)
	}
	if got := len(f.callsOf(pluginabi.MethodHostStreamClose)); got != 1 {
		t.Fatalf("downstream closes = %d", got)
	}
}

func TestExecuteStreamEmitFailsMidstream(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	base := streamResponder(streamScript{
		upstreamID: "up-8",
		frames: []string{
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}` + "\n\n",
		},
	})
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostStreamEmit {
			return hostErr("detached", "client vanished"), nil
		}
		return base(method, payload)
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream must be closed after emit failure, got %d", got)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream must close with error: %v", downCloses)
	}
}

func TestExecuteStreamPartialLineDroppedOnCleanClose(t *testing.T) {
	// No trailing newline and no [DONE]: the buffered partial line never
	// formed an SSE event, so the stream still ends cleanly.
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-9",
		frames:     []string{"data: {truncated"},
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("envelope = %s", resp)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream must close cleanly: %v", downCloses)
	}
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream closes = %d", got)
	}
}

func TestExecuteStreamExceedsMaxResponseBytes(t *testing.T) {
	// F9 regression: the max-response-bytes cap must accumulate across
	// stream payloads and fail closed with both streams closed.
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	tinyCatalog := `{"data":[{"id":"glm-5.4"}]}`
	frame := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` +
		strings.Repeat("x", 60) + `"}]}` + "\n\n"
	script := streamScript{upstreamID: "up-cap", frames: []string{frame, frame}}
	f.responder = wrapWithCatalog(tinyCatalog, streamResponder(script))
	if _, err := m.HandleCall("plugin.register",
		lifecycleRequestBody(testValidYAML+"max-response-bytes: 64\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.4", "openai", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream closes = %d, want 1", got)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream must close with error: %v", downCloses)
	}
}

func TestExecuteStreamWatchdogClosesIdleUpstream(t *testing.T) {
	// F6: the wire format has no deadline field, so an idle upstream would
	// block stream_read forever; the request-timeout watchdog must close the
	// upstream (unblocking the read) and fail with a timeout-class error.
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	tinyCatalog := `{"data":[{"id":"glm-5.4"}]}`
	var once sync.Once
	idle := make(chan struct{})
	f.responder = wrapWithCatalog(tinyCatalog, func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "up-idle"}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			<-idle // upstream sends nothing until the watchdog closes it
			return hostOK(hostStreamReadResp{Done: true}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			once.Do(func() { close(idle) })
			return hostOK(map[string]any{}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	})
	if _, err := m.HandleCall("plugin.register",
		lifecycleRequestBody(testValidYAML+"request-timeout: 50ms\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.4", "openai", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	upCloses := f.callsOf(pluginabi.MethodHostHTTPStreamClose)
	if len(upCloses) == 0 || !strings.Contains(string(upCloses[0].payload), `"up-idle"`) {
		t.Fatalf("watchdog must close up-idle first, closes = %v", upCloses)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream must close with error: %v", downCloses)
	}
}

func TestExecuteStreamReadTransportError(t *testing.T) {
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-2",
		readErr:    errors.New("pipe broke"),
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if got := len(f.callsOf(pluginabi.MethodHostStreamEmit)); got != 0 {
		t.Fatalf("emits after read failure = %d", got)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), `"error":"`) ||
		!strings.Contains(string(downCloses[0].payload), "pipe broke") {
		t.Fatalf("downstream must close with redacted error: %v", downCloses)
	}
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream close count = %d, want 1", got)
	}
}

// TestExecuteStreamPanicStillClosesStreams pins the deferred once-closer: a
// panic out of StreamRead (CGO bridge calls can panic) is contained by the
// bridge's recover as a redacted error — same policy as every other host
// callback — and the pump treats it as a post-first-byte failure whose
// fail() closes both streams exactly once, downstream cleanly.
func TestExecuteStreamPanicStillClosesStreams(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				StreamID:   "up-panic",
			}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			panic("cgo bridge exploded")
		default:
			return hostOK(map[string]any{}), nil
		}
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "down-p"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream closes after panic = %d, want 1", got)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 {
		t.Fatalf("downstream closes after panic = %d, want 1", len(downCloses))
	}
	if !strings.Contains(string(downCloses[0].payload), `"error":"host http stream_read failed: host callback panicked"`) {
		t.Fatalf("downstream must close with the redacted panic label: %s", downCloses[0].payload)
	}
}

func TestExecuteStreamUpstreamErrorLabel(t *testing.T) {
	m, f := newStreamManager(t, streamScript{upstreamID: "up-3", readErrorMsg: "upstream exploded"})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), "upstream exploded") {
		t.Fatalf("downstream close missing label: %v", downCloses)
	}
}

func TestExecuteStreamConverterError(t *testing.T) {
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-4",
		frames:     []string{"data: {definitely not json\n\n"},
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream must be closed after converter failure, got %d", got)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream must close with error: %v", downCloses)
	}
}

// F1 pin: a retryable-classified converter error arriving AFTER events
// were already emitted downstream must not surface a retryable envelope —
// host re-execution would duplicate delivered content.
func TestExecuteStreamChunkErrorAfterEmitNotRetryable(t *testing.T) {
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-f1a",
		frames: []string{
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hel"}}]}` + "\n\n",
			`data: {"error":{"message":"boom","code":500}}` + "\n\n",
		},
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-f1"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream must close with error: %v", downCloses)
	}
}

// F1 companion: the same classifier error BEFORE any emission closes stream with error.
func TestExecuteStreamChunkErrorBeforeEmitPreservesRetryable(t *testing.T) {
	m, f := newStreamManager(t, streamScript{
		upstreamID: "up-f1b",
		frames: []string{
			`data: {"error":{"message":"boom","code":429}}` + "\n\n",
		},
	})
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-f1"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if got := len(f.callsOf(pluginabi.MethodHostStreamEmit)); got != 0 {
		t.Fatalf("emits = %d, want none before failure", got)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream must close with error: %v", downCloses)
	}
}

func TestExecuteMalformedBody(t *testing.T) {
	m, _ := newExecManager(t)
	resp, err := m.HandleCall("executor.execute", []byte("{"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
		t.Fatalf("envelope = %s", resp)
	}
}

func TestExecutorIdentifierAndCountTokens(t *testing.T) {
	m, _ := newExecManager(t)
	resp, err := m.HandleCall("executor.identifier", nil)
	if err != nil {
		t.Fatalf("identifier: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || !strings.Contains(string(env.Result), `"identifier":"opencode-go"`) {
		t.Fatalf("identifier envelope = %s", resp)
	}
	resp, err = m.HandleCall("executor.count_tokens", nil)
	if err != nil {
		t.Fatalf("count_tokens: %v", err)
	}
	env = decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassUnsupported) {
		t.Fatalf("count_tokens envelope = %s", resp)
	}
	// executor.http_request has no handler either: it must classify as
	// unsupported (not generic unknown_method), naming the method/endpoint.
	resp, err = m.HandleCall(pluginabi.MethodExecutorHTTPRequest, nil)
	if err != nil {
		t.Fatalf("http_request: %v", err)
	}
	env = decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassUnsupported) {
		t.Fatalf("http_request envelope = %s", resp)
	}
	if !strings.Contains(env.Error.Message, "executor.http_request") {
		t.Fatalf("message must name the endpoint: %q", env.Error.Message)
	}
}

func TestRegistrationCapabilitiesIncludeExecutor(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registrationResult
	decodeResult(t, resp, &reg)
	want := []string{"openai", "claude", "openai-response"}
	if !reg.Capabilities.Executor || !reg.Capabilities.AuthProvider ||
		!reflectDeepEqualStrings(reg.Capabilities.ExecutorInputFormats, want) ||
		!reflectDeepEqualStrings(reg.Capabilities.ExecutorOutputFormats, want) {
		t.Fatalf("capabilities = %+v", reg.Capabilities)
	}
}

func reflectDeepEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExecuteMaxResponseBytesGuard(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	// The cap also applies to catalog fetches, so keep the catalog tiny and
	// make only the upstream completion body exceed it.
	tinyCatalog := `{"data":[{"id":"glm-5.4"}]}`
	bigBody := `{"id":"r2","choices":[{"index":0,"message":{"role":"assistant","content":"` +
		strings.Repeat("x", 150) + `"},"finish_reason":"stop"}]}`
	f.responder = wrapWithCatalog(tinyCatalog, upstreamRouter(t, map[string]string{
		"/v1/chat/completions": bigBody,
	}))
	if _, err := m.HandleCall("plugin.register",
		lifecycleRequestBody(testValidYAML+"max-response-bytes: 100\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	env := mustExecute(t, m, "opencode-go/glm-5.4", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassTranslation) {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

func TestExecuteTransportErrorClassified(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(map[string]any{}), nil
		}
		if method == pluginabi.MethodHostAuthSave {
			return hostOK(map[string]any{}), nil
		}
		return nil, errors.New("cable cut")
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	env := mustExecute(t, m, "opencode-go/glm-5.3", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassNetwork) {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

// White-box coverage for seams unreachable through full executes.
func TestConvertNonStreamSeamBranches(t *testing.T) {
	if _, eErr := convertNonStream(catalog.Route("weird"), "openai", http.StatusOK, nil); eErr == nil ||
		eErr.Class != errclass.ClassTranslation {
		t.Fatalf("unknown route = %v", eErr)
	}
	if _, eErr := buildUpstreamRequest(catalog.Route("weird"), "m", "openai", nil, nil); eErr == nil ||
		eErr.Class != errclass.ClassTranslation {
		t.Fatalf("unknown route build = %v", eErr)
	}
	if got := catalog.JoinUpstreamURL("https://gw.test/", "/v1/responses"); got != "https://gw.test/responses" {
		t.Fatalf("url join = %q", got)
	}
	if got := upstreamAuthHeaders(catalog.RouteChatCompletions, "k", "session"); got.Get("Authorization") != "Bearer k" || got.Get("x-opencode-session") != "session" {
		t.Fatalf("bearer headers = %v", got)
	}
	// Adapters own status classification uniformly (§7).
	if _, eErr := convertNonStream(catalog.RouteMessages, "claude", http.StatusServiceUnavailable, []byte("upstream down")); eErr == nil ||
		eErr.Class != errclass.ClassUpstream || !eErr.Retryable {
		t.Fatalf("status classify = %v", eErr)
	}
	// Cross formats now translate: an empty Messages body is a translation
	// failure from the adapter's decoder, not a not-supported stub.
	if _, eErr := convertNonStream(catalog.RouteMessages, "openai", http.StatusOK, []byte("{}")); eErr == nil ||
		eErr.Class != errclass.ClassTranslation || !strings.Contains(eErr.Message, "no content blocks") {
		t.Fatalf("cross-format messages = %v", eErr)
	}
	if _, eErr := convertNonStream(catalog.RouteResponses, "claude", http.StatusOK, []byte("{}")); eErr == nil ||
		eErr.Class != errclass.ClassTranslation || !strings.Contains(eErr.Message, "no output items") {
		t.Fatalf("cross-format responses = %v", eErr)
	}
	if _, eErr := convertNonStream(catalog.RouteMessages, "claude", http.StatusOK, []byte("{bad")); eErr == nil ||
		eErr.Class != errclass.ClassTranslation {
		t.Fatalf("malformed passthrough = %v", eErr)
	}
	long := strings.Repeat("x", 300)
	if got := shared.RedactedSnippet(long); got != long[:80]+"..." {
		t.Fatalf("snippet truncation = %d chars", len(got))
	}
	if got := shared.RedactedSnippet("short"); got != "short" {
		t.Fatalf("short snippet = %q", got)
	}
}

func TestExecuteMalformedUpstreamStatusUsesSelectedKey(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	var seen []string
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		seen = append(seen, method)
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"bad request shape"}`)}), nil
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	env := mustExecute(t, m, "opencode-go/glm-5.3", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil || env.Error.Code != "unsupported_protocol_or_parameter" || env.Error.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("envelope = %+v", env.Error)
	}
	if len(seen) != 1 || seen[0] != pluginabi.MethodHostHTTPDo {
		t.Fatalf("unexpected upstream calls = %v", seen)
	}
}

func TestExecuteStreamMidStreamFailureNoRetry(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	opens := 0
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			opens++
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "up-x"}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			return hostOK(hostStreamReadResp{Error: "midstream blowup", Done: true}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "down-9"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	if opens != 1 {
		t.Fatalf("selected key must be used once, opens = %d", opens)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), "midstream blowup") {
		t.Fatalf("downstream must close with error: %v", downCloses)
	}
}

func TestExecuteInvalidAuthRejection(t *testing.T) {
	m, _ := newExecManager(t)

	t.Run("wrong auth provider", func(t *testing.T) {
		req := executorRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				AuthProvider: "wrong-provider", AuthAttributes: map[string]string{"api_key": testKey},
				Model: "opencode-go/glm-5.3", SourceFormat: "openai", OriginalRequest: []byte(ccRequestBody),
			},
		}
		b, _ := json.Marshal(req)
		resp, err := m.HandleCall("executor.execute", b)
		if err != nil {
			t.Fatalf("handle call: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "auth_failure" || !strings.Contains(env.Error.Message, "selected auth provider is not opencode-go") {
			t.Fatalf("wrong provider envelope = %+v", env.Error)
		}
	})

	t.Run("missing api key", func(t *testing.T) {
		req := executorRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": "   "},
				Model: "opencode-go/glm-5.3", SourceFormat: "openai", OriginalRequest: []byte(ccRequestBody),
			},
		}
		b, _ := json.Marshal(req)
		resp, err := m.HandleCall("executor.execute", b)
		if err != nil {
			t.Fatalf("handle call: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "auth_failure" || !strings.Contains(env.Error.Message, "selected auth has no api key") {
			t.Fatalf("missing key envelope = %+v", env.Error)
		}
	})
}

func TestExecuteStreamInvalidAuthRejection(t *testing.T) {
	m, f := newStreamManager(t, streamScript{})

	t.Run("wrong auth provider", func(t *testing.T) {
		req := executorRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				AuthProvider: "wrong-provider", AuthAttributes: map[string]string{"api_key": testKey},
				Model: "opencode-go/glm-5.3", SourceFormat: "openai", OriginalRequest: []byte(ccRequestBody), Stream: true,
			},
			StreamID: "down-bad-auth",
		}
		b, _ := json.Marshal(req)
		resp, err := m.HandleCall("executor.execute_stream", b)
		if err != nil {
			t.Fatalf("handle call: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "auth_failure" || !strings.Contains(env.Error.Message, "selected auth provider is not opencode-go") {
			t.Fatalf("stream wrong provider envelope = %+v", env.Error)
		}
		if got := len(f.callsOf(pluginabi.MethodHostHTTPDoStream)); got != 0 {
			t.Fatalf("stream opens after auth failure = %d", got)
		}
	})

	t.Run("missing api key", func(t *testing.T) {
		req := executorRequest{
			ExecutorRequest: pluginapi.ExecutorRequest{
				AuthProvider: ProviderID, AuthAttributes: map[string]string{},
				Model: "opencode-go/glm-5.3", SourceFormat: "openai", OriginalRequest: []byte(ccRequestBody), Stream: true,
			},
			StreamID: "down-bad-auth-key",
		}
		b, _ := json.Marshal(req)
		resp, err := m.HandleCall("executor.execute_stream", b)
		if err != nil {
			t.Fatalf("handle call: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "auth_failure" || !strings.Contains(env.Error.Message, "selected auth has no api key") {
			t.Fatalf("stream missing key envelope = %+v", env.Error)
		}
		if got := len(f.callsOf(pluginabi.MethodHostHTTPDoStream)); got != 0 {
			t.Fatalf("stream opens after auth failure = %d", got)
		}
	})
}

func TestExecuteUsesSelectedAuthKeyWithoutFallback(t *testing.T) {
	customKey := "sk-cpa-selected-unique-key"
	f := &fakeCaller{responder: wrapWithCatalog(testCatalogJSON, upstreamRouter(t, map[string]string{
		"/v1/chat/completions": ccResponseBody,
	}))}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := m.HandleCall("executor.execute", execReqBodyWithKey("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), false, customKey))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute envelope error: %+v", env.Error)
	}

	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	headers := wire["headers"].(map[string]any)
	if headers["Authorization"].([]any)[0].(string) != "Bearer "+customKey {
		t.Fatalf("auth header = %v, want Bearer %s", headers["Authorization"], customKey)
	}
}

func TestExecuteStreamUsesSelectedAuthKey(t *testing.T) {
	customKey := "sk-cpa-selected-stream-key"
	var gotAuth string
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDoStream {
			var wire map[string]any
			_ = json.Unmarshal(payload, &wire)
			if headers, ok := wire["headers"].(map[string]any); ok {
				if auth, ok := headers["Authorization"].([]any); ok && len(auth) > 0 {
					gotAuth, _ = auth[0].(string)
				}
			}
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "up-auth"}), nil
		}
		if method == pluginabi.MethodHostHTTPStreamRead {
			return hostOK(hostStreamReadResp{Done: true}), nil
		}
		return hostOK(map[string]any{}), nil
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBodyForKey("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "down-auth", customKey))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream envelope error: %+v", env.Error)
	}
	if gotAuth != "Bearer "+customKey {
		t.Fatalf("stream auth header = %q, want Bearer %s", gotAuth, customKey)
	}
}

func TestExecuteStreamBlockedEmitCannotWedgeTheProducer(t *testing.T) {
	oldEmit := emitTimeout
	emitTimeout = 80 * time.Millisecond
	defer func() { emitTimeout = oldEmit }()

	release := make(chan struct{})
	var once sync.Once
	drain := func() { once.Do(func() { close(release) }) }
	t.Cleanup(drain)

	var reads atomic.Int32
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				StreamID:   "up-blocked",
			}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			if reads.Add(1) == 1 {
				return hostOK(hostStreamReadResp{Payload: []byte(
					`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}` + "\n\n")}), nil
			}
			// Only reachable during teardown drain; ends the pump so the
			// orphaned goroutine exits instead of spinning.
			return hostOK(hostStreamReadResp{Done: true}), nil
		case pluginabi.MethodHostStreamEmit:
			<-release // downstream consumer wedged: this never drains
			return hostOK(map[string]any{}), nil
		default:
			return hostOK(map[string]any{}), nil
		}
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(ccRequestBody), "down-blocked"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("execute_stream initial envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(1 * time.Second)

	upCloses := f.callsOf(pluginabi.MethodHostHTTPStreamClose)
	if len(upCloses) == 0 || !strings.Contains(string(upCloses[0].payload), `"up-blocked"`) {
		t.Fatalf("upstream stream_close not issued on emit failure: %v", upCloses)
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) == 0 || !strings.Contains(string(downCloses[0].payload), `"down-blocked"`) ||
		!strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream close with error label missing: %v", downCloses)
	}
}
