// Non-stream and stream execution paths (FR-005/FR-006/FR-007, arch §4/§5
// steps 5-8): resolve the public model ID against the catalog snapshot,
// translate the inbound payload to the record's upstream protocol, call
// upstream through the host HTTP callbacks, and translate back to the
// client protocol. Request authentication is selected by CPA.

package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/chatcompletions"
	"opencode-go-cliproxyapi/internal/adapter/messages"
	"opencode-go-cliproxyapi/internal/adapter/responses"
	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/config"
	"opencode-go-cliproxyapi/internal/errclass"
)

// executorRequest mirrors rpcExecutorRequest: the SDK embeds
// pluginapi.ExecutorRequest untagged, so its fields marshal under Go field
// names ("Model", "SourceFormat", "OriginalRequest", "Stream"). StreamID is
// the DOWNSTREAM host-allocated id for executor.execute_stream emissions;
// ids returned by DoStream are UPSTREAM and never interchangeable (§4).
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

// resolvedExecution carries everything both execution paths need after
// model/key resolution succeeded.
type resolvedExecution struct {
	cfg config.Config
	rec catalog.ModelRecord
	key string
}

// resolveExecution resolves the requested model against the snapshot and
// extracts the key selected by CPA. A non-nil second return is a ready-made
// failure envelope.
func (m *Manager) resolveExecution(req executorRequest) (*resolvedExecution, []byte) {
	m.mu.RLock()
	cfg, mgr := m.cfg, m.mgr
	m.mu.RUnlock()
	if req.AuthProvider != ProviderID {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth provider is not opencode-go"})
	}
	key := strings.TrimSpace(req.AuthAttributes["api_key"])
	debugTrace("executor auth model=%s auth_id=%s provider=%s attr_api_key_present=%t attr_count=%d storage_json_bytes=%d", req.Model, req.AuthID, req.AuthProvider, key != "", len(req.AuthAttributes), len(req.StorageJSON))
	if key == "" {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth has no api key"})
	}
	var rec catalog.ModelRecord
	var found bool
	if mgr != nil && req.Model != "" {
		rec, found = mgr.Lookup(req.Model)
	}
	if !found {
		return nil, classEnvelope(&errclass.Error{
			Class:      errclass.ClassInvalidModel,
			Message:    "model not in routable catalog",
			StatusCode: http.StatusNotFound,
		})
	}
	return &resolvedExecution{cfg: cfg, rec: rec, key: key}, nil
}

// handleExecute implements executor.execute (non-stream). Stream-flagged
// requests are routed to the stream path instead of rejected.
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	if req.Stream {
		return m.executeStream(req)
	}
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveOpenCodeSessionID(req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	debugTrace("executor session mode=%s source_format=%s x_opencode_session=%s fallback=%t", "non-stream", req.SourceFormat, sessionID, sessionID == emptyOpenCodeSessionID)
	upstreamBody, eErr := buildUpstreamRequest(res.rec.Protocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	url := catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath)
	debugTrace("executor resolved public_model=%s upstream_model=%s route=%s url=%s key_count=%d", req.Model, res.rec.UpstreamID, res.rec.Protocol, url, len(res.cfg.APIKeys))
	debugTrace("executor sending non-stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(res.rec.Protocol, res.key, sessionID),
		Body:    upstreamBody,
	})
	if err != nil {
		debugTrace("executor non-stream network error: %v", err)
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	debugTrace("executor received non-stream status=%d body_len=%d", resp.StatusCode, len(resp.Body))
	if resp.StatusCode >= 400 {
		return classEnvelope(shared.UpstreamStatusError(resp.StatusCode, resp.Body)), nil
	}
	// Parse/envelope guard only; true OOM prevention belongs to the host transport's byte cap.
	if int64(len(resp.Body)) > res.cfg.MaxResponseBytes {
		return classEnvelope(errclass.Translation("response exceeds max-response-bytes")), nil
	}
	converted, eErr := convertNonStream(res.rec.Protocol, req.SourceFormat, resp.StatusCode, resp.Body)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: converted, Headers: resp.Headers}), nil
}

func buildUpstreamRequest(route catalog.Route, upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	case catalog.RouteMessages:
		return messages.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	case catalog.RouteResponses:
		return responses.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	}
	return nil, errclass.Translation("unsupported route")
}

func upstreamAuthHeaders(route catalog.Route, key, sessionID string) http.Header {
	var h http.Header
	if route == catalog.RouteMessages {
		h = messages.AuthHeaders(key)
	} else {
		// Chat Completions and Responses endpoints are OpenAI-style bearer.
		h = chatcompletions.AuthHeaders(key)
	}
	h.Set("x-opencode-session", sessionID)
	return h
}

