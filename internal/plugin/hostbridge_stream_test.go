package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func decodePayload(t *testing.T, c capturedCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.payload, &m); err != nil {
		t.Fatalf("payload not json (%s): %v", c.payload, err)
	}
	return m
}

func TestBridgeDoStream(t *testing.T) {
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDoStream {
			t.Fatalf("unexpected callback %q", method)
		}
		return hostOK(hostStreamStartResp{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
			StreamID:   "up-stream-1",
		}), nil
	}}
	bridge := NewHostBridge(f.call)
	status, headers, streamID, err := bridge.DoStream(context.Background(), pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     "https://up.test/v1/chat/completions",
		Headers: http.Header{"Authorization": []string{"Bearer " + testKey}},
		Body:    []byte(`{"model":"glm-5.3"}`),
	})
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if status != http.StatusOK || streamID != "up-stream-1" || headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("decoded start = %d %q %v", status, streamID, headers)
	}
	calls := f.callsOf(pluginabi.MethodHostHTTPDoStream)
	if len(calls) != 1 {
		t.Fatalf("do_stream calls = %d, want 1", len(calls))
	}
	m := decodePayload(t, calls[0])
	if m["method"] != http.MethodPost || m["url"] != "https://up.test/v1/chat/completions" ||
		m["body"] != "eyJtb2RlbCI6ImdsbS01LjMifQ==" {
		t.Fatalf("wire payload wrong: %v", m)
	}
	hdrs := m["headers"].(map[string]any)
	if hdrs["Authorization"].([]any)[0].(string) != "Bearer "+testKey {
		t.Fatalf("headers wrong: %v", hdrs)
	}

	// omitempty: no headers/stream_id in result decodes to zero values.
	f2 := &fakeCaller{responder: func(_ string, _ []byte) ([]byte, error) {
		return []byte(`{"ok":true,"result":{"status_code":429}}`), nil
	}}
	status2, headers2, id2, err2 := NewHostBridge(f2.call).DoStream(context.Background(), pluginapi.HTTPRequest{})
	if err2 != nil || status2 != http.StatusTooManyRequests || headers2 != nil || id2 != "" {
		t.Fatalf("bare start = %d %q %v %v", status2, id2, headers2, err2)
	}
}

