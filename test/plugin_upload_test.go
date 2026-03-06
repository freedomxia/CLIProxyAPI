package test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func newPluginTestHandler(t *testing.T) (*management.Handler, string) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := &config.Config{
		AuthDir: filepath.Join(tmpDir, "auths"),
	}
	if err := os.MkdirAll(cfg.AuthDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth-dir: "+cfg.AuthDir+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	return management.NewHandler(cfg, configPath, nil), cfg.AuthDir
}

func setupPluginRouter(h *management.Handler) *gin.Engine {
	r := gin.New()
	plugin := r.Group("/api/plugin")
	{
		plugin.GET("/status", h.GetPluginStatus)
		plugin.POST("/update-token", h.UpdatePluginToken)
		plugin.POST("/check-tokens", h.CheckPluginTokens)
	}
	mgmt := r.Group("/v0/management")
	{
		mgmt.GET("/plugin-connection-token", h.GetPluginConnectionToken)
		mgmt.PUT("/plugin-connection-token", h.PutPluginConnectionToken)
		mgmt.PATCH("/plugin-connection-token", h.PutPluginConnectionToken)
		mgmt.DELETE("/plugin-connection-token", h.DeletePluginConnectionToken)
	}
	return r
}

func TestPluginStatusDisabledWithoutToken(t *testing.T) {
	h, _ := newPluginTestHandler(t)
	r := setupPluginRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/api/plugin/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["enabled"] {
		t.Fatalf("expected plugin endpoint to be disabled without env token")
	}
}

func TestUpdatePluginTokenRequiresConfiguredToken(t *testing.T) {
	h, _ := newPluginTestHandler(t)
	r := setupPluginRouter(h)

	body := `{"token":"secret","credential":{"refresh_token":"rt"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, w.Code)
	}
}

func TestUpdatePluginTokenRejectsInvalidToken(t *testing.T) {
	t.Setenv("PLUGIN_CONNECTION_TOKEN", "expected")
	h, _ := newPluginTestHandler(t)
	r := setupPluginRouter(h)

	body := `{"token":"wrong","credential":{"refresh_token":"rt"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestUpdatePluginTokenCreatesAndUpdatesGeminiCredential(t *testing.T) {
	h, authDir := newPluginTestHandler(t)
	h.SetConfig(&config.Config{
		AuthDir:               authDir,
		PluginConnectionToken: "expected",
	})
	r := setupPluginRouter(h)

	firstBody := `{
		"token":"expected",
		"mode":"geminicli",
		"name":"Gemini Import",
		"credential":{
			"client_id":"plugin-client",
			"client_secret":"plugin-secret",
			"token":"access-1",
			"refresh_token":"refresh-1",
			"project_id":"proj-1",
			"expiry":"2030-01-02T03:04:05Z"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(firstBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}
	if resp["action"] != "created" {
		t.Fatalf("expected action created, got %#v", resp["action"])
	}
	if success, _ := resp["success"].(bool); !success {
		t.Fatalf("expected success=true, got %#v", resp["success"])
	}

	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("failed to read auth dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 auth file, got %d", len(entries))
	}

	filePath := filepath.Join(authDir, entries[0].Name())
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read saved auth file: %v", err)
	}
	var saved map[string]any
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("failed to decode saved auth file: %v", err)
	}
	if got := saved["type"]; got != "gemini" {
		t.Fatalf("expected type gemini, got %#v", got)
	}
	if got := saved["project_id"]; got != "proj-1" {
		t.Fatalf("expected project_id proj-1, got %#v", got)
	}
	if got := saved["label"]; got != "Gemini Import" {
		t.Fatalf("expected label to be persisted, got %#v", got)
	}
	tokenMap, ok := saved["token"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested token map, got %#v", saved["token"])
	}
	if got := tokenMap["access_token"]; got != "access-1" {
		t.Fatalf("expected nested access_token access-1, got %#v", got)
	}

	secondBody := `{
		"mode":"geminicli",
		"credential":{
			"client_id":"plugin-client",
			"client_secret":"plugin-secret",
			"access_token":"access-2",
			"refresh_token":"refresh-2",
			"project_id":"proj-1"
		}
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(secondBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer expected")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode update response: %v", err)
	}
	if resp["action"] != "updated" {
		t.Fatalf("expected action updated, got %#v", resp["action"])
	}

	entries, err = os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("failed to read auth dir after update: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 auth file after update, got %d", len(entries))
	}

	data, err = os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read updated auth file: %v", err)
	}
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("failed to decode updated auth file: %v", err)
	}
	if got := saved["access_token"]; got != "access-2" {
		t.Fatalf("expected top-level access_token access-2, got %#v", got)
	}
	if got := saved["refresh_token"]; got != "refresh-2" {
		t.Fatalf("expected top-level refresh_token refresh-2, got %#v", got)
	}
	tokenMap, ok = saved["token"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested token map after update, got %#v", saved["token"])
	}
	if got := tokenMap["access_token"]; got != "access-2" {
		t.Fatalf("expected nested access_token access-2, got %#v", got)
	}
}

