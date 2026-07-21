package account

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestIsChatAccessDeniedStatus(t *testing.T) {
	body := []byte(`{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please log into console.x.ai and update the permissions, or contact support."}`)
	if !isChatAccessDeniedStatus(http.StatusForbidden, body) {
		t.Fatal("official permission-denied body should be denied")
	}
	if !isChatAccessDeniedStatus(http.StatusForbidden, nil) {
		t.Fatal("empty 403 should be denied for probe cleanup")
	}
	if isChatAccessDeniedStatus(http.StatusTooManyRequests, body) {
		t.Fatal("429 must not be treated as chat denial")
	}
	if isChatAccessDeniedStatus(http.StatusForbidden, []byte(`{"error":"upstream policy rejected request"}`)) {
		t.Fatal("generic policy body without permission signals should not delete")
	}
}

func TestProbeBuildChatAccessDeletesDeniedAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "chat-probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	deniedToken, err := cipher.Encrypt("denied-access")
	if err != nil {
		t.Fatal(err)
	}
	okToken, err := cipher.Encrypt("ok-access")
	if err != nil {
		t.Fatal(err)
	}
	denied, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "denied", SourceKey: "denied",
		EncryptedAccessToken: deniedToken, ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	healthy, _, err := accountRepo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "healthy", SourceKey: "healthy",
		EncryptedAccessToken: okToken, ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 90, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &chatProbeAdapter{}
	registry := provider.NewRegistry(adapter)
	service := NewService(accountRepo, nil, nil, memory.NewStickyStore(), registry, cipher, nil)

	report, err := service.ProbeBuildChatAccessAndDelete(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 2 || report.Deleted != 1 || report.Failed != 0 {
		t.Fatalf("report = %#v", report)
	}
	if _, err := accountRepo.Get(ctx, denied.ID); err == nil {
		t.Fatal("denied account should be deleted")
	}
	if _, err := accountRepo.Get(ctx, healthy.ID); err != nil {
		t.Fatalf("healthy account should remain: %v", err)
	}
	if adapter.calls.Load() < 2 {
		t.Fatalf("expected both accounts probed, calls=%d", adapter.calls.Load())
	}
}

type chatProbeAdapter struct {
	calls atomic.Int64
}

func (a *chatProbeAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }
func (a *chatProbeAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderBuild, ModelNamespace: accountdomain.ProviderBuild.ModelNamespace(),
		Credential:   provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth, Refresh: true},
		Conversation: provider.ConversationSurface{Responses: true},
		Quota:        provider.QuotaBilling,
	}
}
func (a *chatProbeAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.calls.Add(1)
	if request.Credential.Name == "denied" {
		return &provider.Response{
			StatusCode: http.StatusForbidden,
			Status:     "403 Forbidden",
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials."}`)),
		}, nil
	}
	return &provider.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_ok","status":"completed"}`)),
	}, nil
}
