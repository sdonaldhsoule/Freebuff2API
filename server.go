package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Server struct {
	cfg      Config
	logger   *log.Logger
	client   *UpstreamClient
	runs     *RunManager
	registry *ModelRegistry
	accounts *AccountManager
	sessions *SessionManager
	started  time.Time
}

func NewServer(
	cfg Config,
	logger *log.Logger,
	registry *ModelRegistry,
	client *UpstreamClient,
	runs *RunManager,
	accounts *AccountManager,
	sessions *SessionManager,
) *Server {
	return &Server{
		cfg:      cfg,
		logger:   logger,
		client:   client,
		runs:     runs,
		registry: registry,
		accounts: accounts,
		sessions: sessions,
		started:  time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/healthz", s.requireAPIKey(http.HandlerFunc(s.handleHealthz)))
	mux.Handle("/v1/models", s.requireAPIKey(http.HandlerFunc(s.handleModels)))
	mux.Handle("/v1/chat/completions", s.requireAPIKey(http.HandlerFunc(s.handleChatCompletions)))

	if s.sessions != nil && s.sessions.Enabled() {
		mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("/", s.handleIndex)
		mux.HandleFunc("/app.css", s.handleCSS)
		mux.HandleFunc("/app.js", s.handleJS)
		mux.HandleFunc("/web/login", s.handleWebLogin)
		mux.HandleFunc("/web/logout", s.handleWebLogout)
		mux.HandleFunc("/web/session", s.handleWebSession)
		mux.Handle("/web/api/healthz", s.requireSession(http.HandlerFunc(s.handleWebHealthz)))
		mux.Handle("/web/api/models", s.requireSession(http.HandlerFunc(s.handleWebModels)))
		mux.Handle("/web/api/chat/completions", s.requireSession(http.HandlerFunc(s.handleWebChatCompletions)))
		mux.Handle("/web/api/accounts", s.requireSession(http.HandlerFunc(s.handleWebAccountsCollection)))
		mux.Handle("/web/api/accounts/", s.requireSession(http.HandlerFunc(s.handleWebAccountItem)))
	}

	return mux
}

func (s *Server) Start(ctx context.Context) {
	s.runs.Start(ctx)
}

func (s *Server) Shutdown(ctx context.Context) {
	s.runs.Close(ctx)
}

func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	if len(s.cfg.APIKeys) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid proxy api key", "authentication_error", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.sessions == nil || !s.sessions.Enabled() {
			http.NotFound(w, r)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !s.sessions.Valid(cookie.Value) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{
					"message": "未登录或会话已过期",
					"type":    "authentication_error",
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		return false
	}
	apiKey := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	return containsString(s.cfg.APIKeys, apiKey)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	writeJSON(w, http.StatusOK, s.healthPayload(r.Context()))
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	writeJSON(w, http.StatusOK, s.modelsPayload())
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.proxyChatCompletions(w, r)
}

