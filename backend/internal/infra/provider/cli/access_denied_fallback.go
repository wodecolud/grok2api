package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

const (
	grokOfficialAPIHost   = "api.x.ai"
	grokFallbackBodyLimit = 64 << 10
)

// grokAccessDeniedFallbackTransport keeps cli-chat-proxy as the primary OAuth
// route, but retries a replayable request against api.x.ai when the proxy
// returns its compatibility-specific 403 permission-denied body.
//
// Trial / mismatched-plane subscriptions can hit this boundary while the same
// OAuth credential remains valid on the official API. Real entitlement failures
// stay on the original response so account scheduling is unchanged.
//
// Mirrors Sub2API's transport-boundary behavior after PR #4009.
type grokAccessDeniedFallbackTransport struct {
	base http.RoundTripper
}

func (t *grokAccessDeniedFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || !isGrokCLIAccessDeniedFallbackCandidate(req, resp) {
		return resp, err
	}

	body, ok := bufferSmallResponseBody(resp, grokFallbackBodyLimit)
	if !ok || !isGrokCLICompatibilityAccessDenied(body) {
		return resp, nil
	}

	fallbackReq, err := newGrokOfficialAPIFallbackRequest(req)
	if err != nil {
		return resp, nil
	}
	fallbackResp, fallbackErr := base.RoundTrip(fallbackReq)
	if fallbackErr != nil {
		slog.Debug("grok_cli_access_denied_api_fallback_failed", "path", req.URL.EscapedPath(), "error", fallbackErr)
		return resp, nil
	}
	if fallbackResp.StatusCode < http.StatusOK || fallbackResp.StatusCode >= http.StatusMultipleChoices {
		if fallbackResp.Body != nil {
			_ = fallbackResp.Body.Close()
		}
		return resp, nil
	}

	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	slog.Warn("grok_cli_access_denied_api_fallback_succeeded", "method", req.Method, "path", req.URL.EscapedPath())
	return fallbackResp, nil
}

func isGrokCLICompatibilityAccessDenied(body []byte) bool {
	lower := bytes.ToLower(body)
	// Contiguous "access denied" covers the legacy proxy wording only.
	// Phrases like "access to the chat endpoint is denied ..." do not match.
	if bytes.Contains(lower, []byte("access denied")) {
		return true
	}
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(payload.Code))
	if code != "permission_denied" && code != "permission-denied" {
		return false
	}
	const chatEndpointDeniedPrefix = "access to the chat endpoint is denied. please ensure you're using the correct credentials. if you believe this is a mistake, please"
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(payload.Error)), chatEndpointDeniedPrefix)
}

func isGrokCLIAccessDeniedFallbackCandidate(req *http.Request, resp *http.Response) bool {
	return req != nil && req.URL != nil && req.GetBody != nil && resp != nil &&
		resp.StatusCode == http.StatusForbidden &&
		strings.EqualFold(strings.TrimSpace(req.URL.Hostname()), grokCLIProxyHost) &&
		strings.EqualFold(strings.TrimSpace(req.Header.Get("X-XAI-Token-Auth")), "xai-grok-cli") &&
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(req.Header.Get("Authorization"))), "bearer ")
}

func newGrokOfficialAPIFallbackRequest(req *http.Request) (*http.Request, error) {
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	fallbackReq := req.Clone(req.Context())
	fallbackReq.Body = body
	fallbackReq.GetBody = req.GetBody
	fallbackReq.URL = cloneURL(req.URL)
	fallbackReq.URL.Scheme = "https"
	fallbackReq.URL.Host = grokOfficialAPIHost
	fallbackReq.Host = ""
	fallbackReq.RequestURI = ""
	fallbackReq.Header = req.Header.Clone()
	for _, header := range []string{
		"X-XAI-Token-Auth",
		"x-grok-client-version",
		"X-Grok-Client-Version",
		"x-grok-client-identifier",
		"x-grok-client-mode",
		"X-Grok-Client-Surface",
		"x-userid",
		"X-UserID",
		"x-email",
		"X-Email",
		"User-Agent",
	} {
		fallbackReq.Header.Del(header)
	}
	return fallbackReq, nil
}

func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func bufferSmallResponseBody(resp *http.Response, limit int64) ([]byte, bool) {
	if resp == nil || resp.Body == nil || limit <= 0 {
		return nil, false
	}
	original := resp.Body
	body, err := io.ReadAll(io.LimitReader(original, limit+1))
	if err != nil || int64(len(body)) > limit {
		resp.Body = &prefixedReadCloser{
			Reader: io.MultiReader(bytes.NewReader(body), original),
			Closer: original,
		}
		return nil, false
	}
	_ = original.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return body, true
}

type prefixedReadCloser struct {
	io.Reader
	io.Closer
}

// attachReplayableBody sets Body/GetBody/ContentLength so transport-level
// fallbacks can safely clone POST bodies (required by access-denied retry).
func attachReplayableBody(req *http.Request, body []byte) error {
	if req == nil {
		return nil
	}
	if len(body) == 0 {
		// Keep Body nil for GET/DELETE resource requests (tests and some
		// servers treat a non-nil empty body as unexpected).
		req.Body = nil
		req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		req.ContentLength = 0
		return nil
	}
	payload := append([]byte(nil), body...)
	req.Body = io.NopCloser(bytes.NewReader(payload))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	req.ContentLength = int64(len(payload))
	return nil
}
