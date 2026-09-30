// Package plugin implements the CLIProxyAPI method dispatcher for the
// opencode-go provider (Milestone 3): registration, reconfiguration,
// model publication, catalog refresh scheduling, and host-callback bridging.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// RawCaller performs one host callback round-trip: method name plus JSON
// request bytes in, JSON envelope bytes out.
type RawCaller func(method string, payload []byte) ([]byte, error)

// HostBridge routes outbound traffic through the host callbacks
// "host.http.do", "host.http.do_stream", "host.log", and the stream
// helpers. Callers must never place API keys, auth headers, prompts, or
// tool args in Log messages or fields.
type HostBridge struct {
	call RawCaller
	// inFlight counts host-callback invocations whose goroutines may still
	// run — including callbacks orphaned past their deadline and the
	// abandon/drain cleanup spawned for them. handleShutdown waits on it
	// because Unix loaders free host_api and dlclose the plugin as soon as
	// the shutdown export returns; any goroutine still calling into the
	// host after that would crash the process.
	inFlight sync.WaitGroup
}

// NewHostBridge wraps the injected raw host caller so the bridge satisfies
// catalog.HostClient. main.go wires it to the C host API after init;
// tests inject fakes.
func NewHostBridge(call RawCaller) *HostBridge {
	return &HostBridge{call: call}
}

// hostHTTPReq is the wire shape accepted by host.http.do / do_stream.
type hostHTTPReq struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

// hostLogReq is the wire shape accepted by host.log.
type hostLogReq struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields"`
}

// AuthSave persists one plugin-owned auth record through the host. The
// credential is deliberately accepted only as JSON here; callers must not put
// it in logs or error text.
func (b *HostBridge) AuthSave(ctx context.Context, req pluginapi.HostAuthSaveRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("host auth save failed: invalid request")
	}
	raw, err := b.callWithTimeout(ctx, pluginabi.MethodHostAuthSave, payload)
	if err != nil {
		if strings.Contains(err.Error(), "timed out") {
			return fmt.Errorf("host auth save failed: timed out")
		}
		return fmt.Errorf("host auth save failed")
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return fmt.Errorf("host auth save failed: %w", err)
	}
	if !env.OK {
		// Host error text is not trusted and may echo credential JSON.
		return fmt.Errorf("host auth save failed")
	}
	return nil
}

// AuthList returns the auth records currently known by CPA without modifying
// their files. Callers use stable names/IDs to avoid overwriting CPA-managed
// metadata during plugin registration.
func (b *HostBridge) AuthList(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	env, err := b.invoke(ctx, pluginabi.MethodHostAuthList, struct{}{}, "host auth list")
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if len(env.Result) > 0 && json.Unmarshal(env.Result, &resp) != nil {
		return nil, fmt.Errorf("host auth list failed: undecodable response body")
	}
	return resp.Files, nil
}

// hostStreamIDReq addresses an existing stream by its ID.
type hostStreamIDReq struct {
	StreamID string `json:"stream_id"`
}

// hostStreamStartResp decodes the host.http.do_stream result.
type hostStreamStartResp struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

// hostStreamReadResp decodes one host.http.stream_read result.
type hostStreamReadResp struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// decodeEnvelope parses a host callback response envelope.
func decodeEnvelope(raw []byte) (pluginabi.Envelope, error) {
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("undecodable host response")
	}
	return env, nil
}

// callWithTimeout runs one host callback under the attempt deadline. On ctx
// expiry it returns a timeout-classified error immediately; the orphaned
// callback goroutine's eventual result is discarded — the underlying host
// HTTP client governs actual teardown, and the goroutine exits when it
// returns (buffered channel, so it never leaks). The goroutine also
// recovers panics: a host-boundary crash must degrade to a redacted error,
// never escape into the host process.
func (b *HostBridge) callWithTimeout(ctx context.Context, method string, payload []byte) ([]byte, error) {
	raw, _, err := b.callWithTimeoutResult(ctx, method, payload, 0, nil)
	return raw, err
}

// streamTimeoutGrace bounds how long a timed-out host.http.do_stream start
// SYNCHRONOUSLY waits for its orphaned callback: the host registers the
// upstream stream BEFORE surfacing its status, so a result landing just
// past the deadline is salvaged inline. Results landing later are not lost:
// they are handed to abandon on a background goroutine that closes any
// stream they name.
var streamTimeoutGrace = 3 * time.Second