func TestBridgeDoStreamFailures(t *testing.T) {
	cases := []struct {
		name    string
		respond func(string, []byte) ([]byte, error)
		wantSub string
	}{
		{"envelope error", func(string, []byte) ([]byte, error) { return hostErr("busy", "no slots"), nil }, "no slots"},
		{"envelope error without object", func(string, []byte) ([]byte, error) { return []byte(`{"ok":false}`), nil }, "host http do_stream failed:"},
		{"malformed result", func(string, []byte) ([]byte, error) { return []byte(`{"ok":true,"result":"{bad"}`), nil }, "undecodable response body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := NewHostBridge((&fakeCaller{responder: tc.respond}).call).
				DoStream(context.Background(), pluginapi.HTTPRequest{})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestBridgeDoStreamLateStreamClosed pins the F1 deploy-safety fix: a
// do_stream start that completes just past the attempt deadline must still
// surface as a timeout error AND close the upstream stream the host had
// already registered, instead of leaking one connection + host pump per
// rotated key.
func TestBridgeDoStreamLateStreamClosed(t *testing.T) {
	oldGrace := streamTimeoutGrace
	streamTimeoutGrace = time.Second
	defer func() { streamTimeoutGrace = oldGrace }()

	f := &fakeCaller{responder: func(string, []byte) ([]byte, error) {
		time.Sleep(200 * time.Millisecond) // lands past the ctx deadline, inside grace
		return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "late-stream"}), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, err := NewHostBridge(f.call).DoStream(ctx, pluginapi.HTTPRequest{}); err == nil ||
		!strings.Contains(err.Error(), "host call host.http.do_stream timed out") {
		t.Fatalf("late result must still surface as timeout: %v", err)
	}
	closes := f.callsOf(pluginabi.MethodHostHTTPStreamClose)
	if len(closes) != 1 {
		t.Fatalf("stream_close calls = %d, want 1", len(closes))
	}
	if m := decodePayload(t, closes[0]); m["stream_id"] != "late-stream" {
		t.Fatalf("closed wrong stream: %v", m)
	}
}

// Late results without a salvageable stream ID (error envelope, bare ok,
// malformed result, missing ID, panicked callback) must close nothing.
func TestBridgeDoStreamLateNoClose(t *testing.T) {
	oldGrace := streamTimeoutGrace
	streamTimeoutGrace = time.Second
	defer func() { streamTimeoutGrace = oldGrace }()

	cases := []struct {
		name    string
		respond func(string, []byte) ([]byte, error)
	}{
		{"error envelope", func(string, []byte) ([]byte, error) {
			time.Sleep(100 * time.Millisecond)
			return hostErr("busy", "no slots"), nil
		}},
		{"bare ok without result", func(string, []byte) ([]byte, error) {
			time.Sleep(100 * time.Millisecond)
			return []byte(`{"ok":true}`), nil
		}},
		{"malformed result", func(string, []byte) ([]byte, error) {
			time.Sleep(100 * time.Millisecond)
			return []byte(`{"ok":true,"result":"{bad"}`), nil
		}},
		{"success without stream id", func(string, []byte) ([]byte, error) {
			time.Sleep(100 * time.Millisecond)
			return hostOK(hostStreamStartResp{StatusCode: http.StatusTooManyRequests}), nil
		}},
		{"panicked callback", func(string, []byte) ([]byte, error) {
			time.Sleep(100 * time.Millisecond)
			panic("boom")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCaller{responder: tc.respond}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, _, _, err := NewHostBridge(f.call).DoStream(ctx, pluginapi.HTTPRequest{}); err == nil ||
				!strings.Contains(err.Error(), "timed out") {
				t.Fatalf("err = %v, want timeout", err)
			}
			if got := len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)); got != 0 {
				t.Fatalf("stream_close calls = %d, want 0", got)
			}
		})
	}
}

// A result landing LONG past deadline+grace must not leak the registered
// upstream stream: DoStream still returns its timeout error immediately
// (the caller never waits for the orphan), and when the orphaned callback
// eventually lands, stream_close is issued however late.
func TestBridgeDoStreamAbandonedStreamClosedEventually(t *testing.T) {
	oldGrace := streamTimeoutGrace
	streamTimeoutGrace = 40 * time.Millisecond
	defer func() { streamTimeoutGrace = oldGrace }()

	produced := make(chan struct{})
	closedID := make(chan string, 1)
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			<-produced // parked far beyond deadline + grace
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "very-late-stream"}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			closedID <- "very-late-stream"
			return hostOK(map[string]any{}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, _, _, err := NewHostBridge(f.call).DoStream(ctx, pluginapi.HTTPRequest{}); err == nil ||
		!strings.Contains(err.Error(), "host call host.http.do_stream timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout surfaced after %v; caller must not wait for the orphan", elapsed)
	}

	close(produced)
	select {
	case id := <-closedID:
		if id != "very-late-stream" {
			t.Fatalf("closed %q, want very-late-stream", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("orphaned result landed but stream_close was never issued")
	}
}

func TestBridgeStreamRead(t *testing.T) {
	cases := []struct {
		name       string
		result     string
		wantData   string
		wantErrMsg string
		wantDone   bool
	}{
		{"payload chunk", `{"payload":"aGVsbG8="}`, "hello", "", false},
		{"clean end", `{"done":true}`, "", "", true},
		{"upstream error ends stream", `{"error":"overloaded","done":true}`, "", "overloaded", true},
		{"empty result", `{}`, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCaller{responder: func(_ string, _ []byte) ([]byte, error) {
				return []byte(`{"ok":true,"result":` + tc.result + `}`), nil
			}}
			data, errMsg, done, err := NewHostBridge(f.call).StreamRead("up-stream-1")
			if err != nil {
				t.Fatalf("StreamRead: %v", err)
			}
			if string(data) != tc.wantData || errMsg != tc.wantErrMsg || done != tc.wantDone {
				t.Fatalf("read = %q %q %v, want %q %q %v", data, errMsg, done, tc.wantData, tc.wantErrMsg, tc.wantDone)
			}
			calls := f.callsOf(pluginabi.MethodHostHTTPStreamRead)
			if len(calls) != 1 {
				t.Fatalf("stream_read calls = %d, want 1", len(calls))
			}
			m := decodePayload(t, calls[0])
			if len(m) != 1 || m["stream_id"] != "up-stream-1" {
				t.Fatalf("wire payload wrong: %v", m)
			}
		})
	}
	if _, _, _, err := NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return hostErr("gone", "stream closed"), nil
	}}).call).StreamRead("up-stream-1"); err == nil || !strings.Contains(err.Error(), "stream closed") {
		t.Fatalf("expected envelope error, got %v", err)
	}
	if _, _, _, err := NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return []byte(`{"ok":true,"result":"{bad"}`), nil
	}}).call).StreamRead("up"); err == nil || !strings.Contains(err.Error(), "undecodable response body") {
		t.Fatalf("expected malformed-result error, got %v", err)
	}
}

