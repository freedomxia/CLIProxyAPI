package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractAccessToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata map[string]any
		expected string
	}{
		{
			"antigravity top-level access_token",
			map[string]any{"access_token": "tok-abc"},
			"tok-abc",
		},
		{
			"gemini nested token.access_token",
			map[string]any{
				"token": map[string]any{"access_token": "tok-nested"},
			},
			"tok-nested",
		},
		{
			"top-level takes precedence over nested",
			map[string]any{
				"access_token": "tok-top",
				"token":        map[string]any{"access_token": "tok-nested"},
			},
			"tok-top",
		},
		{
			"empty metadata",
			map[string]any{},
			"",
		},
		{
			"whitespace-only access_token",
			map[string]any{"access_token": "   "},
			"",
		},
		{
			"wrong type access_token",
			map[string]any{"access_token": 12345},
			"",
		},
		{
			"token is not a map",
			map[string]any{"token": "not-a-map"},
			"",
		},
		{
			"nested whitespace-only",
			map[string]any{
				"token": map[string]any{"access_token": "  "},
			},
			"",
		},
		{
			"fallback to nested when top-level empty",
			map[string]any{
				"access_token": "",
				"token":        map[string]any{"access_token": "tok-fallback"},
			},
			"tok-fallback",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractAccessToken(tt.metadata)
			if got != tt.expected {
				t.Errorf("extractAccessToken() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFileTokenStoreList_ProjectsNotionCredentialJSON(t *testing.T) {
	tempDir := t.TempDir()
	data, err := json.Marshal(map[string]any{
		"type":      "notion",
		"token_v2":  "token-secret",
		"space_id":  "space-1",
		"user_id":   "user-1",
		"base_url":  "https://www.notion.so",
		"prefix":    "team-notion",
		"proxy_url": "http://proxy.local",
		"headers": map[string]string{
			"x-test-header": "from-json",
		},
	})
	if err != nil {
		t.Fatalf("marshal auth json: %v", err)
	}
	if err = os.WriteFile(filepath.Join(tempDir, "notion-auth.json"), data, 0o600); err != nil {
		t.Fatalf("write auth json: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(tempDir)

	items, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list auth files: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(items))
	}

	auth := items[0]
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
		t.Fatalf("expected token_v2 to be projected, got %q", got)
	}
	if got := auth.Attributes["space_id"]; got != "space-1" {
		t.Fatalf("expected space_id to be projected, got %q", got)
	}
	if got := auth.Attributes["user_id"]; got != "user-1" {
		t.Fatalf("expected user_id to be projected, got %q", got)
	}
	if got := auth.Attributes["base_url"]; got != "https://www.notion.so" {
		t.Fatalf("expected base_url to be projected, got %q", got)
	}
	if got := auth.Attributes["header:x-test-header"]; got != "from-json" {
		t.Fatalf("expected custom header to be projected, got %q", got)
	}
	if got := auth.Attributes["auth_kind"]; got != "apikey" {
		t.Fatalf("expected auth_kind=apikey, got %q", got)
	}
}