// callWithTimeoutResult is callWithTimeout with an optional bounded grace
// window after ctx expiry: a result arriving within the window is returned
// with late=true so callers can salvage side effects (late-started streams);
// timeout semantics remain the caller's policy. When the window lapses the
// timeout error is returned immediately and abandon (if non-nil) later runs
// once with whatever the orphaned callback produces — however long that
// takes — so abandoned host side effects are released without delaying the
// caller. grace 0 with nil abandon behaves exactly as callWithTimeout.
//
// Every invocation holds one inFlight count from entry until it FULLY
// completes: the count is released only once the callback goroutine has
// exited AND its post-work is settled (result observed, abandon/drain
// cleanup finished including any late stream_close, or nothing pending).
// A callback parked inside the host therefore keeps shutdown waiting even
// when no abandon func was supplied.
func (b *HostBridge) callWithTimeoutResult(ctx context.Context, method string, payload []byte, grace time.Duration, abandon func(raw []byte)) ([]byte, bool, error) {
	type callResult struct {
		raw []byte
		err error
	}
	done := make(chan callResult, 1)
	b.inFlight.Add(1)
	var (
		hs      sync.Mutex
		exited  bool // callback goroutine has returned
		drained bool // post-work settled (observed / abandoned / none needed)
		letGo   bool // inFlight.Done already performed
	)
	maybeRelease := func() {
		if exited && drained && !letGo {
			letGo = true
			b.inFlight.Done()
		}
	}
	settle := func() {
		hs.Lock()
		drained = true
		maybeRelease()
		hs.Unlock()
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- callResult{err: fmt.Errorf("host callback panicked")}
			}
			hs.Lock()
			exited = true
			maybeRelease()
			hs.Unlock()
		}()
		raw, err := b.call(method, payload)
		done <- callResult{raw: raw, err: err}
	}()
	select {
	case res := <-done:
		settle()
		return res.raw, false, res.err
	case <-ctx.Done():
		if grace > 0 {
			t := time.NewTimer(grace)
			defer t.Stop()
			select {
			case res := <-done:
				settle()
				return res.raw, true, res.err
			case <-t.C:
			}
		}
		if abandon != nil {
			go func() {
				// Held until the host returns, however late: a parked
				// goroutine is acceptable, a leaked upstream stream is not.
				res := <-done
				abandon(res.raw)
				// Settled only after abandon fully completes, so shutdown
				// never outruns the late-ID stream_close.
				settle()
			}()
			return nil, false, fmt.Errorf("host call %s timed out", method)
		}
		// No abandon func: only the callback goroutine's exit remains.
		settle()
		return nil, false, fmt.Errorf("host call %s timed out", method)
	}
}

// WaitForInFlight blocks until every in-flight host-callback invocation has
// fully completed — including callbacks orphaned past their deadline and
// their abandon/drain cleanup — or timeout elapses, returning false on
// timeout (sync.WaitGroup has no timed wait). handleShutdown calls this so
// the export does not return while our goroutines may still call into host
// memory that a Unix loader frees immediately afterwards.
func (b *HostBridge) WaitForInFlight(timeout time.Duration) bool {
	drained := make(chan struct{})
	go func() {
		b.inFlight.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return true
	case <-time.After(timeout):
		return false
	}
}

// invoke marshals payload and performs the host callback under ctx through
// callWithTimeout, so EVERY invocation holds one inFlight count for the
// duration of its callback (the invariant documented on
// callWithTimeoutResult) and shutdown draining sees all of them.
// Normalizes every failure into a redacted error prefixed with what. Result
// decoding is left to callers because each method has a distinct result
// shape.
func (b *HostBridge) invoke(ctx context.Context, method string, payload any, what string) (pluginabi.Envelope, error) {
	payloadBytes, _ := json.Marshal(payload)
	raw, err := b.callWithTimeout(ctx, method, payloadBytes)
	if err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %w", what, err)
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %w", what, err)
	}
	if !env.OK {
		msg := ""
		if env.Error != nil {
			msg = env.Error.Message
		}
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %s", what, msg)
	}
	return env, nil
}

// Do issues an upstream HTTP request through the host callback
// "host.http.do"; transport-level failures surface as errors whose text is
// safe for logs (no keys, no response bodies).
func (b *HostBridge) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	env, err := b.invoke(ctx, pluginabi.MethodHostHTTPDo, hostHTTPReq{
		Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body,
	}, "host http do")
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var resp pluginapi.HTTPResponse
	if len(env.Result) > 0 && json.Unmarshal(env.Result, &resp) != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("host http do failed: undecodable response body")
	}
	return resp, nil
}

// DoStream opens an upstream HTTP stream via "host.http.do_stream" and
// returns the upstream status, headers, and UPSTREAM stream ID used with
// StreamRead/StreamClose. When the start callback outlives the attempt
// deadline, the host may still have registered the upstream stream before
// surfacing its status: DoStream waits streamTimeoutGrace for the orphaned
// callback and closes any stream it produced (F1), while still returning
// the timeout error for CPA retry handling; a result arriving after the
// grace window is closed by the background abandon goroutine instead, so
// no registered upstream stream ever leaks regardless of host latency.
func (b *HostBridge) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (statusCode int, headers http.Header, upstreamStreamID string, err error) {
	payloadBytes, _ := json.Marshal(hostHTTPReq{
		Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body,
	})
	raw, late, err := b.callWithTimeoutResult(ctx, pluginabi.MethodHostHTTPDoStream, payloadBytes, streamTimeoutGrace, b.closeLateStream)
	if late {
		// Deadline already expired: whatever the orphaned start produced,
		// release its stream if any and report the timeout for CPA retry handling.
		b.closeLateStream(raw)
		return 0, nil, "", fmt.Errorf("host http do_stream failed: host call %s timed out", pluginabi.MethodHostHTTPDoStream)
	}
	if err != nil {
		return 0, nil, "", fmt.Errorf("host http do_stream failed: %w", err)
	}
	env, derr := decodeEnvelope(raw)
	if derr != nil {
		return 0, nil, "", fmt.Errorf("host http do_stream failed: %w", derr)
	}
	if !env.OK {
		msg := ""
		if env.Error != nil {
			msg = env.Error.Message
		}
		return 0, nil, "", fmt.Errorf("host http do_stream failed: %s", msg)
	}
	var out hostStreamStartResp
	if len(env.Result) > 0 && json.Unmarshal(env.Result, &out) != nil {
		return 0, nil, "", fmt.Errorf("host http do_stream failed: undecodable response body")
	}
	return out.StatusCode, out.Headers, out.StreamID, nil
}

