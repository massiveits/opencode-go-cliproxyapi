package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// forwardingBridge bridges only the C boundary: host.http.do performs REAL
// http requests against the mock OpenCode Go server; do_stream opens a real
// streamed response and tracks its body so stream_read/stream_close drain
// and release it like the host would.
type forwardingBridge struct {
	mu      sync.Mutex
	streams map[string]*http.Response
	next    int
}

func newForwardingCaller() RawCaller {
	b := &forwardingBridge{streams: map[string]*http.Response{}}
	return b.call
}

func (b *forwardingBridge) perform(wire struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), wire.Method, wire.URL, bytes.NewReader(wire.Body))
	if err != nil {
		return nil, err
	}
	for k, vs := range wire.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return http.DefaultClient.Do(req)
}

func (b *forwardingBridge) decode(payload []byte) (struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}, error) {
	var wire struct {
		Method  string      `json:"method"`
		URL     string      `json:"url"`
		Headers http.Header `json:"headers"`
		Body    []byte      `json:"body"`
	}
	err := json.Unmarshal(payload, &wire)
	return wire, err
}

func (b *forwardingBridge) call(method string, payload []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostHTTPDo:
		wire, err := b.decode(payload)
		if err != nil {
			return hostErr("test", "undecodable host.http.do payload"), nil
		}
		resp, err := b.perform(wire)
		if err != nil {
			return hostErr("transport", err.Error()), nil
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return hostErr("transport", err.Error()), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: body}), nil

	case pluginabi.MethodHostHTTPDoStream:
		wire, err := b.decode(payload)
		if err != nil {
			return hostErr("test", "undecodable payload"), nil
		}
		resp, err := b.perform(wire)
		if err != nil {
			return hostErr("transport", err.Error()), nil
		}
		start := hostStreamStartResp{StatusCode: resp.StatusCode, Headers: resp.Header}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			return hostOK(start), nil
		}
		b.mu.Lock()
		b.next++
		id := fmt.Sprintf("up-%d", b.next)
		b.streams[id] = resp
		b.mu.Unlock()
		start.StreamID = id
		return hostOK(start), nil

	case pluginabi.MethodHostHTTPStreamRead:
		var req hostStreamIDReq
		if err := json.Unmarshal(payload, &req); err != nil {
			return hostErr("test", "undecodable stream_id"), nil
		}
		b.mu.Lock()
		resp := b.streams[req.StreamID]
		b.mu.Unlock()
		if resp == nil {
			return hostErr("gone", "unknown upstream stream"), nil
		}
		buf := make([]byte, 1024)
		n, rerr := resp.Body.Read(buf)
		if n == 0 {
			if rerr == nil || rerr == io.EOF {
				return hostOK(hostStreamReadResp{Done: true}), nil
			}
			return hostErr("transport", rerr.Error()), nil
		}
		return hostOK(hostStreamReadResp{Payload: buf[:n]}), nil

	case pluginabi.MethodHostHTTPStreamClose:
		var req hostStreamIDReq
		if err := json.Unmarshal(payload, &req); err == nil {
			b.mu.Lock()
			if resp := b.streams[req.StreamID]; resp != nil {
				_ = resp.Body.Close()
				delete(b.streams, req.StreamID)
			}
			b.mu.Unlock()
		}
		return hostOK(map[string]any{}), nil

	default:
		// host.log and downstream emit/close are host-side concerns here.
		return hostOK(map[string]any{}), nil
	}
}

// integrationCatalog is the documented tolerant catalog JSON: no protocol
// hints, so the built-in compatibility table routes every entry.
const integrationCatalog = `{"data":[` +
	`{"id":"glm-5.2"},` +
	`{"id":"gpt-5.6-luna"},` +
	`{"id":"qwen3.7-max"}` +
	`]}`

