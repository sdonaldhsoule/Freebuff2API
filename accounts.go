package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

type RuntimeAccount struct {
	ID       string
	Label    string
	Token    string
	Enabled  bool
	Priority int
	Weight   int
}

type AccountView struct {
	ID              string    `json:"id"`
	Label           string    `json:"label"`
	TokenPreview    string    `json:"token_preview"`
	Enabled         bool      `json:"enabled"`
	Priority        int       `json:"priority"`
	Weight          int       `json:"weight"`
	LastStatus      string    `json:"last_status"`
	LastError       string    `json:"last_error"`
	LastCheckedAt   time.Time `json:"last_checked_at,omitempty"`
	TotalRequests   int64     `json:"total_requests"`
	SuccessRequests int64     `json:"success_requests"`
	FailedRequests  int64     `json:"failed_requests"`
	LastUsedAt      time.Time `json:"last_used_at,omitempty"`
	LastSuccessAt   time.Time `json:"last_success_at,omitempty"`
	LastFailureAt   time.Time `json:"last_failure_at,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type AccountManager struct {
	cfg    Config
	logger *log.Logger
	store  *AccountStore
	client *UpstreamClient
	runs   *RunManager

	legacyRecords []AccountRecord
}

func NewAccountManager(cfg Config, logger *log.Logger, store *AccountStore, client *UpstreamClient, runs *RunManager) *AccountManager {
	return &AccountManager{
		cfg:    cfg,
		logger: logger,
		store:  store,
		client: client,
		runs:   runs,
	}
}

func (m *AccountManager) Enabled() bool {
	return m != nil && m.store != nil
}

func (m *AccountManager) Stats(ctx context.Context) (RequestStats, bool, error) {
	if !m.Enabled() {
		return RequestStats{}, false, nil
	}
	stats, err := m.store.Stats(ctx)
	if err != nil {
		return RequestStats{}, true, err
	}
	return stats, true, nil
}

func (m *AccountManager) Bootstrap(ctx context.Context) error {
	if !m.Enabled() {
		m.legacyRecords = legacyAccountsFromTokens(m.cfg.AuthTokens)
		return m.runs.SyncAccounts(ctx, runtimeAccountsFromRecords(m.legacyRecords))
	}

	count, err := m.store.Count(ctx)
	if err != nil {
		return err
	}
	if count == 0 && len(m.cfg.AuthTokens) > 0 {
		for index, token := range m.cfg.AuthTokens {
			_, err := m.store.Create(ctx, AccountInput{
				Label:    fmt.Sprintf("迁移账号 %d", index+1),
				Token:    token,
				Enabled:  true,
				Priority: 100,
				Weight:   1,
			})
			if err != nil {
				return fmt.Errorf("导入 AUTH_TOKENS 失败: %w", err)
			}
		}
	}

	return m.RefreshRuntime(ctx)
}

func (m *AccountManager) RefreshRuntime(ctx context.Context) error {
	records, err := m.store.List(ctx)
	if err != nil {
		return err
	}
	return m.runs.SyncAccounts(ctx, runtimeAccountsFromRecords(records))
}

func (m *AccountManager) List(ctx context.Context) ([]AccountView, error) {
	if !m.Enabled() {
		views := make([]AccountView, 0, len(m.legacyRecords))
		for _, record := range m.legacyRecords {
			views = append(views, toAccountView(record))
		}
		return views, nil
	}

	records, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]AccountView, 0, len(records))
	for _, record := range records {
		views = append(views, toAccountView(record))
	}
	return views, nil
}

func (m *AccountManager) Create(ctx context.Context, input AccountInput) (AccountView, error) {
	if !m.Enabled() {
		return AccountView{}, fmt.Errorf("账号池存储未启用")
	}
	record, err := m.store.Create(ctx, input)
	if err != nil {
		return AccountView{}, err
	}
	if _, err := m.Validate(ctx, record.ID); err != nil {
		return AccountView{}, err
	}
	record, err = m.store.Get(ctx, record.ID)
	if err != nil {
		return AccountView{}, err
	}
	return toAccountView(record), nil
}

func (m *AccountManager) Update(ctx context.Context, id string, input AccountUpdateInput) (AccountView, error) {
	if !m.Enabled() {
		return AccountView{}, fmt.Errorf("账号池存储未启用")
	}
	record, err := m.store.Update(ctx, id, input)
	if err != nil {
		return AccountView{}, err
	}
	needsValidation := input.Token != nil
	if input.Enabled != nil && *input.Enabled {
		needsValidation = true
	}
	if needsValidation {
		if _, err := m.Validate(ctx, record.ID); err != nil {
			return AccountView{}, err
		}
	} else if err := m.RefreshRuntime(ctx); err != nil {
		return AccountView{}, err
	}
	record, err = m.store.Get(ctx, record.ID)
	if err != nil {
		return AccountView{}, err
	}
	return toAccountView(record), nil
}

func (m *AccountManager) Delete(ctx context.Context, id string) error {
	if !m.Enabled() {
		return fmt.Errorf("账号池存储未启用")
	}
	if err := m.store.Delete(ctx, id); err != nil {
		return err
	}
	return m.RefreshRuntime(ctx)
}

func (m *AccountManager) Validate(ctx context.Context, id string) (AccountView, error) {
	if !m.Enabled() {
		return AccountView{}, fmt.Errorf("账号池存储未启用")
	}

	record, err := m.store.Get(ctx, id)
	if err != nil {
		return AccountView{}, err
	}

	status := accountStatusHealthy
	lastError := ""
	if err := m.validateToken(ctx, record.Token); err != nil {
		status = classifyValidationError(err)
		lastError = truncateError(err.Error())
	}

	if err := m.store.UpdateValidation(ctx, id, status, lastError, time.Now().UTC()); err != nil {
		return AccountView{}, err
	}
	if err := m.RefreshRuntime(ctx); err != nil {
		return AccountView{}, err
	}

	record, err = m.store.Get(ctx, id)
	if err != nil {
		return AccountView{}, err
	}
	return toAccountView(record), nil
}

func (m *AccountManager) MarkInvalid(ctx context.Context, id, reason string) {
	if !m.Enabled() {
		return
	}
	if err := m.store.UpdateValidation(ctx, id, accountStatusInvalid, truncateError(reason), time.Now().UTC()); err != nil {
		m.logger.Printf("account manager: mark invalid %s failed: %v", id, err)
		return
	}
	if err := m.RefreshRuntime(ctx); err != nil {
		m.logger.Printf("account manager: refresh after invalid %s failed: %v", id, err)
	}
}

func (m *AccountManager) RecordRequestStart() {
	m.withStatsContext("record global request start", func(ctx context.Context) error {
		return m.store.RecordGlobalRequestStart(ctx)
	})
}

func (m *AccountManager) RecordRequestSuccess() {
	m.withStatsContext("record global request success", func(ctx context.Context) error {
		return m.store.RecordGlobalRequestSuccess(ctx)
	})
}

func (m *AccountManager) RecordRequestFailure() {
	m.withStatsContext("record global request failure", func(ctx context.Context) error {
		return m.store.RecordGlobalRequestFailure(ctx)
	})
}

func (m *AccountManager) RecordAccountAttempt(id string) {
	m.withStatsContext("record account request start", func(ctx context.Context) error {
		return m.store.RecordAccountRequestStart(ctx, id)
	})
}

func (m *AccountManager) RecordAccountSuccess(id string) {
	m.withStatsContext("record account request success", func(ctx context.Context) error {
		return m.store.RecordAccountRequestSuccess(ctx, id)
	})
}

func (m *AccountManager) RecordAccountFailure(id string) {
	m.withStatsContext("record account request failure", func(ctx context.Context) error {
		return m.store.RecordAccountRequestFailure(ctx, id)
	})
}

func (m *AccountManager) validateToken(ctx context.Context, token string) error {
	const validationAgentID = "editor-lite"

	runID, err := m.client.StartRun(ctx, token, validationAgentID)
	if err != nil {
		return err
	}

	finishCtx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
	defer cancel()
	return m.client.FinishRun(finishCtx, token, runID, 0)
}

func runtimeAccountsFromRecords(records []AccountRecord) []RuntimeAccount {
	accounts := make([]RuntimeAccount, 0, len(records))
	for _, record := range records {
		if !record.Enabled {
			continue
		}
		if record.LastStatus == accountStatusInvalid {
			continue
		}
		accounts = append(accounts, RuntimeAccount{
			ID:       record.ID,
			Label:    record.Label,
			Token:    record.Token,
			Enabled:  record.Enabled,
			Priority: record.Priority,
			Weight:   record.Weight,
		})
	}
	return accounts
}

func toAccountView(record AccountRecord) AccountView {
	return AccountView{
		ID:              record.ID,
		Label:           record.Label,
		TokenPreview:    record.TokenPreview,
		Enabled:         record.Enabled,
		Priority:        record.Priority,
		Weight:          record.Weight,
		LastStatus:      record.LastStatus,
		LastError:       record.LastError,
		LastCheckedAt:   record.LastCheckedAt,
		TotalRequests:   record.TotalRequests,
		SuccessRequests: record.SuccessRequests,
		FailedRequests:  record.FailedRequests,
		LastUsedAt:      record.LastUsedAt,
		LastSuccessAt:   record.LastSuccessAt,
		LastFailureAt:   record.LastFailureAt,
		CreatedAt:       record.CreatedAt,
		UpdatedAt:       record.UpdatedAt,
	}
}

func legacyAccountsFromTokens(tokens []string) []AccountRecord {
	records := make([]AccountRecord, 0, len(tokens))
	now := time.Now().UTC()
	for index, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		records = append(records, AccountRecord{
			ID:           fmt.Sprintf("legacy-%d", index+1),
			Label:        fmt.Sprintf("Legacy Token %d", index+1),
			Token:        token,
			TokenPreview: maskToken(token),
			Enabled:      true,
			Priority:     100,
			Weight:       1,
			LastStatus:   accountStatusUnknown,
			CreatedAt:    now,
			UpdatedAt:    now,
		})
	}
	return records
}

func classifyValidationError(err error) string {
	if err == nil {
		return accountStatusHealthy
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "401"),
		strings.Contains(message, "403"),
		strings.Contains(message, "unauthorized"),
		strings.Contains(message, "forbidden"),
		strings.Contains(message, "auth"),
		strings.Contains(message, "invalid"):
		return accountStatusInvalid
	default:
		return accountStatusError
	}
}

func truncateError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= 300 {
		return message
	}
	return message[:300]
}

func (m *AccountManager) withStatsContext(action string, fn func(ctx context.Context) error) {
	if !m.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fn(ctx); err != nil {
		m.logger.Printf("account manager: %s failed: %v", action, err)
	}
}
