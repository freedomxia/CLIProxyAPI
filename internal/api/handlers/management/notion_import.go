package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const defaultNotionBaseURL = "https://www.notion.so"

var notionImportHTTPClient = &http.Client{Timeout: 15 * time.Second}

type notionImportRequest struct {
	TokenV2       string `json:"token_v2"`
	BaseURL       string `json:"base_url"`
	Prefix        string `json:"prefix"`
	ExtractedText string `json:"extracted_text"`
}

type notionImportAccount struct {
	TokenV2     string
	BaseURL     string
	Prefix      string
	SpaceID     string
	UserID      string
	SpaceViewID string
	UserName    string
	UserEmail   string
}

func (h *Handler) RequestNotionTokenImport(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	if strings.TrimSpace(h.cfg.AuthDir) == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth directory not configured"})
		return
	}

	var payload notionImportRequest
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	ctx := context.Background()
	if reqCtx := c.Request.Context(); reqCtx != nil {
		ctx = reqCtx
	}

	account, err := resolveNotionImportAccount(ctx, payload)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	targetPath, created, err := h.resolveNotionAuthPath(account)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	fileName := filepath.Base(targetPath)
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "notion",
		FileName: fileName,
		Label:    account.label(),
		Metadata: account.metadata(),
		Attributes: map[string]string{
			"path":   targetPath,
			"source": targetPath,
		},
	}

	savedPath, err := h.saveTokenRecord(ctx, record)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save authentication tokens"})
		return
	}

	if err := h.registerAuthFromFile(ctx, savedPath, nil); err != nil {
		log.WithError(err).Warn("notion import saved auth file but runtime registration failed")
	}

	c.JSON(http.StatusOK, gin.H{
		"status":        "ok",
		"saved_path":    savedPath,
		"user_email":    account.UserEmail,
		"user_name":     account.UserName,
		"user_id":       account.UserID,
		"space_id":      account.SpaceID,
		"space_view_id": account.SpaceViewID,
		"base_url":      account.BaseURL,
		"prefix":        account.Prefix,
		"created":       created,
	})
}

func resolveNotionImportAccount(ctx context.Context, payload notionImportRequest) (*notionImportAccount, error) {
	extracted, err := parseNotionExtractedText(payload.ExtractedText)
	if err != nil {
		return nil, err
	}

	account := &notionImportAccount{}
	if extracted != nil {
		*account = *extracted
	}

	account.TokenV2 = firstNonEmptyText(strings.TrimSpace(payload.TokenV2), account.TokenV2)

	baseURL, err := normalizeNotionBaseURL(firstNonEmptyText(payload.BaseURL, account.BaseURL))
	if err != nil {
		return nil, err
	}
	account.BaseURL = baseURL

	prefix, err := normalizeNotionPrefix(firstNonEmptyText(payload.Prefix, account.Prefix))
	if err != nil {
		return nil, err
	}
	account.Prefix = prefix

	if account.TokenV2 == "" {
		return nil, fmt.Errorf("token_v2 is required")
	}

	if account.SpaceID == "" || account.UserID == "" {
		discovered, err := discoverNotionAccount(ctx, account.BaseURL, account.TokenV2)
		if err != nil {
			return nil, err
		}
		account.SpaceID = firstNonEmptyText(account.SpaceID, discovered.SpaceID)
		account.UserID = firstNonEmptyText(account.UserID, discovered.UserID)
		account.SpaceViewID = firstNonEmptyText(account.SpaceViewID, discovered.SpaceViewID)
		account.UserName = firstNonEmptyText(account.UserName, discovered.UserName)
		account.UserEmail = firstNonEmptyText(account.UserEmail, discovered.UserEmail)
	}

	if account.SpaceID == "" || account.UserID == "" {
		return nil, fmt.Errorf("failed to resolve notion account identifiers")
	}

	return account, nil
}

