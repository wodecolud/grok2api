package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestEnsureGrokCLIProxyIdentityFillsMissingHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)

	ensureGrokCLIProxyIdentity(req)

	if got := req.Header.Get("X-XAI-Token-Auth"); got != defaultGrokCLITokenAuth {
		t.Fatalf("X-XAI-Token-Auth = %q", got)
	}
	if got := req.Header.Get("x-grok-client-version"); got != config.RecommendedBuildClientVersion {
		t.Fatalf("x-grok-client-version = %q", got)
	}
	if got := req.Header.Get("x-grok-client-identifier"); got != defaultGrokCLIClientIdentifier {
		t.Fatalf("x-grok-client-identifier = %q", got)
	}
	if got := req.Header.Get("x-grok-client-mode"); got != defaultGrokCLIClientMode {
		t.Fatalf("x-grok-client-mode = %q", got)
	}
	wantUA := "grok-shell/" + config.RecommendedBuildClientVersion + " (linux; x86_64)"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
}

func TestEnsureGrokCLIProxyIdentityPreservesExistingHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/chat/completions", nil)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", "0.2.102")
	req.Header.Set("x-grok-client-identifier", "custom-id")
	req.Header.Set("x-grok-client-mode", "headless")
	req.Header.Set("User-Agent", "grok-shell/0.2.102 (linux; x86_64)")

	ensureGrokCLIProxyIdentity(req)

	if got := req.Header.Get("x-grok-client-version"); got != "0.2.102" {
		t.Fatalf("version overwritten: %q", got)
	}
	if got := req.Header.Get("x-grok-client-identifier"); got != "custom-id" {
		t.Fatalf("identifier overwritten: %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "grok-shell/0.2.102 (linux; x86_64)" {
		t.Fatalf("user-agent overwritten: %q", got)
	}
}

func TestEnsureGrokCLIProxyIdentitySkipsAPIKeyHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://api.x.ai/v1/responses", nil)

	ensureGrokCLIProxyIdentity(req)

	if got := req.Header.Get("X-XAI-Token-Auth"); got != "" {
		t.Fatalf("api.x.ai must not receive X-XAI-Token-Auth, got %q", got)
	}
	if got := req.Header.Get("x-grok-client-version"); got != "" {
		t.Fatalf("api.x.ai must not receive client version, got %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "" {
		t.Fatalf("api.x.ai must not receive injected User-Agent, got %q", got)
	}
}

func TestCLIIdentityTransportInjectsBeforeNext(t *testing.T) {
	var seen http.Header
	next := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = request.Header.Clone()
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: request}, nil
	})
	transport := &cliIdentityTransport{next: next}
	req := httptest.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := seen.Get("X-XAI-Token-Auth"); got != defaultGrokCLITokenAuth {
		t.Fatalf("transport X-XAI-Token-Auth = %q", got)
	}
}

func TestIsSupportedGrokCLIVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{version: "0.2.93", want: true},
		{version: "0.2.106", want: true},
		{version: "0.2.92", want: false},
		{version: "v0.2.106", want: false},
		{version: "0.2.106-beta", want: false},
		{version: "", want: false},
		{version: "latest", want: false},
	}
	for _, tc := range tests {
		if got := isSupportedGrokCLIVersion(tc.version); got != tc.want {
			t.Fatalf("isSupportedGrokCLIVersion(%q)=%v want %v", tc.version, got, tc.want)
		}
	}
}

func TestApplyHeadersDefaultsEmptyTokenAuth(t *testing.T) {
	adapter := NewAdapter(Config{
		BaseURL: "https://cli-chat-proxy.grok.com/v1",
	}, nil)
	req := httptest.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
	if err := adapter.applyHeaders(req, accountCredentialForIdentityTest(), "token", "grok-4.5", "", false); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-XAI-Token-Auth"); got != defaultGrokCLITokenAuth {
		t.Fatalf("TokenAuth default = %q", got)
	}
	if got := req.Header.Get("x-grok-client-version"); got != config.RecommendedBuildClientVersion {
		t.Fatalf("client version default = %q", got)
	}
	if got := req.Header.Get("User-Agent"); got == "" {
		t.Fatal("User-Agent empty")
	}
}

func accountCredentialForIdentityTest() account.Credential {
	return account.Credential{}
}
