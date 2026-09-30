package plugin

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestBridgeCallTimeout pins the F-watchdog contract: a black-holed host
// callback returns a timeout-classified error within the ctx budget while
// the orphaned goroutine drains on release.
func TestBridgeCallTimeout(t *testing.T) {
	oldGrace := streamTimeoutGrace
	streamTimeoutGrace = 20 * time.Millisecond
	defer func() { streamTimeoutGrace = oldGrace }()
	release := make(chan struct{})
	defer close(release) // drain the orphaned goroutine
	blocking := func(string, []byte) ([]byte, error) {
		<-release
		return hostOK(map[string]any{}), nil
	}
	f := &fakeCaller{responder: blocking}
	b := NewHostBridge(f.call)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	if _, err := b.Do(ctx, pluginapi.HTTPRequest{}); err == nil ||
		!strings.Contains(err.Error(), "host call host.http.do timed out") {
		t.Fatalf("Do err = %v", err)
	}
	if _, _, _, err := b.DoStream(ctx, pluginapi.HTTPRequest{}); err == nil ||
		!strings.Contains(err.Error(), "host call host.http.do_stream timed out") {
		t.Fatalf("DoStream err = %v", err)
	}
}

// TestBridgeWaitForInFlight pins the shutdown-drain contract: an orphaned
// host callback (parked past its deadline) keeps the bridge's in-flight
// count held — WaitForInFlight must time out while it lives — and the
// count drops only after the orphan returns AND its late-drain cleanup
// (the salvage stream_close) has fully run.
func TestBridgeWaitForInFlight(t *testing.T) {
	if !NewHostBridge(nil).WaitForInFlight(50 * time.Millisecond) {
		t.Fatal("idle bridge must report no in-flight callbacks")
	}

	oldGrace := streamTimeoutGrace
	streamTimeoutGrace = 20 * time.Millisecond
	defer func() { streamTimeoutGrace = oldGrace }()

	release := make(chan struct{})
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			<-release // parked far past deadline + grace: the orphan
			return hostOK(hostStreamStartResp{StatusCode: http.StatusOK, StreamID: "orphan-stream"}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			return hostOK(map[string]any{}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	b := NewHostBridge(f.call)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if _, _, _, err := b.DoStream(ctx, pluginapi.HTTPRequest{}); err == nil ||
		!strings.Contains(err.Error(), "host call host.http.do_stream timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	start := time.Now()
	if b.WaitForInFlight(50 * time.Millisecond) {
		t.Fatal("WaitForInFlight reported idle while the orphaned callback is still parked")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout branch took %v, want ~50ms", elapsed)
	}

	close(release)
	if !b.WaitForInFlight(time.Second) {
		t.Fatal("WaitForInFlight timed out even after the orphan was released")
	}
	closes := f.callsOf(pluginabi.MethodHostHTTPStreamClose)
	if len(closes) != 1 {
		t.Fatalf("stream_close calls = %d, want 1", len(closes))
	}
	if m := decodePayload(t, closes[0]); m["stream_id"] != "orphan-stream" {
		t.Fatalf("late close released wrong stream: %v", m)
	}
}

// TestBridgeStreamCloseJoinsInFlight pins the shutdown-drain fix for the
// unbounded upstream stream callbacks: a StreamClose parked inside the host
// (executor watchdog StreamClose path) holds one inFlight count, so
// handleShutdown's WaitForInFlight must NOT return while it lives — Unix
// dlclose would otherwise unmap its live FFI frame — and must report idle
// only after release, with the late close recorded.
func TestBridgeStreamCloseJoinsInFlight(t *testing.T) {
	release := make(chan struct{})
	f := &fakeCaller{responder: func(_ string, _ []byte) ([]byte, error) {
		<-release // parked inside the host until signalled
		return hostOK(map[string]any{}), nil
	}}
	b := NewHostBridge(f.call)
	done := make(chan error, 1)
	go func() { done <- b.StreamClose("parked-stream") }()

	// The fake caller records the invocation before parking, so seeing it
	// recorded proves the inFlight count is already held.
	deadline := time.Now().Add(2 * time.Second)
	for len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("StreamClose never reached the host")
		}
		time.Sleep(time.Millisecond)
	}
	if b.WaitForInFlight(50 * time.Millisecond) {
		t.Fatal("WaitForInFlight reported idle while StreamClose is parked inside the host")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StreamClose: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("released StreamClose never returned")
	}
	if !b.WaitForInFlight(2 * time.Second) {
		t.Fatal("WaitForInFlight timed out even after StreamClose was released")
	}
	closes := f.callsOf(pluginabi.MethodHostHTTPStreamClose)
	if len(closes) != 1 {
		t.Fatalf("stream_close calls = %d, want 1", len(closes))
	}
	if m := decodePayload(t, closes[0]); m["stream_id"] != "parked-stream" {
		t.Fatalf("late close released wrong stream: %v", m)
	}
}

// Same drain contract for a StreamRead parked mid-stream (the executor
// pump goroutine): in-flight during the park, drained after release.
func TestBridgeStreamReadJoinsInFlight(t *testing.T) {
	release := make(chan struct{})
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPStreamRead {
			return hostOK(map[string]any{}), nil
		}
		<-release // parked mid-stream until signalled
		return []byte(`{"ok":true,"result":{"payload":"Y2h1bms="}}`), nil
	}}
	b := NewHostBridge(f.call)
	done := make(chan error, 1)
	go func() {
		_, _, _, err := b.StreamRead("mid-stream")
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for len(f.callsOf(pluginabi.MethodHostHTTPStreamRead)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("StreamRead never reached the host")
		}
		time.Sleep(time.Millisecond)
	}
	if b.WaitForInFlight(50 * time.Millisecond) {
		t.Fatal("WaitForInFlight reported idle while StreamRead is parked mid-stream")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StreamRead: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("released StreamRead never returned")
	}
	if !b.WaitForInFlight(2 * time.Second) {
		t.Fatal("WaitForInFlight timed out even after StreamRead was released")
	}
}

func TestBridgeNoTimeoutWhenCallbackReturns(t *testing.T) {
	f := &fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return hostOK(map[string]any{}), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := NewHostBridge(f.call).Do(ctx, pluginapi.HTTPRequest{}); err != nil {
		t.Fatalf("fast callback must not time out: %v", err)
	}
}

// TestBridgeLogTimeout pins the bounded host.log contract: a wedged host.log
// callback must surface a timeout error within logTimeout instead of
// blocking forever (the ticker goroutine logs between close(stop) and
// close(done); an unbounded callback there stalls shutdown).
func TestBridgeLogTimeout(t *testing.T) {
	old := logTimeout
	logTimeout = 40 * time.Millisecond
	defer func() { logTimeout = old }()
	release := make(chan struct{})
	defer close(release) // drain the orphaned goroutine
	f := &fakeCaller{responder: func(string, []byte) ([]byte, error) {
		<-release
		return hostOK(map[string]any{}), nil
	}}

	start := time.Now()
	err := NewHostBridge(f.call).Log("warn", "m", nil)
	if err == nil || !strings.Contains(err.Error(), "host call host.log timed out") {
		t.Fatalf("Log err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Log returned after %v, want ~logTimeout", elapsed)
	}

	logTimeout = time.Minute // normal logs unaffected by the bound
	if err := NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return hostOK(map[string]any{}), nil
	}}).call).Log("warn", "m", nil); err != nil {
		t.Fatalf("normal Log: %v", err)
	}
}