const integrationCompletion = `{"id":"chatcmpl-1","object":"chat.completion","model":"glm-5.2",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"sunny","tool_calls":[` +
	`{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"sf\"}"}}]},` +
	`"finish_reason":"tool_calls"}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

const integrationMessages = `{"id":"msg_1","type":"message","role":"assistant","model":"qwen3.7-max",` +
	`"content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"sf"}}],` +
	`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":9}}`

const integrationResponses = `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-luna",` +
	`"output":[{"type":"message","role":"assistant","id":"m1","content":[{"type":"output_text","text":"hi there"}]},` +
	`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"sf\"}"}],` +
	`"usage":{"input_tokens":5,"output_tokens":6,"total_tokens":11}}`

var chatSSEFrames = []string{
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n",
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n",
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
	"data: [DONE]\n\n",
}

var messagesSSEFrames = []string{
	"event: message_start\n" + `data: {"type":"message_start","message":{"id":"m1","role":"assistant","content":[],"model":"qwen3.7-max"}}` + "\n\n",
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n",
}

var responsesSSEFrames = []string{
	"event: response.created\n" + `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n",
	"event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n",
	"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}` + "\n\n",
}

func writeSSE(w http.ResponseWriter, frames []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	for _, f := range frames {
		_, _ = io.WriteString(w, f)
		fl.Flush()
	}
}

// mockOpenCode is a stand-in OpenCode Go server with mutable outage state,
// swappable catalog / messages payloads, and a scriptable chat endpoint.
type mockOpenCode struct {
	mu                   sync.Mutex
	srv                  *httptest.Server
	catalogDown          bool
	catalogBody          string
	messagesBody         string
	responsesBody        string
	lastPath             string
	lastAuth             string
	lastChatSession      string
	lastChatBody         []byte
	lastMessagesAuth     string
	lastMessagesVersion  string
	lastMessagesSession  string
	lastMessagesBody     []byte
	lastResponsesAuth    string
	lastResponsesSession string
	lastResponsesBody    []byte
	chatHits             int
	chatPlan             func(hit int) (status int, retryAfter string, body string)
	authSeen             []string
}

func newMockOpenCode(t *testing.T) *mockOpenCode {
	t.Helper()
	st := &mockOpenCode{catalogBody: integrationCatalog, messagesBody: integrationMessages, responsesBody: integrationResponses}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		st.mu.Lock()
		down, body := st.catalogDown, st.catalogBody
		st.mu.Unlock()
		if down {
			http.Error(w, "catalog unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		st.mu.Lock()
		st.lastPath = r.URL.Path
		st.lastAuth = r.Header.Get("Authorization")
		st.lastChatSession = r.Header.Get("x-opencode-session")
		st.lastChatBody = body
		st.authSeen = append(st.authSeen, r.Header.Get("Authorization"))
		st.chatHits++
		hit, plan := st.chatHits, st.chatPlan
		st.mu.Unlock()
		if plan != nil {
			status, retryAfter, errBody := plan(hit)
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			http.Error(w, errBody, status)
			return
		}
		if stream, _ := parsed["stream"].(bool); stream {
			writeSSE(w, chatSSEFrames)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(integrationCompletion))
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		st.mu.Lock()
		st.lastMessagesAuth = r.Header.Get("x-api-key")
		st.lastMessagesVersion = r.Header.Get("Anthropic-Version")
		st.lastMessagesSession = r.Header.Get("x-opencode-session")
		st.lastMessagesBody = body
		st.mu.Unlock()
		if stream, _ := parsed["stream"].(bool); stream {
			writeSSE(w, messagesSSEFrames)
			return
		}
		st.mu.Lock()
		respBody := st.messagesBody
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	})
	mux.HandleFunc("/v1/messages-override", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		st.mu.Lock()
		st.lastMessagesAuth = r.Header.Get("x-api-key")
		st.lastMessagesVersion = r.Header.Get("Anthropic-Version")
		st.lastMessagesSession = r.Header.Get("x-opencode-session")
		st.lastMessagesBody = body
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(st.messagesBody))
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		st.mu.Lock()
		st.lastResponsesAuth = r.Header.Get("Authorization")
		st.lastResponsesSession = r.Header.Get("x-opencode-session")
		st.lastResponsesBody = body
		st.mu.Unlock()
		if stream, _ := parsed["stream"].(bool); stream {
			writeSSE(w, responsesSSEFrames)
			return
		}
		st.mu.Lock()
		respBody := st.responsesBody
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	})
	st.srv = httptest.NewServer(mux)
	t.Cleanup(st.srv.Close)
	return st
}

func (st *mockOpenCode) setCatalogDown(down bool) {
	st.mu.Lock()
	st.catalogDown = down
	st.mu.Unlock()
}

func (st *mockOpenCode) setCatalogBody(body string) {
	st.mu.Lock()
	st.catalogBody = body
	st.mu.Unlock()
}

func (st *mockOpenCode) setMessagesBody(body string) {
	st.mu.Lock()
	st.messagesBody = body
	st.mu.Unlock()
}

func (st *mockOpenCode) setResponsesBody(body string) {
	st.mu.Lock()
	st.responsesBody = body
	st.mu.Unlock()
}

// setChatPlan scripts per-hit chat responses; nil restores the default 200.
func (st *mockOpenCode) setChatPlan(plan func(hit int) (status int, retryAfter string, body string)) {
	st.mu.Lock()
	st.chatPlan = plan
	st.chatHits = 0
	st.authSeen = nil
	st.mu.Unlock()
}

func (st *mockOpenCode) chatCalls() (hits int, auths []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.chatHits, append([]string(nil), st.authSeen...)
}

func logBlobOf(f *fakeCaller) string {
	blob := ""
	for _, c := range f.callsOf(pluginabi.MethodHostLog) {
		blob += string(c.payload)
	}
	return blob
}

func (st *mockOpenCode) chatRequest(t *testing.T) (path, auth string, body map[string]any) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	path, auth = st.lastPath, st.lastAuth
	body = map[string]any{}
	if err := json.Unmarshal(st.lastChatBody, &body); err != nil {
		t.Fatalf("mock received non-JSON chat body: %v (%s)", err, st.lastChatBody)
	}
	return path, auth, body
}

func integrationYAML(baseURL string) string {
	// The real base-url carries the version segment (.../zen/go/v1); the
	// mock server root therefore gets "/v1" appended so /v1/models and
	// /v1/chat/completions line up.
	return fmt.Sprintf("base-url: %s/v1\nallow-http: true\napi-keys:\n  - value: sk-test-1\n", baseURL)
}

// newIntegrationManager registers a manager whose outbound calls flow
// through forwardingCaller into a fresh mock OpenCode Go server.
func newIntegrationManager(t *testing.T) (*Manager, *fakeCaller, *mockOpenCode, string) {
	t.Helper()
	st := newMockOpenCode(t)
	f := &fakeCaller{responder: newForwardingCaller()}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	yamlText := integrationYAML(st.srv.URL)
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("register failed: %s", resp)
	}
	return m, f, st, yamlText
}

func integrationStaticIDs(t *testing.T, m *Manager) map[string]pluginapi.ModelInfo {
	t.Helper()
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	byID := map[string]pluginapi.ModelInfo{}
	for _, mi := range static.Models {
		byID[mi.ID] = mi
	}
	return byID
}