func TestBridgeStreamCloseUpstream(t *testing.T) {
	f := &fakeCaller{}
	if err := NewHostBridge(f.call).StreamClose("up-stream-1"); err != nil {
		t.Fatalf("StreamClose: %v", err)
	}
	calls := f.callsOf(pluginabi.MethodHostHTTPStreamClose)
	if len(calls) != 1 {
		t.Fatalf("stream_close calls = %d, want 1", len(calls))
	}
	m := decodePayload(t, calls[0])
	if len(m) != 1 || m["stream_id"] != "up-stream-1" {
		t.Fatalf("wire payload wrong: %v", m)
	}
	err := NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return hostErr("gone", "already closed"), nil
	}}).call).StreamClose("up-stream-1")
	if err == nil || !strings.Contains(err.Error(), "already closed") {
		t.Fatalf("err = %v", err)
	}
}

func TestBridgeStreamEmit(t *testing.T) {
	f := &fakeCaller{}
	if err := NewHostBridge(f.call).StreamEmit("down-stream-7", []byte("chunk-one")); err != nil {
		t.Fatalf("StreamEmit: %v", err)
	}
	calls := f.callsOf(pluginabi.MethodHostStreamEmit)
	if len(calls) != 1 {
		t.Fatalf("stream emit calls = %d, want 1", len(calls))
	}
	m := decodePayload(t, calls[0])
	if len(m) != 2 || m["stream_id"] != "down-stream-7" || m["payload"] != "Y2h1bmstb25l" {
		t.Fatalf("wire payload wrong: %v", m)
	}
	err := NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return hostErr("detached", "downstream gone"), nil
	}}).call).StreamEmit("down-stream-7", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "downstream gone") {
		t.Fatalf("err = %v", err)
	}
}

func TestBridgeStreamCloseDownstream(t *testing.T) {
	f := &fakeCaller{}
	if err := NewHostBridge(f.call).StreamCloseDownstream("down-stream-7", ""); err != nil {
		t.Fatalf("StreamCloseDownstream: %v", err)
	}
	calls := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(calls) != 1 {
		t.Fatalf("stream close calls = %d, want 1", len(calls))
	}
	m := decodePayload(t, calls[0])
	if len(m) != 1 || m["stream_id"] != "down-stream-7" {
		t.Fatalf("empty-error wire payload should omit error key: %v", m)
	}
	f2 := &fakeCaller{}
	if err := NewHostBridge(f2.call).StreamCloseDownstream("down-stream-7", "upstream failed"); err != nil {
		t.Fatalf("StreamCloseDownstream with error: %v", err)
	}
	m2 := decodePayload(t, f2.callsOf(pluginabi.MethodHostStreamClose)[0])
	if len(m2) != 2 || m2["error"] != "upstream failed" {
		t.Fatalf("wire payload wrong: %v", m2)
	}
	err := NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return hostErr("gone", "no such stream"), nil
	}}).call).StreamCloseDownstream("down-stream-7", "x")
	if err == nil || !strings.Contains(err.Error(), "no such stream") {
		t.Fatalf("err = %v", err)
	}
}