func (s *Server) proxyChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "")
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(requestBody, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error", "")
		return
	}

	requestedModel, _ := payload["model"].(string)
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		writeOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "")
		return
	}
	agentID, ok := s.registry.AgentForModel(requestedModel)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("unsupported model %q", requestedModel), "invalid_request_error", "model_not_found")
		return
	}

	s.accounts.RecordRequestStart()
	startTime := time.Now()

	for attempt := 0; attempt < 2; attempt++ {
		lease, err := s.runs.Acquire(r.Context(), agentID)
		if err != nil {
			s.accounts.RecordRequestFailure()
			writeOpenAIError(w, http.StatusBadGateway, "no healthy upstream auth token available", "server_error", "")
			return
		}
		s.accounts.RecordAccountAttempt(lease.pool.accountID)

		s.logger.Printf("[%s] Routing request (model: %s) via run: %s", lease.pool.label, requestedModel, lease.run.id)

		upstreamBody, err := s.injectUpstreamMetadata(payload, requestedModel, lease.run.id)
		if err != nil {
			s.accounts.RecordAccountFailure(lease.pool.accountID)
			s.accounts.RecordRequestFailure()
			s.runs.Release(lease)
			writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
			return
		}

		resp, errorBody, err := s.client.ChatCompletions(r.Context(), lease.pool.token, upstreamBody)
		if err != nil {
			s.accounts.RecordAccountFailure(lease.pool.accountID)
			s.accounts.RecordRequestFailure()
			s.runs.Release(lease)
			writeOpenAIError(w, http.StatusBadGateway, err.Error(), "server_error", "")
			return
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.accounts.RecordAccountSuccess(lease.pool.accountID)
			s.accounts.RecordRequestSuccess()
			defer resp.Body.Close()
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if err := copyResponseBody(w, resp.Body); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Printf("[%s] proxy response copy failed: %v", lease.pool.label, err)
			}
			s.logger.Printf("[%s] Request completed successfully in %v (status: %d)", lease.pool.label, time.Since(startTime).Round(time.Millisecond), resp.StatusCode)
			s.runs.Release(lease)
			return
		}

		if isRunInvalid(resp.StatusCode, errorBody) {
			s.accounts.RecordAccountFailure(lease.pool.accountID)
			s.logger.Printf("%s: run %s invalid, rotating and retrying", lease.pool.label, lease.run.id)
			s.runs.Invalidate(lease, strings.TrimSpace(string(errorBody)))
			s.runs.Release(lease)
			continue
		}

		s.accounts.RecordAccountFailure(lease.pool.accountID)
		s.accounts.RecordRequestFailure()
		if resp.StatusCode == http.StatusUnauthorized {
			reason := "upstream auth rejected token"
			s.runs.Cooldown(lease, 30*time.Minute, reason)
			s.accounts.MarkInvalid(context.Background(), lease.pool.accountID, reason)
		}

		s.runs.Release(lease)
		s.logger.Printf("[%s] upstream error response: %s", lease.pool.label, string(errorBody))
		writePassthroughError(w, resp.StatusCode, errorBody)
		return
	}

	s.accounts.RecordRequestFailure()
	writeOpenAIError(w, http.StatusBadGateway, "upstream run expired twice in a row", "server_error", "")
}

func (s *Server) injectUpstreamMetadata(payload map[string]any, requestedModel, runID string) ([]byte, error) {
	cloned := cloneMap(payload)
	cloned["model"] = requestedModel

	metadata, ok := cloned["codebuff_metadata"].(map[string]any)
	if !ok || metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["run_id"] = runID
	metadata["cost_mode"] = "free"
	metadata["client_id"] = generateClientSessionId()
	cloned["codebuff_metadata"] = metadata

	body, err := json.Marshal(cloned)
	if err != nil {
		return nil, fmt.Errorf("marshal upstream request: %w", err)
	}
	return body, nil
}

func (s *Server) healthPayload(ctx context.Context) map[string]any {
	payload := map[string]any{
		"ok":                    true,
		"started_at":            s.started.UTC(),
		"uptime_sec":            int(time.Since(s.started).Seconds()),
		"token_state":           s.runs.Snapshots(),
		"account_store_enabled": s.accounts.Enabled(),
	}

	stats, enabled, err := s.accounts.Stats(ctx)
	payload["stats_enabled"] = enabled
	if err != nil {
		s.logger.Printf("health payload stats failed: %v", err)
		payload["stats_error"] = err.Error()
		return payload
	}
	if enabled {
		payload["stats"] = stats
	}
	return payload
}

func (s *Server) modelsPayload() map[string]any {
	created := s.started.Unix()
	modelsList := s.registry.Models()
	models := make([]map[string]any, 0, len(modelsList))
	for _, model := range modelsList {
		models = append(models, map[string]any{
			"id":         model,
			"object":     "model",
			"created":    created,
			"owned_by":   "Freebuff-Go",
			"root":       model,
			"permission": []any{},
		})
	}

	return map[string]any{
		"object": "list",
		"data":   models,
	}
}

func (s *Server) handleWebLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
		return
	}
	var payload struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "请求体不是合法 JSON"}})
		return
	}
	if !s.sessions.Authenticate(strings.TrimSpace(payload.Password)) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "访问密码错误"}})
		return
	}

	token := s.sessions.Create()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
		Expires:  time.Now().Add(24 * time.Hour),
	})

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWebLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.Destroy(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWebSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
		return
	}
	authenticated := false
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		authenticated = s.sessions.Valid(cookie.Value)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":               s.sessions.Enabled(),
		"authenticated":         authenticated,
		"account_store_enabled": s.accounts.Enabled(),
	})
}

func (s *Server) handleWebHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
		return
	}
	writeJSON(w, http.StatusOK, s.healthPayload(r.Context()))
}