// TestRegisterAndDiscover covers AC §A: one provider, /v1/models discovery
// through the compatibility table, stable prefixed public IDs with intact
// upstream mapping, schema_version 3, model_provider+executor capabilities.
func TestRegisterAndDiscover(t *testing.T) {
	m, f, _, _ := newIntegrationManager(t)

	var reg registrationResult
	decodeResult(t, mustHandle(t, m, "plugin.register", lifecycleRequestBody(integrationYAML("http://unused.invalid"))), &reg)
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if !reg.Capabilities.ModelProvider || !reg.Capabilities.AuthProvider || !reg.Capabilities.Executor {
		t.Fatalf("capabilities = %+v", reg.Capabilities)
	}

	byID := integrationStaticIDs(t, m)
	wantIDs := []string{"opencode-go/glm-5.2", "opencode-go/gpt-5.6-luna", "opencode-go/qwen3.7-max"}
	if len(byID) != len(wantIDs) {
		t.Fatalf("static ids = %v", byID)
	}
	for _, id := range wantIDs {
		if _, ok := byID[id]; !ok {
			t.Fatalf("missing public id %q in %v", id, byID)
		}
	}
	for _, c := range f.callsOf(pluginabi.MethodHostHTTPDo) {
		var wire struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(c.payload, &wire); err != nil {
			t.Fatalf("catalog callback payload: %v", err)
		}
		if strings.HasSuffix(wire.URL, "/models") && wire.Headers.Get("x-opencode-session") != "" {
			t.Fatalf("catalog /models request must not include x-opencode-session: %v", wire.Headers)
		}
	}
	if byID["opencode-go/glm-5.2"].DisplayName != "glm-5.2" {
		t.Fatalf("glm display name = %q, want model ID", byID["opencode-go/glm-5.2"].DisplayName)
	}
}

func TestSessionPromptMarkerNotCapturedInLogs(t *testing.T) {
	m, f, _, _ := newIntegrationManager(t)
	const marker = "unique-session-prompt-marker-7f4c"
	env := mustExecute(t, m, "opencode-go/glm-5.2", "openai", []byte(`{"model":"opencode-go/glm-5.2","messages":[{"role":"user","content":"`+marker+`"}]}`))
	if !env.OK {
		t.Fatalf("execute envelope = %+v", env.Error)
	}
	for _, c := range f.callsOf(pluginabi.MethodHostLog) {
		if strings.Contains(string(c.payload), marker) {
			t.Fatalf("host.log captured raw prompt marker: %s", c.payload)
		}
	}
}

// TestChatNonStreamRoundTrip covers AC §B: a text+tools request reaches the
// upstream /v1/chat/completions with rewritten model and Bearer auth, and
// the non-streaming response (tool_call + usage intact) passes back through.
func TestChatNonStreamRoundTrip(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)

	reqBody := `{"model":"opencode-go/glm-5.2","messages":[{"role":"user","content":"weather in sf?"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather","description":"current weather",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`
	env := mustExecute(t, m, "opencode-go/glm-5.2", "openai", []byte(reqBody))
	if !env.OK {
		t.Fatalf("execute envelope = %+v", env.Error)
	}

	path, auth, upstream := st.chatRequest(t)
	if path != "/v1/chat/completions" {
		t.Fatalf("mock path = %q", path)
	}
	if auth != "Bearer sk-test-1" {
		t.Fatalf("mock Authorization = %q", auth)
	}
	st.mu.Lock()
	chatSession := st.lastChatSession
	st.mu.Unlock()
	if chatSession != "8fa02f28ce729c164657f5ab84cd1e38f690981132765ab45c6a71aeeb3dacd1" {
		t.Fatalf("mock x-opencode-session = %q", chatSession)
	}
	if upstream["model"] != "glm-5.2" {
		t.Fatalf("upstream model = %v, want bare glm-5.2", upstream["model"])
	}
	tools, _ := upstream["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tool definitions lost upstream: %v", upstream["tools"])
	}

	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out.Payload, &completion); err != nil {
		t.Fatalf("payload not valid chat.completion: %v (%s)", err, out.Payload)
	}
	c0 := completion.Choices[0]
	if c0.FinishReason != "tool_calls" || len(c0.Message.ToolCalls) != 1 ||
		c0.Message.ToolCalls[0].Function.Name != "get_weather" ||
		!strings.Contains(c0.Message.ToolCalls[0].Function.Arguments, "sf") {
		t.Fatalf("tool call did not survive round trip: %s", out.Payload)
	}
	if completion.Usage.PromptTokens != 11 || completion.Usage.TotalTokens != 18 {
		t.Fatalf("usage lost: %+v", completion.Usage)
	}

	// AC §A: bare upstream IDs resolve through the same snapshot.
	env = mustExecute(t, m, "glm-5.2", "openai", []byte(reqBody))
	if !env.OK {
		t.Fatalf("bare-id execute envelope = %+v", env.Error)
	}
}

// TestStaleCatalogSurvivesOutage covers AC §A stale-while-unavailable: a
// reconfigure whose refresh hits a catalog outage still succeeds and keeps
// serving the previous snapshot.
func TestStaleCatalogSurvivesOutage(t *testing.T) {
	m, _, st, yamlText := newIntegrationManager(t)

	st.setCatalogDown(true)
	resp := mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(yamlText))
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("reconfigure during outage must succeed (FR-002): %s", resp)
	}
	byID := integrationStaticIDs(t, m)
	for _, id := range []string{"opencode-go/glm-5.2", "opencode-go/gpt-5.6-luna", "opencode-go/qwen3.7-max"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("stale snapshot lost %q: %v", id, byID)
		}
	}

	// Recovery adopts the fresh snapshot again.
	st.setCatalogDown(false)
	resp = mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(yamlText))
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("reconfigure after recovery: %s", resp)
	}
	if _, ok := integrationStaticIDs(t, m)["opencode-go/glm-5.2"]; !ok {
		t.Fatal("recovered catalog lost models")
	}
}

// ---- AC §C + §D non-stream and streaming round trips -------------------