// closeLateStream releases a do_stream result that completed after its
// attempt deadline — synchronously within the grace window or arbitrarily
// late via the orphan-cleanup goroutine: decode it best-effort and close
// the upstream stream the host registered for it, if any. Failures here
// are non-fatal — the timeout error already governs the caller's retry
// policy.
func (b *HostBridge) closeLateStream(raw []byte) {
	env, err := decodeEnvelope(raw)
	if err != nil || !env.OK || len(env.Result) == 0 {
		return
	}
	var out hostStreamStartResp
	if json.Unmarshal(env.Result, &out) != nil || out.StreamID == "" {
		return
	}
	_ = b.StreamClose(out.StreamID)
}

// StreamRead reads one chunk from an UPSTREAM stream opened by DoStream.
// errMsg carries an upstream-reported failure label; done reports a clean
// end of stream. context.Background() imposes no deadline — blocking is
// governed by upstream close semantics and the executor watchdog — but the
// callback still joins inFlight accounting so shutdown drains a read
// parked inside the host instead of dlclose-ing over its live FFI frame.
func (b *HostBridge) StreamRead(upstreamStreamID string) (payload []byte, errMsg string, done bool, err error) {
	env, err := b.invoke(context.Background(), pluginabi.MethodHostHTTPStreamRead,
		hostStreamIDReq{StreamID: upstreamStreamID}, "host http stream_read")
	if err != nil {
		return nil, "", false, err
	}
	var out hostStreamReadResp
	if len(env.Result) > 0 && json.Unmarshal(env.Result, &out) != nil {
		return nil, "", false, fmt.Errorf("host http stream_read failed: undecodable response body")
	}
	return out.Payload, out.Error, out.Done, nil
}

// StreamClose closes an UPSTREAM stream opened by DoStream; the result
// content is ignored. Like StreamRead: no deadline, but joins inFlight
// accounting so shutdown drains a close parked inside the host.
func (b *HostBridge) StreamClose(upstreamStreamID string) error {
	_, err := b.invoke(context.Background(), pluginabi.MethodHostHTTPStreamClose,
		hostStreamIDReq{StreamID: upstreamStreamID}, "host http stream_close")
	return err
}

// StreamEmit writes one chunk to a DOWNSTREAM stream (host-allocated ID
// received in executor.execute_stream requests). Bounded by emitTimeout:
// host downstream backpressure must surface as an error, not a wedged
// producer goroutine.
func (b *HostBridge) StreamEmit(downstreamStreamID string, payload []byte) error {
	req := struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
	}{downstreamStreamID, payload}
	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
	defer cancel()
	_, err := b.invoke(ctx, pluginabi.MethodHostStreamEmit, req, "host stream emit")
	return err
}

// StreamCloseDownstream terminates a DOWNSTREAM stream, optionally with a
// redacted error label. Bounded by emitTimeout like StreamEmit: on a wedged
// downstream consumer the close itself is what must not block.
func (b *HostBridge) StreamCloseDownstream(downstreamStreamID string, errMsg string) error {
	req := struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error,omitempty"`
	}{downstreamStreamID, errMsg}
	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
	defer cancel()
	_, err := b.invoke(ctx, pluginabi.MethodHostStreamClose, req, "host stream close")
	return err
}

// logTimeout bounds every host.log callback, including the nil-ctx fast
// paths elsewhere in the bridge: a wedged host.log between lifecycle close
// and done would otherwise block the ticker goroutine and stall shutdown.
// Package var so tests can shrink it.
var logTimeout = 5 * time.Second

// emitTimeout bounds the downstream stream callbacks (stream emit / close):
// the pinned SDK executes them under context.Background() with no
// WriteTimeout, so a host whose downstream chunk consumer stops draining
// would otherwise park the execute_stream producer forever. Package var so
// tests can shrink it.
var emitTimeout = logTimeout

// Log forwards one diagnostic record to the host callback "host.log".
// message and fields must already be secret-free. Bounded by logTimeout;
// callers treat the returned error as best-effort.
func (b *HostBridge) Log(level, message string, fields map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), logTimeout)
	defer cancel()
	_, err := b.invoke(ctx, pluginabi.MethodHostLog,
		hostLogReq{Level: level, Message: message, Fields: fields}, "host log")
	return err
}