func (s *Server) handleWebModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
		return
	}
	writeJSON(w, http.StatusOK, s.modelsPayload())
}

func (s *Server) handleWebChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.proxyChatCompletions(w, r)
}

func (s *Server) handleWebAccountsCollection(w http.ResponseWriter, r *http.Request) {
	if !s.accounts.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "账号池存储未启用"}})
		return
	}

	switch r.Method {
	case http.MethodGet:
		accounts, err := s.accounts.List(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": accounts})
	case http.MethodPost:
		var payload struct {
			Label    string `json:"label"`
			Token    string `json:"token"`
			Enabled  bool   `json:"enabled"`
			Priority int    `json:"priority"`
			Weight   int    `json:"weight"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "请求体不是合法 JSON"}})
			return
		}
		account, err := s.accounts.Create(r.Context(), AccountInput{
			Label:    payload.Label,
			Token:    payload.Token,
			Enabled:  payload.Enabled,
			Priority: payload.Priority,
			Weight:   payload.Weight,
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"data": account})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
	}
}

func (s *Server) handleWebAccountItem(w http.ResponseWriter, r *http.Request) {
	if !s.accounts.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "账号池存储未启用"}})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/web/api/accounts/")
	path = strings.Trim(path, "/")
	if path == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "account not found"}})
		return
	}

	parts := strings.Split(path, "/")
	id := strings.TrimSpace(parts[0])
	if id == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "account not found"}})
		return
	}

	if len(parts) == 2 && parts[1] == "validate" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
			return
		}
		account, err := s.accounts.Validate(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": account})
		return
	}

	if len(parts) != 1 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "account not found"}})
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var payload struct {
			Label    *string `json:"label"`
			Token    *string `json:"token"`
			Enabled  *bool   `json:"enabled"`
			Priority *int    `json:"priority"`
			Weight   *int    `json:"weight"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "请求体不是合法 JSON"}})
			return
		}
		account, err := s.accounts.Update(r.Context(), id, AccountUpdateInput{
			Label:    payload.Label,
			Token:    payload.Token,
			Enabled:  payload.Enabled,
			Priority: payload.Priority,
			Weight:   payload.Weight,
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": account})
	case http.MethodDelete:
		if err := s.accounts.Delete(r.Context(), id); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
	}
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[key] = cloneMap(typed)
		case []any:
			output[key] = cloneSlice(typed)
		default:
			output[key] = value
		}
	}
	return output
}

func cloneSlice(input []any) []any {
	output := make([]any, len(input))
	for index, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[index] = cloneMap(typed)
		case []any:
			output[index] = cloneSlice(typed)
		default:
			output[index] = value
		}
	}
	return output
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyResponseBody(w http.ResponseWriter, body io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func isRunInvalid(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(string(body))
	return strings.Contains(message, "runid not found") || strings.Contains(message, "runid not running")
}

func writePassthroughError(w http.ResponseWriter, statusCode int, body []byte) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && json.Valid(trimmed) {
		message, errorType, code := extractUpstreamError(trimmed)
		writeOpenAIError(w, statusCode, message, errorType, code)
		return
	}
	writeOpenAIError(w, statusCode, strings.TrimSpace(string(trimmed)), "upstream_error", "")
}

func extractUpstreamError(body []byte) (message, errorType, code string) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return strings.TrimSpace(string(body)), "upstream_error", ""
	}

	errorType = "upstream_error"

	if rawError, ok := payload["error"]; ok {
		switch typed := rawError.(type) {
		case string:
			code = typed
		case map[string]any:
			if value, ok := typed["message"].(string); ok && strings.TrimSpace(value) != "" {
				message = value
			}
			if value, ok := typed["type"].(string); ok && strings.TrimSpace(value) != "" {
				errorType = value
			}
			if value, ok := typed["code"].(string); ok && strings.TrimSpace(value) != "" {
				code = value
			}
		}
	}

	if value, ok := payload["message"].(string); ok && strings.TrimSpace(value) != "" {
		message = value
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	return message, errorType, code
}

func writeOpenAIError(w http.ResponseWriter, statusCode int, message, errorType, code string) {
	if message == "" {
		message = http.StatusText(statusCode)
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
		},
	}
	if code != "" {
		payload["error"].(map[string]any)["code"] = code
	}
	writeJSON(w, statusCode, payload)
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":{"message":"failed to encode response","type":"server_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}

func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