// emittedEvents returns the downstream SSE payloads emitted through the
// fake rawCaller boundary, in order.
func emittedEvents(t *testing.T, f *fakeCaller) []string {
	t.Helper()
	var out []string
	for _, c := range f.callsOf(pluginabi.MethodHostStreamEmit) {
		out = append(out, string(wireBody(t, decodePayload(t, c), "payload")))
	}
	return out
}

func assertCleanStreamClose(t *testing.T, f *fakeCaller) {
	t.Helper()
	if got := len(f.callsOf(pluginabi.MethodHostStreamClose)); got != 1 {
		t.Fatalf("downstream closes = %d, want 1", got)
	}
	if strings.Contains(string(f.callsOf(pluginabi.MethodHostStreamClose)[0].payload), `"error"`) {
		t.Fatalf("clean stream must close without error: %s", f.callsOf(pluginabi.MethodHostStreamClose)[0].payload)
	}
	if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 1 {
		t.Fatalf("upstream closes = %d, want 1", got)
	}
}

// TestMessagesNonStreamRoundTrip covers AC §C: a claude-format request with
// system + tools reaches /v1/messages with x-api-key and anthropic-version
// auth, rewritten model, folded system; the Anthropic response (text +
// tool_use + usage) passes back natively.
func TestMessagesNonStreamRoundTrip(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)

	reqBody := `{"model":"opencode-go/qwen3.7-max","max_tokens":64,"system":"be terse",` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"weather?"}]}],` +
		`"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object"}}]}`
	env := mustExecute(t, m, "opencode-go/qwen3.7-max", "claude", []byte(reqBody))
	if !env.OK {
		t.Fatalf("execute envelope = %+v", env.Error)
	}

	st.mu.Lock()
	auth, version, session, raw := st.lastMessagesAuth, st.lastMessagesVersion, st.lastMessagesSession, st.lastMessagesBody
	st.mu.Unlock()
	if auth != "sk-test-1" {
		t.Fatalf("mock x-api-key = %q", auth)
	}
	if version == "" {
		t.Fatal("anthropic-version header missing")
	}
	if session != "115ff558a2b5fa032b29f2f452673404575e82417d2843e0f410e890c3e85efa" {
		t.Fatalf("mock x-opencode-session = %q", session)
	}
	var upstream map[string]any
	if err := json.Unmarshal(raw, &upstream); err != nil {
		t.Fatalf("mock body: %v", err)
	}
	if upstream["model"] != "qwen3.7-max" {
		t.Fatalf("upstream model = %v", upstream["model"])
	}
	if sys, _ := upstream["system"].(string); !strings.Contains(sys, "be terse") {
		t.Fatalf("system not folded: %v", upstream["system"])
	}
	if tools, _ := upstream["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools lost: %v", upstream["tools"])
	}

	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	var msg struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out.Payload, &msg); err != nil {
		t.Fatalf("payload not valid Messages JSON: %v (%s)", err, out.Payload)
	}
	if msg.StopReason != "end_turn" || msg.Usage.InputTokens != 12 ||
		len(msg.Content) != 2 || msg.Content[0].Text != "hello" ||
		msg.Content[1].Type != "tool_use" || msg.Content[1].Name != "get_weather" ||
		msg.Content[1].Input["city"] != "sf" {
		t.Fatalf("tool_use/usage did not survive: %s", out.Payload)
	}
}

// TestResponsesNonStreamRoundTrip covers AC §D: an openai-response-format
// request reaches /v1/responses with Bearer auth and rewritten model; the
// Responses result (output_text + function_call) passes back natively.
func TestResponsesNonStreamRoundTrip(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)

	reqBody := `{"model":"opencode-go/gpt-5.6-luna",` +
		`"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`
	env := mustExecute(t, m, "opencode-go/gpt-5.6-luna", "openai-response", []byte(reqBody))
	if !env.OK {
		t.Fatalf("execute envelope = %+v", env.Error)
	}

	st.mu.Lock()
	auth, session, raw := st.lastResponsesAuth, st.lastResponsesSession, st.lastResponsesBody
	st.mu.Unlock()
	if auth != "Bearer sk-test-1" {
		t.Fatalf("mock Authorization = %q", auth)
	}
	if session != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("mock x-opencode-session = %q", session)
	}
	var upstream map[string]any
	if err := json.Unmarshal(raw, &upstream); err != nil {
		t.Fatalf("mock body: %v", err)
	}
	if upstream["model"] != "gpt-5.6-luna" {
		t.Fatalf("upstream model = %v", upstream["model"])
	}

	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	var result struct {
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out.Payload, &result); err != nil {
		t.Fatalf("payload not valid Responses JSON: %v (%s)", err, out.Payload)
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q", result.Status)
	}
	var sawText, sawFn bool
	for _, item := range result.Output {
		switch item.Type {
		case "message":
			for _, p := range item.Content {
				if p.Type == "output_text" && p.Text == "hi there" {
					sawText = true
				}
			}
		case "function_call":
			if item.Name == "get_weather" && strings.Contains(item.Arguments, "sf") {
				sawFn = true
			}
		}
	}
	if !sawText || !sawFn {
		t.Fatalf("output items lost: %s", out.Payload)
	}
}

