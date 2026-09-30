package errclass

import (
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestClassConstants(t *testing.T) {
	want := map[Class]string{
		ClassAuth:         "auth_failure",
		ClassInvalidModel: "invalid_model",
		ClassUnsupported:  "unsupported_protocol_or_parameter",
		ClassRateLimit:    "rate_limit",
		ClassQuota:        "quota_exhaustion",
		ClassBilling:      "billing",
		ClassUpstream:     "upstream_server_failure",
		ClassNetwork:      "timeout_or_network_failure",
		ClassTranslation:  "translation_failure",
	}
	for cls, s := range want {
		if string(cls) != s {
			t.Errorf("Class(%q) constant = %q, want %q", s, string(cls), s)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	e := &Error{Class: ClassRateLimit, Message: "slow down", StatusCode: 429}
	if got := e.Error(); got != "rate_limit: slow down" {
		t.Errorf("Error() = %q", got)
	}
}

func TestFromStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		message   string
		class     Class
		retryable bool
	}{
		{"401 auth failure", 401, "bad key", ClassAuth, true},
		{"401 quota text stays auth", 401, "quota exhausted for key", ClassAuth, true},
		{"403 forbidden auth failure", 403, "no access", ClassAuth, true},
		{"402 billing failure", 402, "payment required", ClassBilling, true},
		{"404 invalid model", 404, "no such model", ClassInvalidModel, false},
		{"429 rate limit", 429, "too many", ClassRateLimit, true},
		{"429 quota text upgrades", 429, "quota exceeded for project", ClassQuota, true},
		{"429 Go usage limit", 429, "GoUsageLimitError", ClassRateLimit, true},
		{"403 quota text upgrades", 403, "quota exhausted", ClassQuota, true},
		{"402 quota text bills as quota", 402, "quota depleted", ClassQuota, true},
		{"408 client timeout unsupported", 408, "request timeout", ClassUnsupported, false},
		{"502 upstream", 502, "bad gateway", ClassUpstream, true},
		{"500 upstream", 500, "boom", ClassUpstream, true},
		{"599 upstream", 599, "weird", ClassUpstream, true},
		{"400 unsupported", 400, "bad field", ClassUnsupported, false},
		{"418 unsupported", 418, "teapot", ClassUnsupported, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FromStatus(tc.status, tc.message)
			if got.Class != tc.class || got.Retryable != tc.retryable ||
				got.StatusCode != tc.status || got.Message != tc.message {
				t.Errorf("FromStatus(%d,%q) = %+v", tc.status, tc.message, got)
			}
		})
	}
}

func TestFromNetwork(t *testing.T) {
	e := FromNetwork(errors.New("dial tcp: refused"))
	if e.Class != ClassNetwork || !e.Retryable || e.StatusCode != 0 ||
		e.Message != "dial tcp: refused" {
		t.Errorf("FromNetwork(err) = %+v", e)
	}
	nilErr := FromNetwork(nil)
	if nilErr.Message != "" || !nilErr.Retryable {
		t.Errorf("FromNetwork(nil) = %+v", nilErr)
	}
}

func TestTranslation(t *testing.T) {
	e := Translation("cannot map tool calls")
	if e.Class != ClassTranslation || e.Retryable || e.StatusCode != 0 {
		t.Errorf("Translation() = %+v", e)
	}
}

func TestUpstreamFallback(t *testing.T) {
	e := UpstreamFallback("upstream stream broke: Bearer sk-secret99 mid-chunk")
	if e.Class != ClassUpstream {
		t.Errorf("Class = %q, want %q", e.Class, ClassUpstream)
	}
	if !e.Retryable {
		t.Error("Retryable = false, want true (§7: upstream server failures are retryable)")
	}
	if e.StatusCode != 0 {
		t.Errorf("StatusCode = %+v", e)
	}
	want := "upstream stream broke: Bearer [redacted] mid-chunk"
	if e.Message != want {
		t.Errorf("Message = %q, want redacted %q", e.Message, want)
	}
}

func TestPostFirstByteNetwork(t *testing.T) {
	e := PostFirstByteNetwork("stream read after first byte: Bearer sk-secret99 dropped")
	if e.Class != ClassNetwork {
		t.Errorf("Class = %q, want %q", e.Class, ClassNetwork)
	}
	if e.Retryable {
		t.Error("Retryable = true, want false (§7: post-first-byte failures must never retry)")
	}
	if e.StatusCode != 0 {
		t.Errorf("StatusCode = %+v", e)
	}
	want := "stream read after first byte: Bearer [redacted] dropped"
	if e.Message != want {
		t.Errorf("Message = %q, want redacted %q", e.Message, want)
	}
}