func parseNotionExtractedText(raw string) (*notionImportAccount, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, nil
	}

	if idx := strings.Index(text, "="); idx > 0 {
		key := strings.TrimSpace(text[:idx])
		if strings.EqualFold(key, "NOTION_ACCOUNTS") {
			text = strings.TrimSpace(text[idx+1:])
		}
	}
	if len(text) >= 2 {
		if (text[0] == '\'' && text[len(text)-1] == '\'') || (text[0] == '"' && text[len(text)-1] == '"') {
			text = text[1 : len(text)-1]
		}
	}

	var payload any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, fmt.Errorf("invalid extracted_text format")
	}

	switch value := payload.(type) {
	case []any:
		if len(value) == 0 {
			return nil, fmt.Errorf("extracted_text is empty")
		}
		item, ok := value[0].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid extracted_text format")
		}
		return notionAccountFromMap(item), nil
	case map[string]any:
		return notionAccountFromMap(value), nil
	default:
		return nil, fmt.Errorf("invalid extracted_text format")
	}
}

func notionAccountFromMap(data map[string]any) *notionImportAccount {
	if data == nil {
		return &notionImportAccount{}
	}
	return &notionImportAccount{
		TokenV2:     firstNonEmptyText(anyString(data["token_v2"]), anyString(data["token-v2"]), anyString(data["token"])),
		BaseURL:     firstNonEmptyText(anyString(data["base_url"]), anyString(data["base-url"])),
		Prefix:      anyString(data["prefix"]),
		SpaceID:     firstNonEmptyText(anyString(data["space_id"]), anyString(data["space-id"])),
		UserID:      firstNonEmptyText(anyString(data["user_id"]), anyString(data["user-id"])),
		SpaceViewID: firstNonEmptyText(anyString(data["space_view_id"]), anyString(data["space-view-id"])),
		UserName:    firstNonEmptyText(anyString(data["user_name"]), anyString(data["user-name"]), anyString(data["name"])),
		UserEmail:   firstNonEmptyText(anyString(data["user_email"]), anyString(data["user-email"]), anyString(data["email"])),
	}
}

func normalizeNotionBaseURL(raw string) (string, error) {
	baseURL := strings.TrimSpace(raw)
	if baseURL == "" {
		return defaultNotionBaseURL, nil
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base_url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid base_url")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("invalid base_url")
	}
	return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host), nil
}

func normalizeNotionPrefix(raw string) (string, error) {
	prefix := strings.TrimSpace(raw)
	if prefix == "" {
		return "", nil
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	if strings.Contains(prefix, "/") {
		return "", fmt.Errorf("prefix must not contain /")
	}
	return prefix, nil
}

func discoverNotionAccount(ctx context.Context, baseURL, tokenV2 string) (*notionImportAccount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v3/loadUserContent", strings.NewReader("{}"))
	if err != nil {
		return nil, fmt.Errorf("failed to build notion discovery request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "token_v2="+tokenV2)
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Referer", baseURL+"/ai")

	resp, err := notionImportHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to contact notion")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read notion response")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("notion rejected token_v2")
		}
		return nil, fmt.Errorf("notion discovery failed with status %d", resp.StatusCode)
	}

	result := gjson.ParseBytes(body)
	userID, userRecord := firstGJSONEntry(result.Get("recordMap.notion_user"))
	spaceID, _ := firstGJSONEntry(result.Get("recordMap.space"))
	spaceViewID, _ := firstGJSONEntry(result.Get("recordMap.space_view"))
	userValue := userRecord.Get("value")

	account := &notionImportAccount{
		BaseURL:     baseURL,
		SpaceID:     strings.TrimSpace(spaceID),
		UserID:      strings.TrimSpace(userID),
		SpaceViewID: strings.TrimSpace(spaceViewID),
		UserName:    firstNonEmptyText(strings.TrimSpace(userValue.Get("given_name").String()), strings.TrimSpace(userValue.Get("name").String())),
		UserEmail:   strings.TrimSpace(userValue.Get("email").String()),
	}
	if account.SpaceID == "" || account.UserID == "" {
		return nil, fmt.Errorf("failed to extract notion account from response")
	}
	return account, nil
}