// TestChatStreamingRoundTrip covers AC §B streaming: openai-source SSE
// passes through as chat.completion.chunk events ending with [DONE], both
// streams close cleanly.
func TestChatStreamingRoundTrip(t *testing.T) {
	m, f, st, _ := newIntegrationManager(t)
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBodyForKey("opencode-go/glm-5.2", "openai",
			[]byte(`{"model":"opencode-go/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`), "down-1", "sk-test-1"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("envelope = %s", resp)
	}
	m.bridge.WaitForInFlight(5 * time.Second)

	st.mu.Lock()
	sawStream := strings.Contains(string(st.lastChatBody), `"stream":true`)
	session := st.lastChatSession
	st.mu.Unlock()
	if !sawStream {
		t.Fatal("mock did not see stream:true")
	}
	if session != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("mock x-opencode-session = %q", session)
	}

	events := emittedEvents(t, f)
	joined := strings.Join(events, "")
	if !strings.Contains(joined, `"content":"Hel"`) || !strings.Contains(joined, `"content":"lo"`) {
		t.Fatalf("text deltas missing:\n%s", joined)
	}
	last := ""
	for i := len(events) - 1; i >= 0; i-- {
		if strings.TrimSpace(events[i]) != "" {
			last = strings.TrimSpace(events[i])
			break
		}
	}
	if !strings.Contains(last, `"finish_reason":"stop"`) {
		t.Fatalf("stream must end with finish_reason stop, got %q", last)
	}
	assertCleanStreamClose(t, f)
}

// TestMessagesStreamingRoundTrip covers AC §C streaming: claude-source
// anthropic SSE events pass through verbatim in order and terminate.
func TestMessagesStreamingRoundTrip(t *testing.T) {
	m, f, st, _ := newIntegrationManager(t)
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBodyForKey("opencode-go/qwen3.7-max", "claude",
			[]byte(`{"model":"opencode-go/qwen3.7-max","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`), "down-2", "sk-test-1"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("envelope = %s", resp)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	st.mu.Lock()
	sawStream := strings.Contains(string(st.lastMessagesBody), `"stream":true`)
	session := st.lastMessagesSession
	st.mu.Unlock()
	if !sawStream {
		t.Fatal("mock did not see stream:true")
	}
	if session != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("mock x-opencode-session = %q", session)
	}

	events := emittedEvents(t, f)
	joined := strings.Join(events, "")
	start, delta, stop := strings.Index(joined, "message_start"), strings.Index(joined, "content_block_delta"), strings.Index(joined, "message_stop")
	if start < 0 || delta < start || stop < delta {
		t.Fatalf("anthropic event sequence out of order:\n%s", joined)
	}
	assertCleanStreamClose(t, f)
}

// TestResponsesStreamingRoundTrip covers AC §D streaming: openai-response
// native frames pass through and terminate on response.completed.
func TestResponsesStreamingRoundTrip(t *testing.T) {
	m, f, st, _ := newIntegrationManager(t)
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBodyForKey("gpt-5.6-luna", "openai-response",
			[]byte(`{"model":"gpt-5.6-luna","input":"hi","stream":true}`), "down-3", "sk-test-1"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("envelope = %s", resp)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	st.mu.Lock()
	sawStream := strings.Contains(string(st.lastResponsesBody), `"stream":true`)
	session := st.lastResponsesSession
	st.mu.Unlock()
	if !sawStream {
		t.Fatal("mock did not see stream:true")
	}
	if session != "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4" {
		t.Fatalf("mock x-opencode-session = %q", session)
	}

	events := emittedEvents(t, f)
	joined := strings.Join(events, "")
	created, completed := strings.Index(joined, "response.created"), strings.Index(joined, "response.completed")
	if created < 0 || completed < created {
		t.Fatalf("responses event sequence wrong:\n%s", joined)
	}
	assertCleanStreamClose(t, f)
}

// ---- AC §A/§B/§C/§D remaining boxes ------------------------------------

func TestPrefixDisabledE2E(t *testing.T) {
	m, _, _, yamlText := newIntegrationManager(t)
	resp := mustHandle(t, m, "plugin.reconfigure",
		lifecycleRequestBody(yamlText+"model-prefix:\n  enabled: false\n"))
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("reconfigure: %s", resp)
	}
	byID := integrationStaticIDs(t, m)
	for _, id := range []string{"glm-5.2", "gpt-5.6-luna", "qwen3.7-max"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("bare id %q missing: %v", id, byID)
		}
	}
	if len(byID) != 3 {
		t.Fatalf("unexpected ids: %v", byID)
	}
}

func TestUnroutableModelExcludedWithDiagnostic(t *testing.T) {
	m, f, st, yamlText := newIntegrationManager(t)
	st.setCatalogBody(`{"data":[
		{"id":"glm-5.2"},
		{"id":"glm-5.2"},
		{"id":"weird-unknown-9x"}
	]}`)
	resp := mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(yamlText))
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("reconfigure: %s", resp)
	}
	byID := integrationStaticIDs(t, m)
	if _, ok := byID["opencode-go/weird-unknown-9x"]; ok {
		t.Fatal("unroutable model must be excluded from static ids")
	}
	if _, ok := byID["opencode-go/glm-5.2"]; !ok {
		t.Fatal("routable model missing")
	}
	blob := logBlobOf(f)
	if !strings.Contains(blob, "unsupported models excluded") || !strings.Contains(blob, "weird-unknown-9x") {
		t.Fatalf("diagnostic log missing: %s", blob)
	}
	if !strings.Contains(blob, `"warnings"`) || !strings.Contains(blob, "duplicate model") {
		t.Fatalf("warnings field missing from diagnostic: %s", blob)
	}
}

