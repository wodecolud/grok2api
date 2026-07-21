package cli

import (
	"net/http"
	"os"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"golang.org/x/mod/semver"
)

const (
	// grokCLIProxyHost is the subscription OAuth endpoint. Requests to this host
	// must identify as a supported Grok CLI client or xAI returns
	// chat-endpoint permission-denied (HTTP 403).
	grokCLIProxyHost = "cli-chat-proxy.grok.com"

	defaultGrokCLITokenAuth        = "xai-grok-cli"
	defaultGrokCLIClientIdentifier = "grok-shell"
	defaultGrokCLIClientMode       = "headless"
	// historicalGrokCLIMinimum is the oldest CLI version known to be accepted by
	// the subscription proxy. Matches the Sub2API transport-boundary baseline.
	historicalGrokCLIMinimum  = "0.2.93"
	grokCLIVersionOverrideEnv = "XAI_GROK_CLI_VERSION"
)

// ensureGrokCLIProxyIdentity applies the official Grok Build client identity at
// the final shared transport boundary. Keying on the exact CLI proxy host keeps
// direct api.x.ai traffic unchanged and covers Responses, Chat Completions,
// media, quota probes, and account tests even when a call site forgets
// applyHeaders.
//
// Existing non-empty headers from applyHeaders win; this only fills gaps so a
// missing X-XAI-Token-Auth cannot slip through as an anonymous/wrong credential
// type.
func ensureGrokCLIProxyIdentity(req *http.Request) {
	if req == nil || req.URL == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.URL.Hostname()), grokCLIProxyHost) {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}

	version := resolveGrokCLIVersion(req.Header.Get("x-grok-client-version"))

	if strings.TrimSpace(req.Header.Get("X-XAI-Token-Auth")) == "" {
		req.Header.Set("X-XAI-Token-Auth", defaultGrokCLITokenAuth)
	}
	if strings.TrimSpace(req.Header.Get("x-grok-client-version")) == "" {
		req.Header.Set("x-grok-client-version", version)
	}
	if strings.TrimSpace(req.Header.Get("x-grok-client-identifier")) == "" {
		req.Header.Set("x-grok-client-identifier", defaultGrokCLIClientIdentifier)
	}
	if strings.TrimSpace(req.Header.Get("x-grok-client-mode")) == "" {
		req.Header.Set("x-grok-client-mode", defaultGrokCLIClientMode)
	}
	if strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		req.Header.Set("User-Agent", "grok-shell/"+version+" (linux; x86_64)")
	}
}

func resolveGrokCLIVersion(current string) string {
	if isSupportedGrokCLIVersion(current) {
		return strings.TrimSpace(current)
	}
	if override := strings.TrimSpace(os.Getenv(grokCLIVersionOverrideEnv)); isSupportedGrokCLIVersion(override) {
		return override
	}
	if isSupportedGrokCLIVersion(config.RecommendedBuildClientVersion) {
		return config.RecommendedBuildClientVersion
	}
	return historicalGrokCLIMinimum
}

func isSupportedGrokCLIVersion(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	// Require bare SemVer (no leading "v"), matching the Sub2API transport baseline.
	canonical := "v" + version
	minimum := "v" + historicalGrokCLIMinimum
	return semver.IsValid(canonical) &&
		semver.Canonical(canonical) == canonical &&
		semver.Prerelease(canonical) == "" &&
		semver.Compare(canonical, minimum) >= 0
}
