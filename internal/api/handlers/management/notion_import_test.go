package management

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
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRequestNotionTokenImport_AutoDiscoverAndSave(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	notionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v3/loadUserContent" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if cookie := r.Header.Get("Cookie"); !strings.Contains(cookie, "token_v2=token-secret") {
			t.Fatalf("expected token_v2 cookie, got %q", cookie)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"recordMap": {
				"notion_user": {
					"user-1": {
						"value": {
							"given_name": "Alice",
							"email": "alice@example.com"
						}
					}
				},
				"space": {
					"space-1": {
						"value": {}
					}
				},
				"space_view": {
					"view-1": {
						"value": {}
					}
				}
			}
		}`))
	}))
	defer notionServer.Close()

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	body := []byte(`{"token_v2":"token-secret","base_url":"` + notionServer.URL + `","prefix":"team-notion"}`)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/notion-auth-import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.RequestNotionTokenImport(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Status      string `json:"status"`
		SavedPath   string `json:"saved_path"`
		UserEmail   string `json:"user_email"`
		UserName    string `json:"user_name"`
		UserID      string `json:"user_id"`
		SpaceID     string `json:"space_id"`
		SpaceViewID string `json:"space_view_id"`
		BaseURL     string `json:"base_url"`
		Prefix      string `json:"prefix"`
		Created     bool   `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.Status != "ok" {
		t.Fatalf("expected status ok, got %q", payload.Status)
	}
	if payload.UserEmail != "alice@example.com" {
		t.Fatalf("expected user email alice@example.com, got %q", payload.UserEmail)
	}
	if payload.UserName != "Alice" {
		t.Fatalf("expected user name Alice, got %q", payload.UserName)
	}
	if payload.UserID != "user-1" {
		t.Fatalf("expected user_id user-1, got %q", payload.UserID)
	}
	if payload.SpaceID != "space-1" {
		t.Fatalf("expected space_id space-1, got %q", payload.SpaceID)
	}
	if payload.SpaceViewID != "view-1" {
		t.Fatalf("expected space_view_id view-1, got %q", payload.SpaceViewID)
	}
	if payload.BaseURL != notionServer.URL {
		t.Fatalf("expected base_url %q, got %q", notionServer.URL, payload.BaseURL)
	}
	if payload.Prefix != "team-notion" {
		t.Fatalf("expected prefix team-notion, got %q", payload.Prefix)
	}
	if !payload.Created {
		t.Fatal("expected created=true for new auth file")
	}
	if payload.SavedPath == "" {
		t.Fatal("expected saved_path")
	}

	savedData, err := os.ReadFile(payload.SavedPath)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}

	var metadata map[string]any
	if err := json.Unmarshal(savedData, &metadata); err != nil {
		t.Fatalf("decode saved metadata: %v", err)
	}
	if got := anyString(metadata["type"]); got != "notion" {
		t.Fatalf("expected type notion, got %q", got)
	}
	if got := anyString(metadata["token_v2"]); got != "token-secret" {
		t.Fatalf("expected token_v2 to be saved, got %q", got)
	}
	if got := anyString(metadata["space_id"]); got != "space-1" {
		t.Fatalf("expected saved space_id, got %q", got)
	}
	if got := anyString(metadata["user_id"]); got != "user-1" {
		t.Fatalf("expected saved user_id, got %q", got)
	}
	if got := anyString(metadata["email"]); got != "alice@example.com" {
		t.Fatalf("expected saved email, got %q", got)
	}

	authID := filepath.Base(payload.SavedPath)
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("expected auth %q to be registered in manager", authID)
	}
	if got := auth.Attributes["token_v2"]; got != "token-secret" {
		t.Fatalf("expected runtime token_v2 attr, got %q", got)
	}
}

func TestRequestNotionTokenImport_AcceptsExtractedTextFallback(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	body := []byte(`{
		"prefix":"notion-fallback",
		"extracted_text":"NOTION_ACCOUNTS='[{\"token_v2\":\"token-fallback\",\"space_id\":\"space-fallback\",\"user_id\":\"user-fallback\",\"space_view_id\":\"view-fallback\",\"user_name\":\"Fallback User\",\"user_email\":\"fallback@example.com\"}]'"
	}`)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/notion-auth-import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.RequestNotionTokenImport(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		SavedPath string `json:"saved_path"`
		BaseURL   string `json:"base_url"`
		Created   bool   `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.BaseURL != defaultNotionBaseURL {
		t.Fatalf("expected default base url %q, got %q", defaultNotionBaseURL, payload.BaseURL)
	}
	if !payload.Created {
		t.Fatal("expected created=true for fallback import")
	}
	savedData, err := os.ReadFile(payload.SavedPath)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !bytes.Contains(savedData, []byte(`"user_email":"fallback@example.com"`)) {
		t.Fatalf("expected fallback user_email to be saved, got %s", string(savedData))
	}
}

func TestRequestNotionTokenImport_RequiresTokenOrExtractedPayload(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/notion-auth-import", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.RequestNotionTokenImport(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "token_v2 is required") {
		t.Fatalf("expected token required error, got %s", rec.Body.String())
	}
}
