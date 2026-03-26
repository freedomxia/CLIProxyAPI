package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRegisterAuthFromFile_ProjectsNotionRuntimeAttributes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	filePath := filepath.Join(authDir, "notion-space.json")
	data := []byte(`{
		"type":"notion",
		"token_v2":"token-secret",
		"space_id":"space-1",
		"user_id":"user-1",
		"prefix":"team-notion",
		"proxy_url":"http://proxy.local"
	}`)
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	if err := h.registerAuthFromFile(context.Background(), filePath, data); err != nil {
		t.Fatalf("registerAuthFromFile failed: %v", err)
	}

	auth, ok := manager.GetByID("notion-space.json")
	if !ok || auth == nil {
		t.Fatal("expected notion auth to be registered")
	}
	if auth.Provider != "notion" {
		t.Fatalf("expected provider notion, got %s", auth.Provider)
	}
	if auth.Prefix != "team-notion" {
		t.Fatalf("expected prefix team-notion, got %q", auth.Prefix)
	}
	if auth.ProxyURL != "http://proxy.local" {
		t.Fatalf("expected proxy_url http://proxy.local, got %q", auth.ProxyURL)
	}
	if got := auth.Attributes["token_v2"]; got != "token-secret" {
		t.Fatalf("expected token_v2 attr, got %q", got)
	}
	if got := auth.Attributes["space_id"]; got != "space-1" {
		t.Fatalf("expected space_id attr, got %q", got)
	}
	if got := auth.Attributes["user_id"]; got != "user-1" {
		t.Fatalf("expected user_id attr, got %q", got)
	}
	if got := auth.Attributes["auth_kind"]; got != "apikey" {
		t.Fatalf("expected auth_kind=apikey, got %q", got)
	}
}

func TestListAuthFiles_RedactsAPIKeyAccount(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	filePath := filepath.Join(authDir, "notion-space.json")
	if err := os.WriteFile(filePath, []byte(`{"type":"notion"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:        "notion-space.json",
		FileName:  "notion-space.json",
		Provider:  "notion",
		Label:     "Notion",
		Status:    coreauth.StatusActive,
		Prefix:    "team-notion",
		ProxyURL:  "http://proxy.local",
		CreatedAt: time.Now(),
		Attributes: map[string]string{
			"path":      filePath,
			"source":    filePath,
			"api_key":   "token-secret-abcdef",
			"token_v2":  "token-secret-abcdef",
			"space_id":  "space-1",
			"user_id":   "user-1",
			"base_url":  "https://www.notion.so",
			"auth_kind": "apikey",
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(payload.Files))
	}

	entry := payload.Files[0]
	if got, _ := entry["account_type"].(string); got != "api_key" {
		t.Fatalf("expected account_type api_key, got %q", got)
	}
	account, _ := entry["account"].(string)
	if account == "" {
		t.Fatal("expected masked account value")
	}
	if account == "token-secret-abcdef" {
		t.Fatal("expected api key account to be redacted")
	}
	if got, _ := entry["space_id"].(string); got != "space-1" {
		t.Fatalf("expected space_id in list entry, got %q", got)
	}
	if got, _ := entry["user_id"].(string); got != "user-1" {
		t.Fatalf("expected user_id in list entry, got %q", got)
	}
}