const emptyOpenCodeSessionID = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// resolveOpenCodeSessionID applies the FR-012/AC-H precedence: CPA's canonical
// identity, an explicit inbound session header, then the existing content hash.
func resolveOpenCodeSessionID(req executorRequest) (string, *errclass.Error) {
	if sid, ok := req.Metadata["canonical_session_id"].(string); ok && sid != "" {
		return sid, nil
	}
	for _, name := range []string{
		"X-Session-Affinity",
		"X-Opencode-Session",
		"X-Session-Id",
		"X-Claude-Code-Session-Id",
		"Session-Id",
	} {
		if sid := req.Headers.Get(name); sid != "" {
			return sid, nil
		}
	}
	return deriveOpenCodeSessionID(req.SourceFormat, req.OriginalRequest)
}

// deriveOpenCodeSessionID hashes the model-visible content of the initial user
// turn before translation (FR-012/AC-H). Metadata is excluded;
// valid requests without a user turn use the fixed empty-input digest.
func deriveOpenCodeSessionID(sourceFormat string, originalRequest []byte) (string, *errclass.Error) {
	var content strings.Builder
	appendParts := func(raw json.RawMessage, target string) *errclass.Error {
		parts, eErr := shared.DecodeStringOrParts(raw, target)
		if eErr != nil {
			return eErr
		}
		for _, part := range parts {
			if part.ImageURL != "" {
				content.WriteString(part.ImageURL)
			} else {
				content.WriteString(part.Text)
			}
		}
		return nil
	}

	switch sourceFormat {
	case "openai":
		var req shared.ChatCompletionsRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai request JSON: " + err.Error())
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role == "system" || msg.Role == "developer" {
					continue
				}
				if msg.Role != "user" {
					break
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if eErr := appendParts(msg.Content, "/v1/chat/completions"); eErr != nil {
				return "", eErr
			}
		}
	case "claude":
		req, eErr := shared.DecodeClaudeMessages(originalRequest)
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role != "user" {
					continue
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if msg.Content != "" {
				content.WriteString(msg.Content)
			}
			for _, block := range msg.Blocks {
				switch block.Kind {
				case "text":
					content.WriteString(block.Text)
				case "image":
					content.WriteString(block.URL)
				case "tool_result":
					text, eErr := shared.ToolResultText(block.Result, "tool messages carry text only")
					if eErr != nil {
						return "", eErr
					}
					content.WriteString(text)
				}
			}
		}
	case "openai-response":
		var req shared.ResponsesRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai-response request JSON: " + err.Error())
		}
		items, eErr := req.DecodeInputItems()
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, item := range items {
			isUserMessage := item.Role == "user" && (item.Type == "message" || item.Type == "")
			if !started {
				if !isUserMessage {
					continue
				}
				started = true
			} else if !isUserMessage {
				break
			}
			if eErr := appendParts(item.Content, "/v1/responses"); eErr != nil {
				return "", eErr
			}
		}
	default:
		return "", shared.UnsupportedFormat(sourceFormat, "OpenCode Go session derivation")
	}

	digest := sha256.Sum256([]byte(content.String()))
	return hex.EncodeToString(digest[:]), nil
}

// convertNonStream routes one upstream response to its adapter's uniform
// translator: every adapter owns status classification (>=400 → §7
// classified errors), native passthrough, and cross-format conversion for
// all client formats.
func convertNonStream(route catalog.Route, sourceFormat string, status int, body []byte) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.ConvertNonStreamResponse(sourceFormat, status, body)
	case catalog.RouteMessages:
		return messages.ConvertNonStreamResponse(sourceFormat, status, body)
	case catalog.RouteResponses:
		return responses.ConvertNonStreamResponse(sourceFormat, status, body)
	}
	return nil, errclass.Translation("unsupported route")
}

// classEnvelope renders a classified failure as the wire error envelope;
// ToEnvelopeError redacts and propagates Retryable/HTTPStatus host-side.
func classEnvelope(e *errclass.Error) []byte {
	wire := errclass.ToEnvelopeError(e)
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &wire})
	return out
}

// streamConverter is the common shape of the three adapters' stream
// converters: feed one upstream SSE chunk, get translated client events.
type streamConverter interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
}

// Compile-time proof the close-without-terminal Flush seam (F5) picks up
// both converters that can hold a deferred terminal.
var (
	_ interface{ Flush() [][]byte } = (*chatcompletions.StreamConverter)(nil)
	_ interface{ Flush() [][]byte } = (*messages.StreamConverter)(nil)
)

func newStreamConverter(route catalog.Route, sourceFormat string) streamConverter {
	switch route {
	case catalog.RouteMessages:
		return messages.NewStreamConverter(sourceFormat)
	case catalog.RouteResponses:
		return responses.NewStreamConverter(sourceFormat)
	}
	return chatcompletions.NewStreamConverter(sourceFormat)
}

