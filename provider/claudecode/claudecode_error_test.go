package claudecode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/bridle/fake"
)

func TestClassifyProviderError_AuthFailed(t *testing.T) {
	waitErr := errors.New("exit status 1")
	tests := []struct {
		name   string
		stderr string
	}{
		{"not logged in", "Not logged in. Please run /login."},
		{"authentication_failed", `{"type":"result","is_api_error":true,"error":"authentication_failed"}`},
		{"run /login", "Error: please run /login to authenticate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := classifyProviderError(tt.stderr, waitErr)
			if pe.Kind != bridle.ProviderErrorAuthFailed {
				t.Errorf("Kind = %q; want %q", pe.Kind, bridle.ProviderErrorAuthFailed)
			}
			if pe.Err != waitErr {
				t.Error("underlying error not preserved")
			}
			if pe.Message == "" {
				t.Error("Message is empty")
			}
			// The human-readable Message must not contain the raw exit code.
			if strings.Contains(pe.Message, "exit status") {
				t.Errorf("Message contains raw exit code: %q", pe.Message)
			}
			t.Logf("auth_failed message: %s", pe.Error())
		})
	}
}

func TestClassifyProviderError_RateLimit(t *testing.T) {
	waitErr := errors.New("exit status 1")
	pe := classifyProviderError("rate_limit: too many requests", waitErr)
	if pe.Kind != bridle.ProviderErrorRateLimit {
		t.Errorf("Kind = %q; want %q", pe.Kind, bridle.ProviderErrorRateLimit)
	}
	t.Logf("rate_limit message: %s", pe.Error())
}

func TestClassifyProviderError_ServerError(t *testing.T) {
	waitErr := errors.New("exit status 1")
	tests := []struct {
		name   string
		stderr string
	}{
		{"server_error", "server_error: internal error"},
		{"internal server error", "internal server error (500)"},
		{"overloaded", "overloaded_error: try again later"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := classifyProviderError(tt.stderr, waitErr)
			if pe.Kind != bridle.ProviderErrorServerError {
				t.Errorf("Kind = %q; want %q", pe.Kind, bridle.ProviderErrorServerError)
			}
		})
	}
}

func TestClassifyProviderError_NetworkError(t *testing.T) {
	waitErr := errors.New("exit status 1")
	tests := []struct {
		name   string
		stderr string
	}{
		{"connection refused", "Error: dial tcp 127.0.0.1:443: connection refused"},
		{"no route to host", "Error: no route to host"},
		{"connection reset", "Error: read tcp ... connection reset by peer"},
		{"eof", "Error: unexpected EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := classifyProviderError(tt.stderr, waitErr)
			if pe.Kind != bridle.ProviderErrorNetworkError {
				t.Errorf("Kind = %q; want %q", pe.Kind, bridle.ProviderErrorNetworkError)
			}
		})
	}
}

func TestClassifyProviderError_Timeout(t *testing.T) {
	waitErr := errors.New("exit status 1")
	tests := []struct {
		name   string
		stderr string
	}{
		{"timeout", "Error: context deadline exceeded (timeout)"},
		{"deadline exceeded", "Error: deadline exceeded"},
		{"timed out", "Error: request timed out after 30s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := classifyProviderError(tt.stderr, waitErr)
			if pe.Kind != bridle.ProviderErrorTimeout {
				t.Errorf("Kind = %q; want %q", pe.Kind, bridle.ProviderErrorTimeout)
			}
		})
	}
}

func TestClassifyProviderError_TLSError(t *testing.T) {
	waitErr := errors.New("exit status 1")
	tests := []struct {
		name   string
		stderr string
	}{
		{"certificate", "Error: x509: certificate signed by unknown authority"},
		{"ssl", "Error: SSL handshake failed"},
		{"tls", "Error: tls: protocol version not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := classifyProviderError(tt.stderr, waitErr)
			if pe.Kind != bridle.ProviderErrorTLSError {
				t.Errorf("Kind = %q; want %q", pe.Kind, bridle.ProviderErrorTLSError)
			}
		})
	}
}