func TestCheckPluginTokensMatchesUpdaterShape(t *testing.T) {
	h, authDir := newPluginTestHandler(t)
	h.SetConfig(&config.Config{
		AuthDir:               authDir,
		PluginConnectionToken: "expected",
	})
	r := setupPluginRouter(h)

	freshBody := `{
		"token":"expected",
		"mode":"geminicli",
		"credential":{
			"client_id":"plugin-client",
			"refresh_token":"refresh-fresh",
			"project_id":"proj-fresh",
			"email":"fresh@example.com",
			"expiry":"2030-01-02T03:04:05Z"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(freshBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected update status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	staleBody := `{
		"token":"expected",
		"mode":"geminicli",
		"credential":{
			"client_id":"plugin-client",
			"refresh_token":"refresh-stale",
			"project_id":"proj-stale",
			"email":"stale@example.com",
			"expiry":"2000-01-02T03:04:05Z"
		}
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(staleBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected second update status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	checkBody := `{"token":"expected","mode":"geminicli"}`
	req = httptest.NewRequest(http.MethodPost, "/api/plugin/check-tokens", bytes.NewBufferString(checkBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected check status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp struct {
		Success            bool             `json:"success"`
		Tokens             []map[string]any `json:"tokens"`
		NeedsRefreshEmails []string         `json:"needs_refresh_emails"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode check response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success=true")
	}
	if len(resp.Tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(resp.Tokens))
	}
	if len(resp.NeedsRefreshEmails) != 1 || resp.NeedsRefreshEmails[0] != "stale@example.com" {
		t.Fatalf("unexpected needs_refresh_emails: %#v", resp.NeedsRefreshEmails)
	}
}

func TestUpdatePluginTokenCreatesAndUpdatesCodexCredential(t *testing.T) {
	h, authDir := newPluginTestHandler(t)
	h.SetConfig(&config.Config{
		AuthDir:               authDir,
		PluginConnectionToken: "expected",
	})
	r := setupPluginRouter(h)

	firstBody := `{
		"token":"expected",
		"mode":"codex",
		"name":"Codex Import",
		"credential":{
			"access_token":"access-1",
			"refresh_token":"refresh-1",
			"account_id":"acct-1",
			"email":"codex@example.com",
			"expired":"2030-01-02T03:04:05Z",
			"last_refresh":"2030-01-01T00:00:00Z"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(firstBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}
	if resp["provider"] != "codex" {
		t.Fatalf("expected provider codex, got %#v", resp["provider"])
	}
	if resp["action"] != "created" {
		t.Fatalf("expected action created, got %#v", resp["action"])
	}
	fileName, _ := resp["filename"].(string)
	if !strings.Contains(fileName, "codex@example.com") {
		t.Fatalf("expected codex filename to contain email, got %#v", fileName)
	}

	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("failed to read auth dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 auth file, got %d", len(entries))
	}

	filePath := filepath.Join(authDir, entries[0].Name())
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read saved auth file: %v", err)
	}
	var saved map[string]any
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("failed to decode saved auth file: %v", err)
	}
	if got := saved["type"]; got != "codex" {
		t.Fatalf("expected type codex, got %#v", got)
	}
	if got := saved["account_id"]; got != "acct-1" {
		t.Fatalf("expected account_id acct-1, got %#v", got)
	}
	if got := saved["email"]; got != "codex@example.com" {
		t.Fatalf("expected email codex@example.com, got %#v", got)
	}
	if got := saved["label"]; got != "Codex Import" {
		t.Fatalf("expected label Codex Import, got %#v", got)
	}
	if got := saved["access_token"]; got != "access-1" {
		t.Fatalf("expected access_token access-1, got %#v", got)
	}
	if got := saved["refresh_token"]; got != "refresh-1" {
		t.Fatalf("expected refresh_token refresh-1, got %#v", got)
	}
	if got := saved["last_refresh"]; got != "2030-01-01T00:00:00Z" {
		t.Fatalf("expected last_refresh to be preserved, got %#v", got)
	}

	secondBody := `{
		"mode":"codex",
		"credential":{
			"access_token":"access-2",
			"refresh_token":"refresh-2",
			"account_id":"acct-1",
			"email":"codex@example.com",
			"expired":"2030-02-02T03:04:05Z"
		}
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(secondBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer expected")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected update status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode update response: %v", err)
	}
	if resp["action"] != "updated" {
		t.Fatalf("expected action updated, got %#v", resp["action"])
	}

	entries, err = os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("failed to read auth dir after update: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 auth file after update, got %d", len(entries))
	}

	data, err = os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read updated auth file: %v", err)
	}
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("failed to decode updated auth file: %v", err)
	}
	if got := saved["access_token"]; got != "access-2" {
		t.Fatalf("expected access_token access-2, got %#v", got)
	}
	if got := saved["refresh_token"]; got != "refresh-2" {
		t.Fatalf("expected refresh_token refresh-2, got %#v", got)
	}
	if got := saved["expired"]; got != "2030-02-02T03:04:05Z" {
		t.Fatalf("expected expired to be refreshed, got %#v", got)
	}
}

func TestCheckPluginTokensSupportsCodexExpiry(t *testing.T) {
	h, authDir := newPluginTestHandler(t)
	h.SetConfig(&config.Config{
		AuthDir:               authDir,
		PluginConnectionToken: "expected",
	})
	r := setupPluginRouter(h)

	freshBody := `{
		"token":"expected",
		"mode":"codex",
		"credential":{
			"refresh_token":"refresh-fresh",
			"account_id":"acct-fresh",
			"email":"fresh-codex@example.com",
			"expired":"2030-01-02T03:04:05Z"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(freshBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected update status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	staleBody := `{
		"token":"expected",
		"mode":"codex",
		"credential":{
			"refresh_token":"refresh-stale",
			"account_id":"acct-stale",
			"email":"stale-codex@example.com",
			"expired":"2000-01-02T03:04:05Z"
		}
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/plugin/update-token", bytes.NewBufferString(staleBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected second update status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	checkBody := `{"token":"expected","mode":"codex"}`
	req = httptest.NewRequest(http.MethodPost, "/api/plugin/check-tokens", bytes.NewBufferString(checkBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected check status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp struct {
		Success            bool             `json:"success"`
		Tokens             []map[string]any `json:"tokens"`
		NeedsRefreshEmails []string         `json:"needs_refresh_emails"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode check response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success=true")
	}
	if len(resp.Tokens) != 2 {
		t.Fatalf("expected 2 codex tokens, got %d", len(resp.Tokens))
	}
	if len(resp.NeedsRefreshEmails) != 1 || resp.NeedsRefreshEmails[0] != "stale-codex@example.com" {
		t.Fatalf("unexpected needs_refresh_emails: %#v", resp.NeedsRefreshEmails)
	}
}

func TestPluginConnectionTokenManagementEndpoints(t *testing.T) {
	h, _ := newPluginTestHandler(t)
	r := setupPluginRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/v0/management/plugin-connection-token", bytes.NewBufferString(`{"value":"abc123"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/plugin-connection-token", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode get response: %v", err)
	}
	if resp["plugin-connection-token"] != "abc123" {
		t.Fatalf("expected stored token abc123, got %#v", resp["plugin-connection-token"])
	}

	req = httptest.NewRequest(http.MethodDelete, "/v0/management/plugin-connection-token", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}