func firstGJSONEntry(result gjson.Result) (string, gjson.Result) {
	var (
		key   string
		value gjson.Result
	)
	result.ForEach(func(k, v gjson.Result) bool {
		key = strings.TrimSpace(k.String())
		value = v
		return false
	})
	return key, value
}

func (h *Handler) resolveNotionAuthPath(account *notionImportAccount) (string, bool, error) {
	if h == nil || h.cfg == nil {
		return "", false, fmt.Errorf("config unavailable")
	}
	authDir := strings.TrimSpace(h.cfg.AuthDir)
	if authDir == "" {
		return "", false, fmt.Errorf("auth directory not configured")
	}
	if !filepath.IsAbs(authDir) {
		if abs, err := filepath.Abs(authDir); err == nil {
			authDir = abs
		}
	}
	if existing, err := notionAuthPathByIdentity(authDir, account); err != nil {
		return "", false, err
	} else if existing != "" {
		return existing, false, nil
	}
	fileName := notionCredentialFileName(account)
	return filepath.Join(authDir, fileName), true, nil
}

func notionAuthPathByIdentity(authDir string, account *notionImportAccount) (string, error) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read auth dir: %w", err)
	}

	userID := strings.TrimSpace(account.UserID)
	userEmail := strings.TrimSpace(account.UserEmail)
	spaceID := strings.TrimSpace(account.SpaceID)

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
		if !strings.EqualFold(anyString(metadata["type"]), "notion") {
			continue
		}
		switch {
		case userID != "" && anyString(metadata["user_id"]) == userID:
			return fullPath, nil
		case userEmail != "" && strings.EqualFold(anyString(metadata["email"]), userEmail):
			return fullPath, nil
		case spaceID != "" && anyString(metadata["space_id"]) == spaceID:
			return fullPath, nil
		}
	}
	return "", nil
}

func notionCredentialFileName(account *notionImportAccount) string {
	base := sanitizeNotionFilePart(firstNonEmptyText(account.UserEmail, account.UserID, account.SpaceID))
	if base == "" {
		base = fmt.Sprintf("%d", time.Now().UnixMilli())
	}
	return fmt.Sprintf("notion-%s.json", base)
}

func sanitizeNotionFilePart(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	var builder strings.Builder
	lastDash := false
	for _, ch := range strings.ToLower(trimmed) {
		valid := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
		if valid {
			builder.WriteRune(ch)
			lastDash = false
			continue
		}
		switch ch {
		case '.', '_':
			builder.WriteRune(ch)
			lastDash = false
		default:
			if !lastDash && builder.Len() > 0 {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}

	return strings.Trim(builder.String(), "-")
}

func (a *notionImportAccount) label() string {
	if a == nil {
		return "Notion"
	}
	return firstNonEmptyText(a.UserEmail, a.UserName, a.UserID, "Notion")
}

func (a *notionImportAccount) metadata() map[string]any {
	if a == nil {
		return map[string]any{"type": "notion"}
	}
	metadata := map[string]any{
		"type":     "notion",
		"label":    a.label(),
		"email":    strings.TrimSpace(a.UserEmail),
		"token_v2": strings.TrimSpace(a.TokenV2),
		"space_id": strings.TrimSpace(a.SpaceID),
		"user_id":  strings.TrimSpace(a.UserID),
		"base_url": strings.TrimSpace(a.BaseURL),
	}
	if a.SpaceViewID != "" {
		metadata["space_view_id"] = strings.TrimSpace(a.SpaceViewID)
	}
	if a.UserName != "" {
		metadata["user_name"] = strings.TrimSpace(a.UserName)
	}
	if a.UserEmail != "" {
		metadata["user_email"] = strings.TrimSpace(a.UserEmail)
	}
	if a.Prefix != "" {
		metadata["prefix"] = strings.TrimSpace(a.Prefix)
	}
	return metadata
}