// handleExecuteStream implements executor.execute_stream (FR-006, §7).
// It decodes once and delegates to executeStream so a stream-flagged
// request arriving via executor.execute is never parsed twice.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor stream invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	return m.executeStream(req)
}

// executeStream runs the already-decoded stream execution: pre-first-byte errors
// (invalid request, unroutable model, upstream HTTP >=400) return immediately as
// error envelopes so CPA can failover pre-emission.
// On success (upstream HTTP 200 OK), the reading and emitting pump loop runs in
// a background goroutine and executeStream returns okEnvelope immediately so the
// host can start draining chunks to the downstream client without buffer deadlocks.
func (m *Manager) executeStream(req executorRequest) ([]byte, error) {
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveOpenCodeSessionID(req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	debugTrace("executor session mode=%s source_format=%s x_opencode_session=%s fallback=%t", "stream", req.SourceFormat, sessionID, sessionID == emptyOpenCodeSessionID)
	upstreamBody, eErr := buildUpstreamRequest(res.rec.Protocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	url := catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath)
	debugTrace("executor sending stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	st, _, id, err := m.bridge.DoStream(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(res.rec.Protocol, res.key, sessionID),
		Body:    upstreamBody,
	})
	debugTrace("executor stream DoStream status=%d upstreamID=%s err=%v", st, id, err)
	if err != nil {
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	if st >= 400 {
		_ = m.bridge.StreamClose(id)
		return classEnvelope(errclass.FromStatus(st, "")), nil
	}

	downID := req.StreamID
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
	}
	go func() {
		if m.bridge != nil {
			defer m.bridge.inFlight.Done()
		}
		m.pumpStream(downID, id, res, req.SourceFormat)
	}()
	return okEnvelope(struct{}{}), nil
}

func (m *Manager) pumpStream(downID, upstreamID string, res *resolvedExecution, sourceFormat string) {
	var closeOnce sync.Once
	closeStreams := func(downErrMsg string) {
		closeOnce.Do(func() {
			_ = m.bridge.StreamClose(upstreamID)
			_ = m.bridge.StreamCloseDownstream(downID, downErrMsg)
		})
	}
	defer closeStreams("")

	var aborted atomic.Bool
	watchdog := time.AfterFunc(res.cfg.RequestTimeout, func() {
		defer func() {
			if r := recover(); r != nil && m.bridge != nil {
				_ = m.bridge.Log("error", "stream watchdog panicked", nil)
			}
		}()
		aborted.Store(true)
		_ = m.bridge.StreamClose(upstreamID)
	})
	defer watchdog.Stop()

	conv := newStreamConverter(res.rec.Protocol, sourceFormat)
	var (
		total          int64
		upstreamClosed bool
		convDone       bool
	)
	for {
		payload, readErrMsg, closed, err := m.bridge.StreamRead(upstreamID)
		debugTrace("executor stream read chunk_len=%d closed=%t readErrMsg=%q err=%v", len(payload), closed, readErrMsg, err)
		upstreamClosed = closed
		if aborted.Load() {
			closeStreams(errclass.Redact("stream exceeded request-timeout"))
			return
		}
		if err != nil {
			closeStreams(errclass.Redact(err.Error()))
			return
		}
		if readErrMsg != "" {
			closeStreams(errclass.Redact(readErrMsg))
			return
		}
		total += int64(len(payload))
		if total > res.cfg.MaxResponseBytes {
			closeStreams(errclass.Redact("stream exceeded max-response-bytes"))
			return
		}
		events, done, convErr := conv.Feed(payload)
		if convErr != nil {
			closeStreams(errclass.Redact(convErr.Message))
			return
		}
		if emitErr := m.emitAll(downID, events); emitErr != nil {
			closeStreams(errclass.Redact(emitErr.Error()))
			return
		}
		convDone = done
		if convDone || upstreamClosed {
			break
		}
	}
	debugTrace("executor stream loop end convDone=%t upstreamClosed=%t", convDone, upstreamClosed)
	if !convDone && upstreamClosed {
		if flusher, ok := conv.(interface{ Flush() [][]byte }); ok {
			flushed := flusher.Flush()
			if emitErr := m.emitAll(downID, flushed); emitErr != nil {
				closeStreams(errclass.Redact(emitErr.Error()))
				return
			}
		}
	}
}

// emitAll feeds converted events downstream in order. StreamEmit is
// deadline-bounded (emitTimeout), so a host that stops draining the
// downstream stream surfaces here as an error; the pump loop treats that
// as a post-first-byte stream-fatal failure and runs fail(), whose
// Once-closer releases both streams.
func (m *Manager) emitAll(downStreamID string, events [][]byte) error {
	for _, evt := range events {
		if err := m.bridge.StreamEmit(downStreamID, evt); err != nil {
			return err
		}
	}
	return nil
}