func TestClassifyProviderError_GenericFallback(t *testing.T) {

	waitErr := errors.New("exit status 2")
	pe := classifyProviderError("some unexpected stderr output", waitErr)
	if pe.Kind != "subprocess_exit" {
		t.Errorf("Kind = %q; want subprocess_exit", pe.Kind)
	}
	if pe.Err != waitErr {
		t.Error("underlying error not preserved")
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		pe   *bridle.ProviderError
		want bool
	}{
		{"rate_limit", &bridle.ProviderError{Kind: bridle.ProviderErrorRateLimit, Message: "rate limited"}, true},
		{"server_error", &bridle.ProviderError{Kind: bridle.ProviderErrorServerError, Message: "server error"}, true},
		{"network_error", &bridle.ProviderError{Kind: bridle.ProviderErrorNetworkError, Message: "network error"}, true},
		{"timeout", &bridle.ProviderError{Kind: bridle.ProviderErrorTimeout, Message: "timed out"}, true},
		{"auth_failed", &bridle.ProviderError{Kind: bridle.ProviderErrorAuthFailed, Message: "auth failed"}, false},
		{"tls_error", &bridle.ProviderError{Kind: bridle.ProviderErrorTLSError, Message: "tls error"}, false},
		{"subprocess_exit", &bridle.ProviderError{Kind: "subprocess_exit", Message: "generic exit"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryable(tt.pe)
			if got != tt.want {
				t.Errorf("isRetryable(%q) = %v; want %v", tt.pe.Kind, got, tt.want)
			}
		})
	}

	// Non-ProviderError should not be retryable.
	if isRetryable(errors.New("some other error")) {
		t.Error("isRetryable on plain error should be false")
	}
}

func TestIsSessionIDInUseErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"exact match", errors.New("Session ID abc123 is already in use"), true},
		{"ANSI prefix", errors.New("\x1b[31mSession ID xyz is already in use\x1b[0m"), true},
		{"nil", nil, false},
		{"unrelated", errors.New("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSessionIDInUseErr(tt.err); got != tt.want {
				t.Errorf("isSessionIDInUseErr(%v) = %v; want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsAPIError(t *testing.T) {
	tests := []struct {
		name  string
		event map[string]json.RawMessage
		want  bool
	}{
		{
			name:  "snake_case true",
			event: map[string]json.RawMessage{"is_api_error": json.RawMessage("true")},
			want:  true,
		},
		{
			name:  "camelCase true",
			event: map[string]json.RawMessage{"isApiErrorMessage": json.RawMessage("true")},
			want:  true,
		},
		{
			name:  "snake_case false",
			event: map[string]json.RawMessage{"is_api_error": json.RawMessage("false")},
			want:  false,
		},
		{
			name:  "neither field",
			event: map[string]json.RawMessage{"type": json.RawMessage(`"assistant"`)},
			want:  false,
		},
		{
			name:  "camelCase false",
			event: map[string]json.RawMessage{"isApiErrorMessage": json.RawMessage("false")},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAPIError(tt.event)
			if got != tt.want {
				t.Errorf("isAPIError = %v; want %v", got, tt.want)
			}
		})
	}
}

// TestParseStream_APIErrorEvent verifies that parseStream emits a TurnError
// when the stream contains an event with is_api_error=true, and that the
// accumulated text content is preserved for the caller to classify.
func TestParseStream_APIErrorEvent(t *testing.T) {
	// Simulate the auth-failure stream: synthetic assistant message
	// followed by an API error event, no result event.
	stream := strings.NewReader(strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Not logged in. Please run /login."}]}}`,
		`{"type":"result","subtype":"error_during_execution","is_api_error":true,"error":"authentication_failed"}`,
	}, "\n"))

	sink := &fake.SliceEventSink{}
	result, err := parseStream(stream, sink)
	if err != nil {
		t.Fatalf("parseStream unexpected error: %v", err)
	}

	// parseStream should surface the assistant text.
	if !strings.Contains(result.FinalText, "Not logged in") {
		t.Errorf("FinalText = %q; expected it to contain 'Not logged in'", result.FinalText)
	}

	// Should have emitted a TurnError for the API error.
	var foundAPIError bool
	for _, e := range sink.Events {
		if te, ok := e.(bridle.TurnError); ok && te.Stage == "provider_api_error" {
			foundAPIError = true
			t.Logf("TurnError captured: %v", te.Err)
		}
	}
	if !foundAPIError {
		t.Error("expected TurnError with stage=provider_api_error to be emitted")
	}
	t.Logf("events emitted: %d", len(sink.Events))
}
