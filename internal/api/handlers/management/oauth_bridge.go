package management

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

const bridgeOAuthWaitTimeout = 45 * time.Second

var errBridgeOAuthTimeout = errors.New("timeout waiting for oauth completion")

type bridgeLoginRequest struct {
	Password string `json:"password"`
}

type bridgeAuthStartRequest struct {
	Mode      string `json:"mode"`
	ProjectID string `json:"project_id"`
}

type bridgeAuthCallbackURLRequest struct {
	Mode        string `json:"mode"`
	CallbackURL string `json:"callback_url"`
	ProjectID   string `json:"project_id"`
}

func (h *Handler) BridgeLogin(c *gin.Context) {
	var req bridgeLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	localClient := bridgeIsLocalClient(c.ClientIP())
	if !localClient && !h.bridgeAllowRemote() {
		c.JSON(http.StatusForbidden, gin.H{"error": "remote management disabled"})
		return
	}
	if !h.bridgeHasConfiguredSecret(localClient) {
		c.JSON(http.StatusForbidden, gin.H{"error": "remote management key not set"})
		return
	}

	password := strings.TrimSpace(req.Password)
	if password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password is required"})
		return
	}
	if !h.bridgeMatchesManagementKey(password, localClient) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":   password,
		"message": "登录成功",
	})
}

func (h *Handler) BridgeStartOAuth(c *gin.Context) {
	if !h.bridgeAuthorizeOrAbort(c) {
		return
	}

	var req bridgeAuthStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	mode, _, targetPath, err := normalizeBridgeOAuthMode(req.Mode)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if projectID := strings.TrimSpace(req.ProjectID); projectID != "" {
		values := url.Values{}
		values.Set("project_id", projectID)
		targetPath = targetPath + "?" + values.Encode()
	}

	statusCode, body, err := invokeJSONHandler(c, http.MethodGet, targetPath, nil, func(ctx *gin.Context) {
		switch mode {
		case "antigravity":
			h.RequestAntigravityToken(ctx)
		default:
			h.RequestGeminiCLIToken(ctx)
		}
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initialize oauth flow"})
		return
	}
	if statusCode != http.StatusOK {
		c.Data(statusCode, "application/json; charset=utf-8", body)
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid oauth response"})
		return
	}
	if authURL, _ := payload["url"].(string); authURL != "" {
		payload["auth_url"] = authURL
	}
	payload["mode"] = mode

	c.JSON(http.StatusOK, payload)
}

func (h *Handler) BridgeCallbackURL(c *gin.Context) {
	if !h.bridgeAuthorizeOrAbort(c) {
		return
	}

	var req bridgeAuthCallbackURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	_, provider, _, err := normalizeBridgeOAuthMode(req.Mode)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	callbackURL := strings.TrimSpace(req.CallbackURL)
	state, err := bridgeStateFromCallbackURL(callbackURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	callbackBody, err := json.Marshal(gin.H{
		"provider":     provider,
		"redirect_url": callbackURL,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode callback payload"})
		return
	}

	statusCode, body, err := invokeJSONHandler(c, http.MethodPost, "/v0/management/oauth-callback", callbackBody, h.PostOAuthCallback)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to submit oauth callback"})
		return
	}
	if statusCode != http.StatusOK {
		c.Data(statusCode, "application/json; charset=utf-8", body)
		return
	}

	if err := waitForBridgeOAuthCompletion(state, bridgeOAuthWaitTimeout); err != nil {
		if errors.Is(err, errBridgeOAuthTimeout) {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"message":  "认证成功，凭证已保存",
		"provider": provider,
		"state":    state,
	})
}

func normalizeBridgeOAuthMode(mode string) (normalizedMode string, provider string, targetPath string, err error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "geminicli", "gemini", "gcli":
		return "geminicli", "gemini", "/v0/management/gemini-cli-auth-url", nil
	case "antigravity", "ag", "anti-gravity":
		return "antigravity", "antigravity", "/v0/management/antigravity-auth-url", nil
	default:
		return "", "", "", fmt.Errorf("unsupported mode")
	}
}

func bridgeStateFromCallbackURL(raw string) (string, error) {
	callbackURL := strings.TrimSpace(raw)
	if callbackURL == "" {
		return "", fmt.Errorf("callback_url is required")
	}
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		return "", fmt.Errorf("invalid callback_url")
	}
	state := strings.TrimSpace(parsed.Query().Get("state"))
	if state == "" {
		return "", fmt.Errorf("state is required")
	}
	if err := ValidateOAuthState(state); err != nil {
		return "", fmt.Errorf("invalid state")
	}
	return state, nil
}