// TestExecuteNativePassthroughMalformedUpstream covers the convert-error
// return on the non-stream path: a native pairing whose upstream body is
// not valid JSON fails as a translation error.
func TestExecuteNativePassthroughMalformedUpstream(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)
	st.setMessagesBody("{definitely not json")
	env := mustExecute(t, m, "opencode-go/qwen3.7-max", "claude",
		[]byte(`{"model":"opencode-go/qwen3.7-max","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	if env.OK || env.Error == nil || env.Error.Code != "translation_failure" {
		t.Fatalf("envelope = %+v", env.Error)
	}
}

func TestDuplicateCatalogEntriesDeduped(t *testing.T) {
	m, _, st, yamlText := newIntegrationManager(t)
	st.setCatalogBody(`{"data":[
		{"id":"glm-5.2"},
		{"id":"glm-5.2"}
	]}`)
	resp := mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(yamlText))
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("reconfigure: %s", resp)
	}
	byID := integrationStaticIDs(t, m)
	if _, ok := byID["opencode-go/glm-5.2"]; !ok {
		t.Fatalf("glm-5.2 missing: %v", byID)
	}
	if len(byID) != 1 {
		t.Fatalf("duplicates created extra public models: %v", byID)
	}
}

func TestToolResultRoundTrip(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)
	reqBody := `{"model":"opencode-go/qwen3.7-max","max_tokens":64,"messages":[
		{"role":"user","content":[{"type":"text","text":"weather?"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"sf"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"sunny"}]}]}
	]}`
	env := mustExecute(t, m, "opencode-go/qwen3.7-max", "claude", []byte(reqBody))
	if !env.OK {
		t.Fatalf("execute envelope = %+v", env.Error)
	}
	st.mu.Lock()
	raw := st.lastMessagesBody
	st.mu.Unlock()
	var upstream struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Name      string `json:"name"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &upstream); err != nil {
		t.Fatalf("mock body: %v (%s)", err, raw)
	}
	if len(upstream.Messages) != 3 {
		t.Fatalf("messages = %d, want 3-turn history preserved", len(upstream.Messages))
	}
	asst, user := upstream.Messages[1], upstream.Messages[2]
	if asst.Role != "assistant" || len(asst.Content) != 1 ||
		asst.Content[0].Type != "tool_use" || asst.Content[0].Name != "get_weather" {
		t.Fatalf("assistant tool_use block lost: %+v", asst)
	}
	if user.Role != "user" || len(user.Content) != 1 ||
		user.Content[0].Type != "tool_result" || user.Content[0].ToolUseID != "tu_1" {
		t.Fatalf("user tool_result block lost: %+v", user)
	}
}

func TestThinkingBlocksPolicy(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)
	st.setMessagesBody(`{"id":"msg_2","role":"assistant","model":"qwen3.7-max",` +
		`"content":[{"type":"thinking","thinking":"weigh options","signature":"sig123"},` +
		`{"type":"text","text":"answer"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`)
	env := mustExecute(t, m, "opencode-go/qwen3.7-max", "claude",
		[]byte(`{"model":"opencode-go/qwen3.7-max","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	if !env.OK {
		t.Fatalf("execute envelope = %+v", env.Error)
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	// Documented policy: thinking blocks are preserved verbatim when the
	// destination protocol supports them (claude source does).
	if !strings.Contains(string(out.Payload), `"type":"thinking"`) ||
		!strings.Contains(string(out.Payload), `"signature":"sig123"`) {
		t.Fatalf("thinking block not preserved: %s", out.Payload)
	}
}

func TestReasoningControlsExplicit(t *testing.T) {
	t.Run("openai reasoning_effort passthrough", func(t *testing.T) {
		m, _, st, _ := newIntegrationManager(t)
		env := mustExecute(t, m, "opencode-go/glm-5.2", "openai",
			[]byte(`{"model":"opencode-go/glm-5.2","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`))
		if !env.OK {
			t.Fatalf("execute envelope = %+v", env.Error)
		}
		st.mu.Lock()
		raw := st.lastChatBody
		st.mu.Unlock()
		var upstream map[string]any
		if err := json.Unmarshal(raw, &upstream); err != nil {
			t.Fatalf("mock body: %v", err)
		}
		if upstream["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %v, want high carried upstream", upstream["reasoning_effort"])
		}
	})
}

func TestResponsesUnsupportedFeatureDescriptive(t *testing.T) {
	m, _, _, _ := newIntegrationManager(t)
	// Cross-format client (openai source) brings a non-function tool to the
	// Responses route; the adapter must reject it descriptively instead of
	// silently dropping it or emitting malformed output.
	reqBody := `{"model":"opencode-go/gpt-5.6-luna",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search"}]}`
	resp, err := m.HandleCall("executor.execute", execReqBody("opencode-go/gpt-5.6-luna", "openai", []byte(reqBody), false))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != "unsupported_protocol_or_parameter" {
		t.Fatalf("envelope = %s", resp)
	}
	if !strings.Contains(env.Error.Message, "web_search") || !strings.Contains(env.Error.Message, "/v1/responses") {
		t.Fatalf("message not descriptive: %q", env.Error.Message)
	}
}

// ---- AC §E upstream errors + §F security, end to end -------------------

func integrationTwoKeyYAML(baseURL string) string {
	return fmt.Sprintf("base-url: %s/v1\nallow-http: true\napi-keys:\n  - value: sk-test-1\n  - value: sk-test-2\n", baseURL)
}

func newTwoKeyManager(t *testing.T) (*Manager, *fakeCaller, *mockOpenCode) {
	t.Helper()
	st := newMockOpenCode(t)
	f := &fakeCaller{responder: newForwardingCaller()}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(integrationTwoKeyYAML(st.srv.URL)))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("register failed: %s", resp)
	}
	return m, f, st
}

