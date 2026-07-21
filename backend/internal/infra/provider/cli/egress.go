package cli

import (
	"context"
	"io"
	"net/http"
	"strings"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

// cliIdentityTransport is the final shared transport boundary for Grok Build.
// It injects CLI OAuth identity for cli-chat-proxy.grok.com before any egress
// or default RoundTripper runs, and never mutates api.x.ai requests.
type cliIdentityTransport struct {
	next http.RoundTripper
}

func (t *cliIdentityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ensureGrokCLIProxyIdentity(request)
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(request)
}

type egressTransport struct {
	manager  *infraegress.Manager
	fallback http.RoundTripper
}

func (t *egressTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// Always re-assert CLI identity after any earlier mutation and before the
	// request leaves the process. Host-gated so api.x.ai stays untouched.
	ensureGrokCLIProxyIdentity(request)

	affinity := infraegress.AccountFromContext(request.Context())
	if affinity == "" {
		affinity = "bootstrap"
	}
	lease, configured, err := t.manager.AcquireIfConfigured(request.Context(), domainegress.ScopeBuild, affinity)
	if err != nil {
		return nil, err
	}
	if !configured {
		return t.fallback.RoundTrip(request)
	}
	// Build egress nodes must not override CLI User-Agent protocol identity.
	// ScopeBuild nodes are stored with empty UserAgent; keep the guard explicit.
	if lease.UserAgent != "" && !isGrokCLIProxyRequest(request) {
		request.Header.Set("User-Agent", lease.UserAgent)
	}
	// Re-apply after lease handling so a misconfigured Build node UA cannot
	// strip CLI identity for subscription OAuth traffic.
	ensureGrokCLIProxyIdentity(request)

	response, err := lease.Do(request)
	if err != nil {
		t.manager.FeedbackForScope(context.WithoutCancel(request.Context()), domainegress.ScopeBuild, lease.NodeID, 0, err)
		lease.Release()
		return nil, err
	}
	t.manager.FeedbackForScope(context.WithoutCancel(request.Context()), domainegress.ScopeBuild, lease.NodeID, response.StatusCode, nil)
	if response.Body == nil {
		lease.Release()
		return response, nil
	}
	response.Body = &egressResponseBody{ReadCloser: response.Body, release: lease.Release}
	return response, nil
}

func isGrokCLIProxyRequest(request *http.Request) bool {
	return request != nil && request.URL != nil &&
		strings.EqualFold(strings.TrimSpace(request.URL.Hostname()), grokCLIProxyHost)
}

type egressResponseBody struct {
	io.ReadCloser
	release func()
}

func (b *egressResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return err
}