func invokeJSONHandler(origin *gin.Context, method, targetPath string, body []byte, handler func(*gin.Context)) (int, []byte, error) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	reqBody := bytes.NewReader(body)
	request := httptest.NewRequest(method, targetPath, reqBody)
	if origin != nil && origin.Request != nil {
		request = request.WithContext(origin.Request.Context())
		request.RemoteAddr = origin.Request.RemoteAddr
		request.Host = origin.Request.Host
		copyHeaders(request.Header, origin.Request.Header)
	}
	if len(body) > 0 && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	ctx.Request = request

	handler(ctx)
	return recorder.Code, recorder.Body.Bytes(), nil
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func waitForBridgeOAuthCompletion(state string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, status, ok := GetOAuthSession(state)
		if !ok {
			return nil
		}
		if status != "" {
			return fmt.Errorf("%s", status)
		}
		if time.Now().After(deadline) {
			return errBridgeOAuthTimeout
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func bridgeIsLocalClient(clientIP string) bool {
	return clientIP == "127.0.0.1" || clientIP == "::1"
}

func (h *Handler) bridgeAllowRemote() bool {
	allowRemote := false
	if h != nil && h.cfg != nil {
		allowRemote = h.cfg.RemoteManagement.AllowRemote
	}
	if h != nil && h.allowRemoteOverride {
		allowRemote = true
	}
	return allowRemote
}

func (h *Handler) bridgeHasConfiguredSecret(localClient bool) bool {
	if h == nil {
		return false
	}
	if localClient && strings.TrimSpace(h.localPassword) != "" {
		return true
	}
	if strings.TrimSpace(h.envSecret) != "" {
		return true
	}
	if h.cfg == nil {
		return false
	}
	return strings.TrimSpace(h.cfg.RemoteManagement.SecretKey) != ""
}

func (h *Handler) bridgeAuthorizeOrAbort(c *gin.Context) bool {
	localClient := bridgeIsLocalClient(c.ClientIP())
	if !localClient && !h.bridgeAllowRemote() {
		c.JSON(http.StatusForbidden, gin.H{"error": "remote management disabled"})
		return false
	}
	if !h.bridgeHasConfiguredSecret(localClient) {
		c.JSON(http.StatusForbidden, gin.H{"error": "remote management key not set"})
		return false
	}

	provided := bridgeProvidedSecret(c)
	if provided == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing management key"})
		return false
	}
	if !h.bridgeMatchesManagementKey(provided, localClient) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid management key"})
		return false
	}
	return true
}

func bridgeProvidedSecret(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if ah := strings.TrimSpace(c.GetHeader("Authorization")); ah != "" {
		parts := strings.SplitN(ah, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return strings.TrimSpace(parts[1])
		}
		return ah
	}
	return strings.TrimSpace(c.GetHeader("X-Management-Key"))
}

func (h *Handler) bridgeMatchesManagementKey(provided string, localClient bool) bool {
	if h == nil {
		return false
	}
	if provided == "" {
		return false
	}
	if localClient {
		if lp := strings.TrimSpace(h.localPassword); lp != "" {
			if subtle.ConstantTimeCompare([]byte(provided), []byte(lp)) == 1 {
				return true
			}
		}
	}
	if envSecret := strings.TrimSpace(h.envSecret); envSecret != "" {
		if subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
			return true
		}
	}
	if h.cfg == nil {
		return false
	}
	secretHash := strings.TrimSpace(h.cfg.RemoteManagement.SecretKey)
	if secretHash == "" {
		return false
	}
	if strings.HasPrefix(secretHash, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(secretHash)) == 1
}