// TestNoKeyLeakageE2E covers AC §F: across upstream error handling the raw
// key material appears ONLY inside Authorization header values of
// host.http.do payloads — never in host.log payloads, error envelopes, or
// anywhere else.
func TestNoKeyLeakageE2E(t *testing.T) {
	m, f, st := newTwoKeyManager(t)
	st.setChatPlan(func(hit int) (int, string, string) {
		if hit == 1 {
			return http.StatusInternalServerError, "", "boom"
		}
		return http.StatusOK, "", integrationCompletion
	})

	failResp, err := m.HandleCall("executor.execute", execReqBodyWithKey("opencode-go/glm-5.2", "openai", []byte(ccRequestBody), false, "sk-test-1"))
	if err != nil {
		t.Fatalf("failing execute: %v", err)
	}
	okResp, err := m.HandleCall("executor.execute", execReqBodyWithKey("opencode-go/glm-5.2", "openai", []byte(ccRequestBody), false, "sk-test-2"))
	if err != nil {
		t.Fatalf("succeeding execute: %v", err)
	}
	if env := decodeEnv(t, okResp); !env.OK {
		t.Fatalf("second CPA-selected key call should succeed: %+v", env.Error)
	}

	keys := []string{"sk-test-1", "sk-test-2"}
	for _, c := range f.recorded() {
		text := string(c.payload)
		switch c.method {
		case pluginabi.MethodHostLog:
			for _, k := range keys {
				if strings.Contains(text, k) {
					t.Fatalf("host.log leaks key material: %s", text)
				}
			}
		case pluginabi.MethodHostHTTPDo:
			var wire map[string]any
			if err := json.Unmarshal(c.payload, &wire); err != nil {
				t.Fatalf("do payload: %v", err)
			}
			headers, _ := wire["headers"].(map[string]any)
			auth, _ := headers["Authorization"].([]any)
			authVal, _ := auth[0].(string)
			foundInHeaders := strings.Contains(authVal, "sk-test-")
			delete(wire, "headers")
			rest, _ := json.Marshal(wire)
			for _, k := range keys {
				if strings.Contains(string(rest), k) {
					t.Fatalf("key outside headers in host.http.do payload: %s", rest)
				}
				if !foundInHeaders && strings.Contains(text, k) {
					t.Fatalf("key outside Authorization header: %s", text)
				}
			}
		}
	}
	for name, respBytes := range map[string][]byte{"failing": failResp, "ok": okResp} {
		for _, k := range keys {
			if strings.Contains(string(respBytes), k) {
				t.Fatalf("%s envelope leaks key %q: %s", name, k, respBytes)
			}
		}
		if strings.Contains(string(respBytes), "Bearer") {
			t.Fatalf("%s envelope echoes Authorization material: %s", name, respBytes)
		}
	}
}

// TestHTTPSDefaultEnforced covers AC §F: http base URLs require an explicit
// allow-http, the invalid-config error names base-url without leaking the
// key, and the shipped default endpoint is https.
func TestHTTPSDefaultEnforced(t *testing.T) {
	st := newMockOpenCode(t)
	f := &fakeCaller{responder: newForwardingCaller()}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	httpYAML := fmt.Sprintf("base-url: %s/v1\napi-keys:\n  - value: sk-test-1\n", st.srv.URL)
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(httpYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != "invalid_config" || !strings.Contains(env.Error.Message, "base-url") {
		t.Fatalf("envelope = %s", resp)
	}
	if strings.Contains(env.Error.Message, "sk-test-1") {
		t.Fatalf("invalid_config leaks key: %q", env.Error.Message)
	}

	resp, err = m.HandleCall("plugin.register", lifecycleRequestBody(httpYAML+"allow-http: true\n"))
	if err != nil {
		t.Fatalf("register with allow-http: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("allow-http register rejected: %s", resp)
	}

	// Config now mandates at least one key; supply a throwaway key so the
	// unset-field defaults (https base-url) still apply.
	cfg, err := config.Load([]byte(dummyKeyYAML))
	if err != nil {
		t.Fatalf("default config load: %v", err)
	}
	if !strings.HasPrefix(cfg.BaseURL, "https://") {
		t.Fatalf("default base-url = %q, want https default", cfg.BaseURL)
	}
}

// TestRouteOverrideEndpointReachesUpstream pins that a user route override,
// not the route default, determines the upstream URL (arch §5 route priority,
// FR-004).
func TestRouteOverrideEndpointReachesUpstream(t *testing.T) {
	m, _, st, yamlText := newIntegrationManager(t)
	st.setCatalogBody(`{"data":[{"id":"totally-new-x"}]}`)
	yamlText += "route-overrides:\n  totally-new-x:\n    protocol: messages\n    endpoint: /v1/messages-override\n"
	resp := mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(yamlText))
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("reconfigure: %s", resp)
	}
	env := mustExecute(t, m, "opencode-go/totally-new-x", "claude",
		[]byte(`{"model":"opencode-go/totally-new-x","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	if !env.OK {
		t.Fatalf("execute envelope = %+v", env.Error)
	}
	st.mu.Lock()
	auth, raw := st.lastMessagesAuth, st.lastMessagesBody
	st.mu.Unlock()
	if auth != "sk-test-1" {
		t.Fatalf("override endpoint lost messages auth: %q", auth)
	}
	var upstream map[string]any
	if err := json.Unmarshal(raw, &upstream); err != nil {
		t.Fatalf("mock body: %v (%s)", err, raw)
	}
	if upstream["model"] != "totally-new-x" {
		t.Fatalf("upstream model = %v", upstream["model"])
	}
}

// ---- cross-format non-stream E2E (M4-completion, AC §C/§D) -------------

const e2eAnthropicToolUse = `{"id":"msg_e2e","type":"message","role":"assistant","model":"qwen3.7-max",` +
	`"content":[{"type":"tool_use","id":"tu_e2e","name":"get_weather","input":{"city":"sf"}}],` +
	`"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":9}}`

const e2eLunaFunctionCall = `{"id":"resp_e2e","object":"response","status":"completed","model":"gpt-5.6-luna",` +
	`"output":[{"type":"function_call","call_id":"call_9","name":"get_weather","arguments":"{\"city\":\"sf\"}"}],` +
	`"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`

// TestE2EOpenAIClientToMessagesRoute: an openai-format client reaches the
// Messages route; the anthropic tool_use response translates into a
// chat.completion with indexed tool_calls and mapped usage.
func TestE2EOpenAIClientToMessagesRoute(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)
	st.setMessagesBody(e2eAnthropicToolUse)

	env := mustExecute(t, m, "opencode-go/qwen3.7-max", "openai",
		[]byte(`{"model":"opencode-go/qwen3.7-max","messages":[{"role":"user","content":"weather?"}]}`))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}

	st.mu.Lock()
	auth := st.lastMessagesAuth
	rawUpstream := append([]byte(nil), st.lastMessagesBody...)
	st.mu.Unlock()

	var upstream struct {
		Model string `json:"model"`
	}
	json.Unmarshal(rawUpstream, &upstream)

	var cc struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out.Payload, &cc); err != nil {
		t.Fatalf("payload not chat.completion: %v (%s)", err, out.Payload)
	}
	c0 := cc.Choices[0]
	if cc.Object != "chat.completion" || cc.Model != "qwen3.7-max" ||
		c0.FinishReason != "tool_calls" || len(c0.Message.ToolCalls) != 1 ||
		c0.Message.ToolCalls[0].ID != "tu_e2e" ||
		c0.Message.ToolCalls[0].Function.Name != "get_weather" ||
		!strings.Contains(c0.Message.ToolCalls[0].Function.Arguments, `"sf"`) {
		t.Fatalf("tool_use translation wrong: %s", out.Payload)
	}
	if cc.Usage.PromptTokens != 12 || cc.Usage.CompletionTokens != 9 {
		t.Fatalf("usage mapping wrong: %+v", cc.Usage)
	}
	if auth != "sk-test-1" || upstream.Model != "qwen3.7-max" {
		t.Fatalf("upstream auth=%q model=%q", auth, upstream.Model)
	}
}

