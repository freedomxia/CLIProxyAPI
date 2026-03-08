package test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func newOAuthBridgeTestHandler(t *testing.T) (*management.Handler, string) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := &config.Config{
		AuthDir: filepath.Join(tmpDir, "auths"),
		RemoteManagement: config.RemoteManagement{
			SecretKey: "admin-secret",
		},
	}
	if err := os.MkdirAll(cfg.AuthDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth-dir: "+cfg.AuthDir+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	h := management.NewHandler(cfg, configPath, nil)
	h.SetLocalPassword("admin-secret")
	return h, cfg.AuthDir
}

func setupOAuthBridgeRouter(h *management.Handler) *gin.Engine {
	r := gin.New()
	auth := r.Group("/auth")
	auth.POST("/login", h.BridgeLogin)
	protected := auth.Group("")
	protected.Use(h.Middleware())
	protected.POST("/start", h.BridgeStartOAuth)
	protected.POST("/callback-url", h.BridgeCallbackURL)
	return r
}

func newLocalJSONRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	return req
}

func TestBridgeLoginReturnsTokenOnValidPassword(t *testing.T) {
	h, _ := newOAuthBridgeTestHandler(t)
	r := setupOAuthBridgeRouter(h)

	req := newLocalJSONRequest(http.MethodPost, "/auth/login", `{"password":"admin-secret"}`)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["token"] != "admin-secret" {
		t.Fatalf("expected returned token to match password, got %#v", resp["token"])
	}
}

func TestBridgeStartOAuthReturnsAuthURLForGeminiCLI(t *testing.T) {
	h, _ := newOAuthBridgeTestHandler(t)
	r := setupOAuthBridgeRouter(h)

	req := newLocalJSONRequest(http.MethodPost, "/auth/start", `{"mode":"geminicli"}`)
	req.Header.Set("Authorization", "Bearer admin-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	authURL, _ := resp["auth_url"].(string)
	if authURL == "" {
		t.Fatalf("expected auth_url to be populated, got %#v", resp["auth_url"])
	}
	state, _ := resp["state"].(string)
	if state == "" {
		t.Fatalf("expected state to be populated")
	}

	management.CompleteOAuthSession(state)
}

func TestBridgeStartOAuthRejectsInvalidManagementKey(t *testing.T) {
	h, _ := newOAuthBridgeTestHandler(t)
	r := setupOAuthBridgeRouter(h)

	req := newLocalJSONRequest(http.MethodPost, "/auth/start", `{"mode":"geminicli"}`)
	req.Header.Set("Authorization", "Bearer wrong-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d: %s", http.StatusUnauthorized, w.Code, w.Body.String())
	}
}

func TestBridgeCallbackURLCompletesPendingSession(t *testing.T) {
	h, authDir := newOAuthBridgeTestHandler(t)
	r := setupOAuthBridgeRouter(h)

	state := "bridge-state-123"
	management.RegisterOAuthSession(state, "gemini")
	callbackFile := filepath.Join(authDir, ".oauth-gemini-"+state+".oauth")

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(callbackFile); err == nil {
				management.CompleteOAuthSession(state)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	req := newLocalJSONRequest(
		http.MethodPost,
		"/auth/callback-url",
		`{"mode":"geminicli","callback_url":"http://localhost:1455/oauth2callback?code=test-code&state=`+state+`"}`,
	)
	req.Header.Set("Authorization", "Bearer admin-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	<-done

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %#v", resp["status"])
	}
}

func TestBridgeCallbackURLRejectsUnknownState(t *testing.T) {
	h, _ := newOAuthBridgeTestHandler(t)
	r := setupOAuthBridgeRouter(h)

	req := newLocalJSONRequest(
		http.MethodPost,
		"/auth/callback-url",
		`{"mode":"antigravity","callback_url":"http://localhost:11451/oauth-callback?code=test-code&state=missing-state"}`,
	)
	req.Header.Set("Authorization", "Bearer admin-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
}
