package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

const (
	chatAccessProbeTimeout   = 45 * time.Second
	chatAccessProbeDefaultModel = "grok-4"
	chatAccessProbeMaxBody   = 64 << 10
)

// ChatAccessProbeReport 是 Grok Build chat 403 批量检测结果。
type ChatAccessProbeReport struct {
	Checked int `json:"checked"`
	Deleted int `json:"deleted"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	Workers int `json:"workers"`
}

// ProbeBuildChatAccessAndDelete 通过上游 chat 代理（含出口代理）探测 Build 账号。
// 命中 chat 端点 403/permission-denied 时直接删除该 Grok Build 账号。
//
// 并发复用账号服务的 runAccountBatch + syncPool（与 Billing/额度批量同步同一套有界线程池；
// 生产环境默认 Batch.SyncConcurrency=25，并受 bulk 全局上限约束）。
func (s *Service) ProbeBuildChatAccessAndDelete(ctx context.Context, ids []uint64, progress BatchProgressObserver) (ChatAccessProbeReport, error) {
	if s.providers == nil {
		return ChatAccessProbeReport{}, fmt.Errorf("Provider 注册表未初始化")
	}
	if _, ok := s.providers.Responses(accountdomain.ProviderBuild); !ok {
		return ChatAccessProbeReport{}, fmt.Errorf("Grok Build Provider 未注册")
	}

	values, skipped, err := s.resolveBuildProbeAccountIDs(ctx, ids)
	if err != nil {
		return ChatAccessProbeReport{}, err
	}
	return s.probeBuildChatAccessAccounts(ctx, values, skipped, progress)
}

// probeBuildChatAccessAccounts 与 refreshBillings/BatchRefreshQuota 一样走 runAccountBatch 多 worker 并发。
func (s *Service) probeBuildChatAccessAccounts(ctx context.Context, ids []uint64, skipped int, progress BatchProgressObserver) (ChatAccessProbeReport, error) {
	pool := s.syncPool
	workers := 0
	if pool != nil {
		workers = pool.Limit()
	}
	report := ChatAccessProbeReport{Skipped: skipped, Workers: workers}
	if len(ids) == 0 {
		if progress != nil {
			_ = progress(0, 0)
		}
		return report, nil
	}

	var deleted atomic.Int64
	// 与额度同步相同：batch.MapObserved 启多 worker，再经 syncPool 做有界并发。
	succeeded, failed, batchErr := s.runAccountBatch(ctx, "build_chat_access_probe", ids, pool, progress, func(workCtx context.Context, id uint64) error {
		removed, probeErr := s.probeBuildChatAccessAndDeleteOne(workCtx, id)
		if probeErr != nil {
			return probeErr
		}
		if removed {
			deleted.Add(1)
		}
		return nil
	})
	report.Checked = succeeded + failed
	report.Deleted = int(deleted.Load())
	report.Failed = failed
	return report, batchErr
}

func (s *Service) resolveBuildProbeAccountIDs(ctx context.Context, ids []uint64) ([]uint64, int, error) {
	if len(ids) == 0 {
		enabled, err := s.accounts.ListEnabledAccountIDs(ctx, accountdomain.ProviderBuild, false)
		if err != nil {
			return nil, 0, err
		}
		return enabled, 0, nil
	}
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return nil, 0, err
	}
	ok, err := s.AccountsBelongToProvider(ctx, values, accountdomain.ProviderBuild)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, invalidInput("仅支持检测 Grok Build 账号")
	}
	probeIDs := make([]uint64, 0, len(values))
	skipped := 0
	for _, id := range values {
		credential, getErr := s.accounts.Get(ctx, id)
		if getErr != nil {
			return nil, 0, mapRepositoryError(getErr)
		}
		if credential.Provider != accountdomain.ProviderBuild || !credential.Enabled {
			skipped++
			continue
		}
		probeIDs = append(probeIDs, id)
	}
	return probeIDs, skipped, nil
}

func (s *Service) probeBuildChatAccessAndDeleteOne(ctx context.Context, id uint64) (bool, error) {
	credential, err := s.accounts.Get(ctx, id)
	if err != nil {
		return false, mapRepositoryError(err)
	}
	if credential.Provider != accountdomain.ProviderBuild || !credential.Enabled {
		return false, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, chatAccessProbeTimeout)
	defer cancel()

	credential, err = s.EnsureCredential(probeCtx, credential, false)
	if err != nil {
		// 凭据已永久失效：直接删除，避免号池长期堆积无效账号。
		if errorsIsPermanentRefresh(err) {
			return true, s.deleteProbedBuildAccount(probeCtx, id, "chat access probe: credential permanently unrefreshable")
		}
		return false, err
	}

	adapter, ok := s.providers.Responses(accountdomain.ProviderBuild)
	if !ok {
		return false, fmt.Errorf("Grok Build Provider 未注册")
	}
	var billing *accountdomain.Billing
	if value, billingErr := s.accounts.GetBilling(probeCtx, id); billingErr == nil {
		billing = &value
	}
	model := chatAccessProbeModel(credential)
	requestBody := []byte(fmt.Sprintf(`{"model":%q,"input":"ping","max_output_tokens":1,"stream":false}`, model))
	response, err := adapter.ForwardResponse(probeCtx, provider.ResponseResourceRequest{
		Credential:    credential,
		Billing:       billing,
		Method:        http.MethodPost,
		Path:          "/responses",
		Body:          requestBody,
		Operation:     conversation.OperationResponses,
		NormalizeBody: true,
		Model:         model,
	})
	if err != nil {
		if status, ok := provider.ErrorHTTPStatus(err); ok && isChatAccessDeniedStatus(status, nil) {
			return true, s.deleteProbedBuildAccount(probeCtx, id, fmt.Sprintf("chat access probe denied: http %d", status))
		}
		return false, err
	}
	body, _ := readProbeResponseBody(response)
	if isChatAccessDeniedStatus(response.StatusCode, body) {
		reason := fmt.Sprintf("chat access probe denied: http %d", response.StatusCode)
		if code := probeUpstreamCode(body); code != "" {
			reason = fmt.Sprintf("chat access probe denied: %s", code)
		}
		return true, s.deleteProbedBuildAccount(probeCtx, id, reason)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return false, nil
	}
	// 非 403 的上游错误记为检测失败，不删除，避免把额度/限流误杀。
	return false, fmt.Errorf("chat access probe upstream status %d", response.StatusCode)
}

func (s *Service) deleteProbedBuildAccount(ctx context.Context, id uint64, reason string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	s.logger.Info("build_chat_access_probe_delete", "account_id", id, "reason", reason)
	if err := s.Delete(writeCtx, id); err != nil {
		s.logger.Error("build_chat_access_probe_delete_failed", "account_id", id, "error", err)
		return err
	}
	return nil
}

func chatAccessProbeModel(credential accountdomain.Credential) string {
	model := strings.TrimSpace(credential.ObservedModel)
	if model == "" {
		return chatAccessProbeDefaultModel
	}
	return model
}

func readProbeResponseBody(response *provider.Response) ([]byte, error) {
	if response == nil {
		return nil, nil
	}
	if response.Diagnostic != nil && len(response.Diagnostic.Body) > 0 {
		return response.Diagnostic.Body, nil
	}
	if response.Body == nil {
		return nil, nil
	}
	defer response.Body.Close()
	return io.ReadAll(io.LimitReader(response.Body, chatAccessProbeMaxBody))
}

func isChatAccessDeniedStatus(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(string(body)))
	if text == "" {
		// 空 body 的 403 在 chat 探测场景下也视为端点拒绝，便于清理无权限号。
		return true
	}
	if strings.Contains(text, "access to the chat endpoint is denied") {
		return true
	}
	if strings.Contains(text, "permission-denied") || strings.Contains(text, "permission_denied") {
		return true
	}
	// 部分兼容路径只保留 permission / credentials 文案。
	if strings.Contains(text, "chat endpoint") && strings.Contains(text, "denied") {
		return true
	}
	if strings.Contains(text, "correct credentials") && strings.Contains(text, "denied") {
		return true
	}
	return false
}

func probeUpstreamCode(body []byte) string {
	text := strings.ToLower(string(body))
	switch {
	case strings.Contains(text, "permission-denied"):
		return "permission-denied"
	case strings.Contains(text, "permission_denied"):
		return "permission_denied"
	default:
		return ""
	}
}

func errorsIsPermanentRefresh(err error) bool {
	return errors.Is(err, ErrCredentialRefreshPermanent)
}