// TestE2EResponsesClientToMessagesRoute: an openai-response client on the
// Messages route receives a Responses result synthesized from the
// anthropic tool_use response.
func TestE2EResponsesClientToMessagesRoute(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)
	st.setMessagesBody(e2eAnthropicToolUse)

	env := mustExecute(t, m, "opencode-go/qwen3.7-max", "openai-response",
		[]byte(`{"model":"opencode-go/qwen3.7-max","input":"weather?"}`))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	if err := json.Unmarshal(out.Payload, &resp); err != nil {
		t.Fatalf("not Responses result: %v (%s)", err, out.Payload)
	}
	if resp.Object != "response" || resp.Status != "completed" ||
		len(resp.Output) != 1 || resp.Output[0].Type != "function_call" ||
		resp.Output[0].CallID != "tu_e2e" || resp.Output[0].Name != "get_weather" ||
		!strings.Contains(resp.Output[0].Arguments, `"sf"`) {
		t.Fatalf("function_call synthesis wrong: %s", out.Payload)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 9 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
}

// TestE2EOpenAIClientToResponsesRoute: an openai-format client on the
// Responses route receives a chat.completion built from the function_call
// result.
func TestE2EOpenAIClientToResponsesRoute(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)
	st.setResponsesBody(e2eLunaFunctionCall)

	env := mustExecute(t, m, "gpt-5.6-luna", "openai",
		[]byte(`{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"weather?"}]}`))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	var cc struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	if err := json.Unmarshal(out.Payload, &cc); err != nil {
		t.Fatalf("payload not chat.completion: %v (%s)", err, out.Payload)
	}
	tc := cc.Choices[0].Message.ToolCalls
	if cc.Model != "gpt-5.6-luna" || cc.Choices[0].FinishReason != "tool_calls" ||
		len(tc) != 1 || tc[0].ID != "call_9" || tc[0].Function.Name != "get_weather" ||
		!strings.Contains(tc[0].Function.Arguments, `"sf"`) {
		t.Fatalf("function_call translation wrong: %s", out.Payload)
	}
	if cc.Usage.PromptTokens != 3 || cc.Usage.CompletionTokens != 4 || cc.Usage.TotalTokens != 7 {
		t.Fatalf("usage wrong: %+v", cc.Usage)
	}
	st.mu.Lock()
	auth := st.lastResponsesAuth
	st.mu.Unlock()
	if auth != "Bearer sk-test-1" {
		t.Fatalf("responses auth = %q", auth)
	}
}

// TestE2EClaudeClientToResponsesRoute: a claude-format client on the
// Responses route receives a Messages-shaped response with mapped usage.
func TestE2EClaudeClientToResponsesRoute(t *testing.T) {
	m, _, st, _ := newIntegrationManager(t)

	env := mustExecute(t, m, "gpt-5.6-luna", "claude",
		[]byte(`{"model":"opencode-go/gpt-5.6-luna","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	var msg struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	if err := json.Unmarshal(out.Payload, &msg); err != nil {
		t.Fatalf("not Messages shape: %v (%s)", err, out.Payload)
	}
	if msg.Type != "message" || msg.Role != "assistant" || msg.Model != "gpt-5.6-luna" ||
		msg.StopReason != "tool_use" || len(msg.Content) != 2 ||
		msg.Content[0].Type != "text" || msg.Content[0].Text != "hi there" ||
		msg.Content[1].Type != "tool_use" || msg.Content[1].Name != "get_weather" {
		t.Fatalf("claude shape wrong: %s", out.Payload)
	}
	if msg.Usage.InputTokens != 5 || msg.Usage.OutputTokens != 6 {
		t.Fatalf("usage wrong: %+v", msg.Usage)
	}
	st.mu.Lock()
	sawResponsesRequest := strings.Contains(string(st.lastResponsesBody), `"input"`)
	st.mu.Unlock()
	if !sawResponsesRequest {
		t.Fatal("upstream did not receive a Responses-format request")
	}
}
