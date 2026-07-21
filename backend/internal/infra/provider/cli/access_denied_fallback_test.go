package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGrokAccessDeniedFallbackRecognizesChatEndpointPermissionDenied(t *testing.T) {
	var hosts []string
	var fallbackAuth string
	var fallbackTokenAuth string
	transport := &grokAccessDeniedFallbackTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hosts = append(hosts, req.URL.Hostname())
			if req.URL.Hostname() == grokCLIProxyHost {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please log into console.x.ai and update the permissions, or contact support."}`,
					)),
					Request: req,
				}, nil
			}
			fallbackAuth = req.Header.Get("Authorization")
			fallbackTokenAuth = req.Header.Get("X-XAI-Token-Auth")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"id":"response-ok"}`)),
				Request:    req,
			}, nil
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", strings.NewReader(`{"model":"grok-4.5"}`))
	payload := []byte(`{"model":"grok-4.5"}`)
	if err := attachReplayableBody(req, payload); err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer oauth-token")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", "0.2.106")
	req.Header.Set("User-Agent", "grok-shell/0.2.106 (linux; x86_64)")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(hosts) != 2 || hosts[0] != grokCLIProxyHost || hosts[1] != grokOfficialAPIHost {
		t.Fatalf("hosts = %#v", hosts)
	}
	if fallbackAuth != "Bearer oauth-token" {
		t.Fatalf("fallback auth = %q", fallbackAuth)
	}
	if fallbackTokenAuth != "" {
		t.Fatalf("fallback must strip X-XAI-Token-Auth, got %q", fallbackTokenAuth)
	}
}

func TestIsGrokCLICompatibilityAccessDenied(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "legacy compatibility wording", body: `{"error":"Access denied"}`, want: true},
		{
			name: "observed chat endpoint permission denial hyphen code",
			body: `{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please log into console.x.ai and update the permissions, or contact support."}`,
			want: true,
		},
		{
			name: "observed chat endpoint permission denial underscore code",
			body: `{"code":"permission_denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please contact support."}`,
			want: true,
		},
		{
			name: "entitlement denial using the same broad terms",
			body: `{"code":"permission_denied","error":"Access to the chat endpoint is denied because a subscription is required"}`,
			want: false,
		},
		{
			name: "different permission denied endpoint",
			body: `{"code":"permission_denied","error":"Access to the billing endpoint is denied."}`,
			want: false,
		},
		{
			name: "wrong structured error code",
			body: `{"code":"subscription_required","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please contact support."}`,
			want: false,
		},
		{name: "malformed response", body: `permission_denied: chat endpoint denied`, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGrokCLICompatibilityAccessDenied([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestIsGrokCLIAccessDeniedFallbackCandidateRequiresAuthenticatedReplayableCLI403(t *testing.T) {
	newRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", strings.NewReader(`{}`))
		_ = attachReplayableBody(req, []byte(`{}`))
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
		return req
	}
	newResponse := func() *http.Response {
		return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody}
	}
	if !isGrokCLIAccessDeniedFallbackCandidate(newRequest(), newResponse()) {
		t.Fatal("expected candidate")
	}
	req := newRequest()
	req.URL.Host = "api.x.ai"
	if isGrokCLIAccessDeniedFallbackCandidate(req, newResponse()) {
		t.Fatal("api.x.ai must not be a candidate")
	}
	req = newRequest()
	req.Header.Del("X-XAI-Token-Auth")
	if isGrokCLIAccessDeniedFallbackCandidate(req, newResponse()) {
		t.Fatal("missing token auth must not be a candidate")
	}
	req = newRequest()
	req.GetBody = nil
	if isGrokCLIAccessDeniedFallbackCandidate(req, newResponse()) {
		t.Fatal("non-replayable body must not be a candidate")
	}
}

func TestAccessDeniedFallbackKeepsEntitlementDenial(t *testing.T) {
	var hits int
	transport := &grokAccessDeniedFallbackTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hits++
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body: io.NopCloser(strings.NewReader(
					`{"code":"permission_denied","error":"Access to the chat endpoint is denied because a subscription is required"}`,
				)),
				Request: req,
			}, nil
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", strings.NewReader(`{}`))
	_ = attachReplayableBody(req, []byte(`{}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (no api.x.ai fallback)", hits)
	}
}
