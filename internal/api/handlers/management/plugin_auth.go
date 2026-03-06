package management

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	codexAuth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	geminiAuth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/gemini"
)

const pluginConnectionTokenEnv = "PLUGIN_CONNECTION_TOKEN"

type pluginUpdateTokenRequest struct {
	Token      string         `json:"token"`
	Credential map[string]any `json:"credential"`
	Filename   string         `json:"filename"`
	Mode       string         `json:"mode"`
	Name       string         `json:"name"`
}

// GetPluginStatus reports whether the external plugin upload endpoint is configured.
func (h *Handler) GetPluginStatus(c *gin.Context) {
	token := h.pluginConnectionToken()
	c.JSON(http.StatusOK, gin.H{
		"enabled":   token != "",
		"has_token": token != "",
	})
}

// UpdatePluginToken accepts plugin-pushed credential JSON and stores it in auth-dir.
func (h *Handler) UpdatePluginToken(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	expectedToken := h.pluginConnectionToken()
	if expectedToken == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plugin connection token not configured"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}
	if len(body) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request body is required"})
		return
	}

	var req pluginUpdateTokenRequest
	if err = json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	providedToken := pluginProvidedToken(c.GetHeader("Authorization"), req.Token)
	if subtle.ConstantTimeCompare([]byte(providedToken), []byte(expectedToken)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid plugin connection token"})
		return
	}

	credential, err := pluginCredentialPayload(payload, req.Credential)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	provider := normalizePluginProvider(req.Mode, credential)
	metadata, err := pluginCredentialMetadata(provider, credential, req.Name)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	dst, created, err := h.resolvePluginAuthPath(metadata, req.Filename)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	data, err := json.Marshal(metadata)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode auth metadata"})
		return
	}

	if err = os.MkdirAll(h.cfg.AuthDir, 0o700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to create auth dir: %v", err)})
		return
	}
	if err = os.WriteFile(dst, data, 0o600); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to write auth file: %v", err)})
		return
	}
	if err = h.registerAuthFromFile(c.Request.Context(), dst, data); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	action := "updated"
	if created {
		action = "created"
	}
	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"message":  fmt.Sprintf("credential %s", action),
		"filename": filepath.Base(dst),
		"action":   action,
		"id":       filepath.Base(dst),
		"status":   "ok",
		"provider": provider,
		"file":     filepath.Base(dst),
		"created":  created,
	})
}