func TestToEnvelopeError(t *testing.T) {
	in := &Error{Class: ClassAuth, Message: "Bearer sk-secret failed", StatusCode: 401}
	got := ToEnvelopeError(in)
	want := pluginabi.Error{Code: "auth_failure", Message: "Bearer [redacted] failed", Retryable: false, HTTPStatus: 401}
	if got != want {
		t.Errorf("ToEnvelopeError = %+v, want %+v", got, want)
	}

	// Issue #2: ClassUnsupported and ClassTranslation with StatusCode 0 default to 400.
	unsupported := &Error{Class: ClassUnsupported, Message: "unsupported input"}
	got = ToEnvelopeError(unsupported)
	if got.HTTPStatus != 400 || got.Code != "unsupported_protocol_or_parameter" {
		t.Errorf("ToEnvelopeError(unsupported) = %+v, want HTTPStatus 400", got)
	}

	translation := Translation("cannot map tool calls")
	got = ToEnvelopeError(translation)
	if got.HTTPStatus != 400 || got.Code != "translation_failure" {
		t.Errorf("ToEnvelopeError(translation) = %+v, want HTTPStatus 400", got)
	}

	// Explicit status code is preserved.
	explicit := &Error{Class: ClassUnsupported, Message: "bad param", StatusCode: 422}
	got = ToEnvelopeError(explicit)
	if got.HTTPStatus != 422 {
		t.Errorf("ToEnvelopeError(explicit) = %+v, want HTTPStatus 422", got)
	}

	// Other classes with StatusCode 0 remain 0.
	netErr := FromNetwork(errors.New("connection reset"))
	got = ToEnvelopeError(netErr)
	if got.HTTPStatus != 0 {
		t.Errorf("ToEnvelopeError(network) = %+v, want HTTPStatus 0", got)
	}
}

func TestRedact(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain failure", "plain failure"},
		{"Authorization: Bearer abc123 bad", "Authorization: Bearer [redacted] bad"},
		{"bearer lower-case tok!", "Bearer [redacted] tok!"},
		{"two: Bearer a1 and Bearer b2 end", "two: Bearer [redacted] and Bearer [redacted] end"},
		{"trailing token Bearer z9", "trailing token Bearer [redacted]"},
		{"list Bearer t1,x \"Bearer t2\"", "list Bearer [redacted],x \"Bearer [redacted]\""},
		{"lone word bearer", "lone word Bearer [redacted]"},
		{"tabbed\tBearer\ttok\tend", "tabbed\tBearer [redacted]\tend"},
	}
	for _, tc := range tests {
		if got := Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactAPIKeyEchoes(t *testing.T) {
	tests := []struct{ in, want string }{
		// Header echo forms: colon, JSON quotes, equals, bare space.
		{"upstream said x-api-key: sk-ant-api03-9f2Kx7QwLp end", "upstream said x-api-key: [redacted] end"},
		{`{"error":{"message":"invalid x-api-key sk-live-ab12cd34ef"}}`, `{"error":{"message":"invalid x-api-key [redacted]"}}`},
		{"auth failed x-api-key=AKIA1234ABCD5678 retry", "auth failed x-api-key=[redacted] retry"},
		{"echo X-API-Key sk-test-1234567890 done", "echo X-API-Key [redacted] done"},
		// Base64 alphabet (+, /, =) values are covered by the value class.
		{"echo x-api-key: aGVsbG8rd29ybGQ9 end", "echo x-api-key: [redacted] end"},
		{"auth x-api-key=Ab12Cd34+/EFghIjKl== retry", "auth x-api-key=[redacted] retry"},
		// Prose after the header name must not be mangled.
		{"missing x-api-key authentication header", "missing x-api-key authentication header"},
		{"the x-api-key header is required", "the x-api-key header is required"},
		{"x-api-key: request rejected by policy", "x-api-key: request rejected by policy"},
		// Too short to be a key.
		{"x-api-key: abc123 next", "x-api-key: abc123 next"},
		// Bearer behavior unchanged alongside the new pattern.
		{"Bearer tok1 then x-api-key: sk-99887766aabb end", "Bearer [redacted] then x-api-key: [redacted] end"},
	}
	for _, tc := range tests {
		got := Redact(tc.in)
		if got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if again := Redact(got); again != got {
			t.Errorf("Redact not idempotent on %q: %q", got, again)
		}
	}
}

func TestRedactQueryKeys(t *testing.T) {
	tests := []struct{ in, want string }{
		// Transport-failure URL echo with a query-carried key.
		{`Get "https://gw.example/v1?api_key=SECRET9key": dial tcp 1.2.3.4:443`, `Get "https://gw.example/v1?api_key=[redacted]": dial tcp 1.2.3.4:443`},
		// Parameter name kept (all alias spellings), value redacted.
		{"bad url ?key=v9alue trailing", "bad url ?key=[redacted] trailing"},
		{"fail ?access_token=abc123def456 end", "fail ?access_token=[redacted] end"},
		{"hyphen url ?api-key=zzz999yyy888 done", "hyphen url ?api-key=[redacted] done"},
		// Consecutive params survive; only keyed values redacted.
		{"https://gw.example/v1?api_key=S3cret&model=gpt&token=tok999 still here",
			"https://gw.example/v1?api_key=[redacted]&model=gpt&token=[redacted] still here"},
		// Unrelated params untouched.
		{"https://gw.example/v1?page=2&q=quota+limit", "https://gw.example/v1?page=2&q=quota+limit"},
	}
	for _, tc := range tests {
		got := Redact(tc.in)
		if got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if again := Redact(got); again != got {
			t.Errorf("Redact not idempotent on %q: %q", got, again)
		}
	}
}