// CheckPluginTokens reports which stored credentials need refresh for external token updater integrations.
func (h *Handler) CheckPluginTokens(c *gin.Context) {
	expectedToken := h.pluginConnectionToken()
	if expectedToken == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plugin connection token not configured"})
		return
	}

	body, payload, err := pluginOptionalPayload(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	var req struct {
		Token string `json:"token"`
		Mode  string `json:"mode"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}

	providedToken := pluginProvidedToken(c.GetHeader("Authorization"), req.Token)
	if subtle.ConstantTimeCompare([]byte(providedToken), []byte(expectedToken)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid plugin connection token"})
		return
	}

	provider := normalizePluginProvider(req.Mode, payload)
	entries, err := h.pluginTokensForProvider(provider)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	needsRefreshEmails := make([]string, 0)
	for _, entry := range entries {
		if active, _ := entry["is_active"].(bool); !active {
			continue
		}
		needsRefresh, _ := entry["needs_refresh"].(bool)
		email, _ := entry["email"].(string)
		if needsRefresh && strings.TrimSpace(email) != "" {
			needsRefreshEmails = append(needsRefreshEmails, email)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success":              true,
		"tokens":               entries,
		"needs_refresh_emails": needsRefreshEmails,
	})
}

func (h *Handler) pluginConnectionToken() string {
	if envToken := strings.TrimSpace(os.Getenv(pluginConnectionTokenEnv)); envToken != "" {
		return envToken
	}
	if h == nil || h.cfg == nil {
		return ""
	}
	return strings.TrimSpace(h.cfg.PluginConnectionToken)
}

func pluginOptionalPayload(c *gin.Context) ([]byte, map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	if len(body) == 0 {
		return nil, map[string]any{}, nil
	}
	payload := make(map[string]any)
	if err = json.Unmarshal(body, &payload); err != nil {
		return body, nil, err
	}
	return body, payload, nil
}

func pluginProvidedToken(authorizationHeader, bodyToken string) string {
	if ah := strings.TrimSpace(authorizationHeader); ah != "" {
		parts := strings.SplitN(ah, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return strings.TrimSpace(parts[1])
		}
		return ah
	}
	return strings.TrimSpace(bodyToken)
}

func pluginCredentialPayload(payload map[string]any, nested map[string]any) (map[string]any, error) {
	if len(nested) > 0 {
		return cloneAnyMap(nested), nil
	}
	if payload == nil {
		return nil, fmt.Errorf("credential is required")
	}
	flattened := make(map[string]any, len(payload))
	for key, value := range payload {
		switch key {
		case "token", "filename", "mode", "name", "credential":
			continue
		default:
			flattened[key] = value
		}
	}
	if len(flattened) == 0 {
		return nil, fmt.Errorf("credential is required")
	}
	return flattened, nil
}

func normalizePluginProvider(mode string, credential map[string]any) string {
	for _, candidate := range []string{
		strings.TrimSpace(mode),
		anyString(credential["mode"]),
		anyString(credential["type"]),
	} {
		switch strings.ToLower(strings.TrimSpace(candidate)) {
		case "antigravity":
			return "antigravity"
		case "codex":
			return "codex"
		case "gemini", "geminicli":
			return "gemini"
		}
	}
	return "gemini"
}

func pluginCredentialMetadata(provider string, credential map[string]any, name string) (map[string]any, error) {
	if len(credential) == 0 {
		return nil, fmt.Errorf("credential is required")
	}
	if provider == "codex" {
		return pluginCodexCredentialMetadata(credential, name)
	}

	tokenData := cloneAnyMap(mapValue(credential, "token"))
	clientID := firstNonEmptyText(anyString(credential["client_id"]), anyString(tokenData["client_id"]))
	if clientID == "" {
		return nil, fmt.Errorf("client_id is required")
	}
	accessToken := firstNonEmptyText(
		anyString(credential["access_token"]),
		anyString(credential["token"]),
		anyString(tokenData["access_token"]),
	)
	refreshToken := firstNonEmptyText(
		anyString(credential["refresh_token"]),
		anyString(tokenData["refresh_token"]),
	)
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh_token is required")
	}

	projectID := firstNonEmptyText(anyString(credential["project_id"]), anyString(tokenData["project_id"]))
	email := firstNonEmptyText(anyString(credential["email"]), anyString(credential["account_email"]))
	label := strings.TrimSpace(name)

	metadata := map[string]any{
		"type":          provider,
		"refresh_token": refreshToken,
	}
	if accessToken != "" {
		metadata["access_token"] = accessToken
	}
	if projectID != "" {
		metadata["project_id"] = projectID
	}
	if email != "" {
		metadata["email"] = email
	}
	if label != "" {
		metadata["label"] = label
	}

	switch provider {
	case "antigravity":
		expiresAt := firstExpiry(credential, tokenData)
		if expiresAt.IsZero() {
			if rawExpired := firstNonEmptyText(anyString(credential["expired"]), anyString(tokenData["expired"])); rawExpired != "" {
				metadata["expired"] = rawExpired
			}
		} else {
			metadata["expired"] = expiresAt.UTC().Format(time.RFC3339)
		}
		if raw, ok := firstNumericValue(credential, tokenData, "expires_in"); ok {
			metadata["expires_in"] = raw
		} else if !expiresAt.IsZero() {
			seconds := int(time.Until(expiresAt).Seconds())
			if seconds < 0 {
				seconds = 0
			}
			metadata["expires_in"] = seconds
		}
	default:
		if tokenType := firstNonEmptyText(anyString(credential["token_type"]), anyString(tokenData["token_type"])); tokenType != "" {
			metadata["token_type"] = tokenType
		}
		if expiresAt := firstExpiry(credential, tokenData); !expiresAt.IsZero() {
			metadata["expiry"] = expiresAt.UTC().Format(time.RFC3339)
		}
		tokenMap := cloneAnyMap(tokenData)
		if tokenMap == nil {
			tokenMap = make(map[string]any)
		}
		if accessToken != "" {
			tokenMap["access_token"] = accessToken
		}
		tokenMap["refresh_token"] = refreshToken
		if _, ok := tokenMap["client_id"]; !ok {
			tokenMap["client_id"] = clientID
		}
		if _, ok := tokenMap["client_secret"]; !ok {
			tokenMap["client_secret"] = geminiAuth.ClientSecret
		}
		if _, ok := tokenMap["token_uri"]; !ok {
			tokenMap["token_uri"] = "https://oauth2.googleapis.com/token"
		}
		if expiry := anyString(credential["expiry"]); expiry != "" {
			tokenMap["expiry"] = expiry
		}
		if tokenType := anyString(credential["token_type"]); tokenType != "" {
			tokenMap["token_type"] = tokenType
		}
		if scopes, ok := credential["scopes"]; ok {
			tokenMap["scopes"] = scopes
		}
		metadata["token"] = tokenMap
	}

	return metadata, nil
}

func pluginCodexCredentialMetadata(credential map[string]any, name string) (map[string]any, error) {
	tokenData := cloneAnyMap(mapValue(credential, "token"))
	accessToken := firstNonEmptyText(
		anyString(credential["access_token"]),
		anyString(tokenData["access_token"]),
	)
	refreshToken := firstNonEmptyText(
		anyString(credential["refresh_token"]),
		anyString(tokenData["refresh_token"]),
	)
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh_token is required")
	}
	idToken := firstNonEmptyText(
		anyString(credential["id_token"]),
		anyString(tokenData["id_token"]),
	)
	accountID := firstNonEmptyText(
		anyString(credential["account_id"]),
		anyString(credential["chatgpt_account_id"]),
		anyString(tokenData["account_id"]),
		anyString(tokenData["chatgpt_account_id"]),
	)
	email := firstNonEmptyText(
		anyString(credential["email"]),
		anyString(credential["account_email"]),
		anyString(tokenData["email"]),
	)
	planType := firstNonEmptyText(
		anyString(credential["plan_type"]),
		anyString(credential["chatgpt_plan_type"]),
		anyString(tokenData["plan_type"]),
		anyString(tokenData["chatgpt_plan_type"]),
	)

	if idToken != "" {
		if claims, err := codexAuth.ParseJWTToken(idToken); err == nil && claims != nil {
			if accountID == "" {
				accountID = strings.TrimSpace(claims.GetAccountID())
			}
			if email == "" {
				email = strings.TrimSpace(claims.GetUserEmail())
			}
			if planType == "" {
				planType = strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
			}
		}
	}

	metadata := map[string]any{
		"type": "codex",
	}
	if idToken != "" {
		metadata["id_token"] = idToken
	}
	if accessToken != "" {
		metadata["access_token"] = accessToken
	}
	if refreshToken != "" {
		metadata["refresh_token"] = refreshToken
	}
	if accountID != "" {
		metadata["account_id"] = accountID
	}
	if email != "" {
		metadata["email"] = email
	}
	if label := strings.TrimSpace(name); label != "" {
		metadata["label"] = label
	}
	if planType != "" {
		metadata["plan_type"] = planType
	}
	if lastRefresh := firstNonEmptyText(
		anyString(credential["last_refresh"]),
		anyString(tokenData["last_refresh"]),
	); lastRefresh != "" {
		metadata["last_refresh"] = lastRefresh
	}
	if expiresAt := firstExpiry(credential, tokenData); !expiresAt.IsZero() {
		metadata["expired"] = expiresAt.UTC().Format(time.RFC3339)
	} else if rawExpired := firstNonEmptyText(
		anyString(credential["expired"]),
		anyString(tokenData["expired"]),
	); rawExpired != "" {
		metadata["expired"] = rawExpired
	}

	return metadata, nil
}

func (h *Handler) pluginTokensForProvider(provider string) ([]gin.H, error) {
	authDir := strings.TrimSpace(h.cfg.AuthDir)
	if authDir == "" {
		return nil, fmt.Errorf("auth dir is not configured")
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []gin.H{}, nil
		}
		return nil, fmt.Errorf("failed to read auth dir: %w", err)
	}

	results := make([]gin.H, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		fullPath := filepath.Join(authDir, entry.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		var metadata map[string]any
		if err = json.Unmarshal(data, &metadata); err != nil {
			continue
		}
		if normalizePluginProvider("", metadata) != provider {
			continue
		}
		item := gin.H{
			"filename":      entry.Name(),
			"email":         strings.TrimSpace(anyString(metadata["email"])),
			"is_active":     !boolValue(metadata["disabled"]),
			"needs_refresh": pluginNeedsRefresh(provider, metadata),
			"error_codes":   stringSlice(metadata["error_codes"]),
		}
		results = append(results, item)
	}
	return results, nil
}

func (h *Handler) resolvePluginAuthPath(metadata map[string]any, requestedName string) (string, bool, error) {
	authDir := strings.TrimSpace(h.cfg.AuthDir)
	if authDir == "" {
		return "", false, fmt.Errorf("auth dir is not configured")
	}
	if !filepath.IsAbs(authDir) {
		if abs, err := filepath.Abs(authDir); err == nil {
			authDir = abs
		}
	}

	provider := strings.TrimSpace(anyString(metadata["type"]))
	projectID := strings.TrimSpace(anyString(metadata["project_id"]))
	accountID := strings.TrimSpace(anyString(metadata["account_id"]))
	email := strings.TrimSpace(anyString(metadata["email"]))
	if provider != "" {
		if existing, err := pluginAuthPathByIdentity(authDir, provider, projectID, accountID, email); err != nil {
			return "", false, err
		} else if existing != "" {
			return existing, false, nil
		}
	}

	fileName := pluginFileName(metadata, requestedName)
	return filepath.Join(authDir, fileName), true, nil
}

func pluginAuthPathByIdentity(authDir, provider, projectID, accountID, email string) (string, error) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read auth dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		fullPath := filepath.Join(authDir, entry.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		var current map[string]any
		if err = json.Unmarshal(data, &current); err != nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(anyString(current["type"])), provider) {
			continue
		}
		switch {
		case projectID != "":
			if strings.TrimSpace(anyString(current["project_id"])) != projectID {
				continue
			}
		case provider == "codex" && accountID != "":
			if strings.TrimSpace(anyString(current["account_id"])) != accountID {
				continue
			}
		case provider == "codex" && email != "":
			if !strings.EqualFold(strings.TrimSpace(anyString(current["email"])), email) {
				continue
			}
		default:
			continue
		}
		return fullPath, nil
	}
	return "", nil
}

func pluginFileName(metadata map[string]any, requestedName string) string {
	if name := sanitizePluginFileName(requestedName); name != "" {
		return name
	}
	provider := strings.TrimSpace(anyString(metadata["type"]))
	projectID := strings.TrimSpace(anyString(metadata["project_id"]))
	email := strings.TrimSpace(anyString(metadata["email"]))
	accountID := strings.TrimSpace(anyString(metadata["account_id"]))

	if provider == "codex" && email != "" {
		planType := strings.TrimSpace(anyString(metadata["plan_type"]))
		hashAccountID := ""
		if accountID != "" {
			digest := sha256.Sum256([]byte(accountID))
			hashAccountID = hex.EncodeToString(digest[:])[:8]
		}
		return sanitizePluginFileName(codexAuth.CredentialFileName(email, planType, hashAccountID, true))
	}
	base := provider
	if base == "" {
		base = "plugin"
	}
	if projectID != "" {
		base = base + "-" + projectID
	} else {
		base = base + "-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	return sanitizePluginFileName(base)
}

func sanitizePluginFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	base := filepath.Base(name)
	ext := filepath.Ext(base)
	if strings.EqualFold(ext, ".json") {
		base = strings.TrimSuffix(base, ext)
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.', r == '@':
			return r
		default:
			return '-'
		}
	}, base)
	base = strings.Trim(base, "-_.")
	if base == "" {
		base = "plugin"
	}
	return base + ".json"
}

func mapValue(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	value, ok := m[key]
	if !ok {
		return nil
	}
	return cloneAnyMap(value)
}

func cloneAnyMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	source, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, item := range source {
		cloned[key] = item
	}
	return cloned
}

func anyString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstExpiry(primary, secondary map[string]any) time.Time {
	for _, key := range []string{"expiry", "expired"} {
		for _, current := range []map[string]any{primary, secondary} {
			if current == nil {
				continue
			}
			if parsed, ok := parseFlexibleTime(current[key]); ok {
				return parsed
			}
		}
	}
	return time.Time{}
}

func parseFlexibleTime(value any) (time.Time, bool) {
	switch v := value.(type) {
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, raw); err == nil {
				return ts, true
			}
		}
	case float64:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(v), 0), true
	case int64:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(v, 0), true
	case int:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(v), 0), true
	case json.Number:
		if i, err := v.Int64(); err == nil && i > 0 {
			return time.Unix(i, 0), true
		}
	}
	return time.Time{}, false
}

func firstNumericValue(primary, secondary map[string]any, key string) (int, bool) {
	for _, current := range []map[string]any{primary, secondary} {
		if current == nil {
			continue
		}
		switch v := current[key].(type) {
		case float64:
			return int(v), true
		case int:
			return v, true
		case int64:
			return int(v), true
		case json.Number:
			if i, err := v.Int64(); err == nil {
				return int(i), true
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}

func boolValue(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func stringSlice(value any) []string {
	raw, ok := value.([]any)
	if !ok || len(raw) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s := anyString(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func pluginNeedsRefresh(provider string, metadata map[string]any) bool {
	refreshAt := time.Time{}
	switch provider {
	case "antigravity":
		if ts, ok := parseFlexibleTime(metadata["expired"]); ok {
			refreshAt = ts
		}
	case "codex":
		if ts, ok := parseFlexibleTime(metadata["expired"]); ok {
			refreshAt = ts
		}
	default:
		if ts, ok := parseFlexibleTime(metadata["expiry"]); ok {
			refreshAt = ts
		}
	}
	if refreshAt.IsZero() {
		return true
	}
	return time.Until(refreshAt) < 5*time.Minute
}
