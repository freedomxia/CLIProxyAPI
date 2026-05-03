package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	executorhelps "github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	notionDefaultBaseURL       = "https://www.notion.so"
	notionRunTranscriptPath    = "/api/v3/runInferenceTranscript"
	notionClientVersion        = "23.13.20260228.0625"
	notionDefaultUserAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	notionSearchMetadataMaxURL = 12
)

var notionThreadTypeByModel = map[string]string{
	"claude-opus4.6":   "workflow",
	"claude-sonnet4.6": "workflow",
	"gemini-3.1pro":    "markdown-chat",
	"gpt-5.2":          "workflow",
	"gpt-5.4":          "workflow",
}

var notionUpstreamModelMap = map[string]string{
	"claude-opus4.6":   "avocado-froyo-medium",
	"claude-sonnet4.6": "almond-croissant-low",
	"gemini-3.1pro":    "galette-medium-thinking",
	"gpt-5.2":          "oatmeal-cookie",
	"gpt-5.4":          "oval-kumquat-medium",
}

var notionLeadingLangTagPattern = regexp.MustCompile(`(?is)^(?:\s*<lang\b[^>]*(?:/>|>\s*</lang>)\s*)+`)

type NotionExecutor struct {
	cfg *config.Config
}

func NewNotionExecutor(cfg *config.Config) *NotionExecutor {
	return &NotionExecutor{cfg: cfg}
}

func (e *NotionExecutor) Identifier() string { return "notion" }

func (e *NotionExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil || auth == nil {
		return nil
	}
	headers, cookies, err := notionAuthHeadersAndCookies(auth)
	if err != nil {
		return err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	return nil
}

func (e *NotionExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("notion executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := executorhelps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *NotionExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	baseModel := strings.TrimSpace(req.Model)
	if base := strings.TrimSpace(thinking.ParseSuffix(req.Model).ModelName); base != "" {
		baseModel = base
	}

	reporter := executorhelps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	requestBody, translatedReq, conversationID, threadType, err := e.buildRequestBody(auth, req, opts, false)
	if err != nil {
		return resp, err
	}

	url := strings.TrimSuffix(notionBaseURL(auth), "/") + notionRunTranscriptPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(requestBody))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/x-ndjson")
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return resp, err
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	executorhelps.RecordAPIRequest(ctx, e.cfg, executorhelps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      requestBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := executorhelps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		executorhelps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("notion executor: close response body error: %v", errClose)
		}
	}()
	executorhelps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		executorhelps.AppendAPIResponseChunk(ctx, e.cfg, b)
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}

	accumulator, rawBody, err := readNotionNDJSON(ctx, e.cfg, httpResp.Body)
	if err != nil {
		executorhelps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	executorhelps.AppendAPIResponseChunk(ctx, e.cfg, rawBody)

	content := accumulator.finalContent()
	if content == "" {
		return resp, statusErr{code: http.StatusBadGateway, msg: "notion upstream returned empty content"}
	}

	usageDetail, usageErr := notionUsageDetail(baseModel, translatedReq, content)
	if usageErr != nil {
		log.Warnf("notion executor: usage counting failed: %v", usageErr)
	}

	openAIResp, err := buildOpenAIChatCompletionResponse(req.Model, content, conversationID, accumulator.searchMetadata(), usageDetail)
	if err != nil {
		return resp, err
	}
	if usageErr == nil {
		reporter.Publish(ctx, usageDetail)
	} else {
		reporter.EnsurePublished(ctx)
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FromString("openai"), opts.SourceFormat, req.Model, opts.OriginalRequest, translatedReq, openAIResp, &param)
	resp = cliproxyexecutor.Response{
		Payload: []byte(out),
		Headers: notionJSONResponseHeaders(httpResp.Header.Clone(), threadType),
	}
	return resp, nil
}

func (e *NotionExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	baseModel := strings.TrimSpace(req.Model)
	if base := strings.TrimSpace(thinking.ParseSuffix(req.Model).ModelName); base != "" {
		baseModel = base
	}

	reporter := executorhelps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	requestBody, translatedReq, conversationID, threadType, err := e.buildRequestBody(auth, req, opts, true)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(notionBaseURL(auth), "/") + notionRunTranscriptPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(requestBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/x-ndjson")
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	executorhelps.RecordAPIRequest(ctx, e.cfg, executorhelps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      requestBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := executorhelps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		executorhelps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	executorhelps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		executorhelps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("notion executor: close response body error: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("notion executor: close response body error: %v", errClose)
			}
		}()

		accumulator, rawBody, readErr := readNotionNDJSON(ctx, e.cfg, httpResp.Body)
		if readErr != nil {
			executorhelps.RecordAPIResponseError(ctx, e.cfg, readErr)
			out <- cliproxyexecutor.StreamChunk{Err: readErr}
			return
		}
		executorhelps.AppendAPIResponseChunk(ctx, e.cfg, rawBody)

		content := accumulator.finalContent()
		if content == "" {
			out <- cliproxyexecutor.StreamChunk{Err: statusErr{code: http.StatusBadGateway, msg: "notion upstream returned empty content"}}
			return
		}

		usageDetail, usageErr := notionUsageDetail(baseModel, translatedReq, content)
		if usageErr != nil {
			log.Warnf("notion executor: usage counting failed: %v", usageErr)
			reporter.EnsurePublished(ctx)
		} else {
			reporter.Publish(ctx, usageDetail)
		}

		includeUsage := notionShouldIncludeStreamUsage(req.Payload, opts.OriginalRequest, translatedReq)
		var param any
		for _, line := range buildOpenAIStreamLines(req.Model, content, conversationID, accumulator.searchMetadata(), usageDetail, includeUsage) {
			chunks := sdktranslator.TranslateStream(ctx, sdktranslator.FromString("openai"), opts.SourceFormat, req.Model, opts.OriginalRequest, translatedReq, line, &param)
			for _, chunk := range chunks {
				if len(chunk) == 0 {
					continue
				}
				out <- cliproxyexecutor.StreamChunk{Payload: chunk}
			}
		}
		doneChunks := sdktranslator.TranslateStream(ctx, sdktranslator.FromString("openai"), opts.SourceFormat, req.Model, opts.OriginalRequest, translatedReq, []byte("[DONE]"), &param)
		for _, chunk := range doneChunks {
			if len(chunk) == 0 {
				continue
			}
			out <- cliproxyexecutor.StreamChunk{Payload: chunk}
		}
	}()

	return &cliproxyexecutor.StreamResult{
		Headers: notionStreamResponseHeaders(httpResp.Header.Clone(), threadType),
		Chunks:  out,
	}, nil
}

func (e *NotionExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := strings.TrimSpace(req.Model)
	if base := strings.TrimSpace(thinking.ParseSuffix(req.Model).ModelName); base != "" {
		baseModel = base
	}
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	enc, err := executorhelps.TokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("notion executor: tokenizer init failed: %w", err)
	}
	count, err := executorhelps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("notion executor: token counting failed: %w", err)
	}

	usageJSON := executorhelps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translatedUsage)}, nil
}

func (e *NotionExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *NotionExecutor) buildRequestBody(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, []byte, string, string, error) {
	if auth == nil {
		return nil, nil, "", "", statusErr{code: http.StatusUnauthorized, msg: "missing notion auth"}
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, req.Model, originalPayloadSource, stream)
	body := sdktranslator.TranslateRequest(from, to, req.Model, req.Payload, stream)

	var chatReq notionOpenAIChatRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		return nil, nil, "", "", fmt.Errorf("notion executor: invalid translated openai request: %w", err)
	}
	if len(chatReq.Messages) == 0 {
		return nil, nil, "", "", statusErr{code: http.StatusBadRequest, msg: "notion executor: empty messages"}
	}

	upstreamModel := strings.TrimSpace(req.Model)
	if base := strings.TrimSpace(thinking.ParseSuffix(req.Model).ModelName); base != "" {
		upstreamModel = base
	}
	if strings.TrimSpace(chatReq.Model) != "" {
		upstreamModel = strings.TrimSpace(chatReq.Model)
	}

	userID := strings.TrimSpace(auth.Attributes["user_id"])
	spaceID := strings.TrimSpace(auth.Attributes["space_id"])
	if userID == "" || spaceID == "" {
		return nil, nil, "", "", statusErr{code: http.StatusUnauthorized, msg: "missing notion user_id or space_id"}
	}

	transcript, threadType := buildNotionTranscript(chatReq.Messages, upstreamModel, userID, spaceID)
	conversationID := notionConversationID(originalTranslated)
	threadID, reusingThread := normalizeConversationID(conversationID)
	payload := map[string]any{
		"traceId":                       uuid.NewString(),
		"spaceId":                       spaceID,
		"threadId":                      threadID,
		"threadType":                    threadType,
		"createThread":                  !reusingThread,
		"generateTitle":                 true,
		"saveAllThreadOperations":       true,
		"setUnreadState":                true,
		"isPartialTranscript":           reusingThread,
		"asPatchResponse":               true,
		"isUserInAnySalesAssistedSpace": false,
		"isSpaceSalesAssisted":          false,
		"threadParentPointer": map[string]any{
			"table":   "space",
			"id":      spaceID,
			"spaceId": spaceID,
		},
		"transcript": transcript,
		"debugOverrides": map[string]any{
			"emitAgentSearchExtractedResults": true,
			"cachedInferences":                map[string]any{},
			"annotationInferences":            map[string]any{},
			"emitInferences":                  false,
		},
	}

	requestBody, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("notion executor: marshal request: %w", err)
	}
	return requestBody, body, threadID, threadType, nil
}

type notionOpenAIChatRequest struct {
	Model    string                `json:"model"`
	Messages []notionOpenAIMessage `json:"messages"`
}

type notionOpenAIMessage struct {
	Role    string `json:"role"`
	Name    string `json:"name,omitempty"`
	Content any    `json:"content"`
}

func notionBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if base := strings.TrimSpace(auth.Attributes["base_url"]); base != "" {
			return base
		}
	}
	return notionDefaultBaseURL
}

func notionAuthHeadersAndCookies(auth *cliproxyauth.Auth) (map[string]string, []*http.Cookie, error) {
	if auth == nil {
		return nil, nil, statusErr{code: http.StatusUnauthorized, msg: "missing notion auth"}
	}
	token := strings.TrimSpace(auth.Attributes["token_v2"])
	if token == "" {
		token = strings.TrimSpace(auth.Attributes["api_key"])
	}
	spaceID := strings.TrimSpace(auth.Attributes["space_id"])
	userID := strings.TrimSpace(auth.Attributes["user_id"])
	if token == "" || spaceID == "" || userID == "" {
		return nil, nil, statusErr{code: http.StatusUnauthorized, msg: "missing notion token_v2/space_id/user_id"}
	}
	headers := map[string]string{
		"User-Agent":                  notionDefaultUserAgent,
		"Origin":                      notionBaseURL(auth),
		"Referer":                     strings.TrimSuffix(notionBaseURL(auth), "/") + "/ai",
		"notion-audit-log-platform":   "web",
		"notion-client-version":       notionClientVersion,
		"x-notion-active-user-header": userID,
		"x-notion-space-id":           spaceID,
	}
	cookies := []*http.Cookie{
		{Name: "token_v2", Value: token},
		{Name: "notion_user_id", Value: userID},
	}
	return headers, cookies, nil
}

func notionConversationID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	for _, key := range []string{"conversation_id", "conversationId"} {
		if value := strings.TrimSpace(gjson.GetBytes(body, key).String()); value != "" {
			return value
		}
	}
	return ""
}

func normalizeConversationID(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return uuid.NewString(), false
	}
	if _, err := uuid.Parse(trimmed); err == nil {
		return trimmed, true
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-notion:"+trimmed)).String(), true
}

func buildNotionTranscript(messages []notionOpenAIMessage, modelName, userID, spaceID string) ([]map[string]any, string) {
	now := time.Now().UTC()
	threadType := notionThreadType(modelName)
	transcript := []map[string]any{
		{
			"id":   uuid.NewString(),
			"type": "config",
			"value": map[string]any{
				"type":          threadType,
				"model":         notionUpstreamModel(modelName),
				"modelFromUser": true,
				"useWebSearch":  true,
			},
		},
		{
			"id":   uuid.NewString(),
			"type": "context",
			"value": map[string]any{
				"timezone":        "Asia/Shanghai",
				"currentDatetime": now.Format(time.RFC3339),
				"userId":          userID,
				"spaceId":         spaceID,
			},
		},
	}

	systemTexts := make([]string, 0, len(messages))
	nonSystem := make([]notionOpenAIMessage, 0, len(messages))
	for _, message := range messages {
		text := flattenOpenAIMessageContent(message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(message.Role), "system") {
			systemTexts = append(systemTexts, text)
			continue
		}
		nonSystem = append(nonSystem, notionOpenAIMessage{
			Role:    message.Role,
			Name:    message.Name,
			Content: text,
		})
	}

	systemPrefix := ""
	if len(systemTexts) > 0 {
		systemPrefix = "[System Instructions: " + strings.Join(systemTexts, "\n") + "]\n\n"
	}
	systemApplied := false
	for _, message := range nonSystem {
		text, _ := message.Content.(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "assistant":
			transcript = append(transcript, map[string]any{
				"id":   uuid.NewString(),
				"type": "agent-inference",
				"value": []map[string]any{
					{"type": "text", "content": text},
				},
			})
		default:
			if !systemApplied && systemPrefix != "" {
				text = systemPrefix + text
				systemApplied = true
			}
			if role == "tool" {
				title := "[Tool Output]"
				if strings.TrimSpace(message.Name) != "" {
					title = "[Tool Output: " + strings.TrimSpace(message.Name) + "]"
				}
				text = title + "\n" + text
			}
			transcript = append(transcript, map[string]any{
				"id":        uuid.NewString(),
				"type":      "user",
				"userId":    userID,
				"createdAt": now.Format(time.RFC3339),
				"value":     [][]string{{text}},
			})
		}
	}

	if !systemApplied && systemPrefix != "" {
		transcript = append(transcript, map[string]any{
			"id":        uuid.NewString(),
			"type":      "user",
			"userId":    userID,
			"createdAt": now.Format(time.RFC3339),
			"value":     [][]string{{strings.TrimSpace(systemPrefix)}},
		})
	}

	return transcript, threadType
}

func notionThreadType(modelName string) string {
	base := strings.TrimSpace(modelName)
	if mapped, ok := notionThreadTypeByModel[base]; ok {
		return mapped
	}
	if strings.HasPrefix(base, "gemini-") {
		return "markdown-chat"
	}
	if strings.HasPrefix(notionUpstreamModel(base), "galette-") {
		return "markdown-chat"
	}
	return "workflow"
}

func notionUpstreamModel(modelName string) string {
	base := strings.TrimSpace(modelName)
	if mapped, ok := notionUpstreamModelMap[base]; ok {
		return mapped
	}
	return base
}

func flattenOpenAIMessageContent(content any) string {
	switch typed := content.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := flattenOpenAIMessageContent(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case map[string]any:
		itemType := strings.ToLower(strings.TrimSpace(anyString(typed["type"])))
		if itemType == "text" {
			if text := strings.TrimSpace(anyString(typed["text"])); text != "" {
				return text
			}
			if text := strings.TrimSpace(anyString(typed["content"])); text != "" {
				return text
			}
		}
		if text := strings.TrimSpace(anyString(typed["input_text"])); text != "" {
			return text
		}
		if text := strings.TrimSpace(anyString(typed["content"])); text != "" {
			return text
		}
		if value, ok := typed["text"]; ok {
			return flattenOpenAIMessageContent(value)
		}
		if value, ok := typed["content"]; ok {
			return flattenOpenAIMessageContent(value)
		}
		if value, ok := typed["value"]; ok {
			return flattenOpenAIMessageContent(value)
		}
	case []string:
		return strings.TrimSpace(strings.Join(typed, "\n"))
	}
	return ""
}

func anyString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

type notionSource struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

type notionSearchMetadata struct {
	Queries    []string       `json:"queries,omitempty"`
	Sources    []notionSource `json:"sources,omitempty"`
	Categories []string       `json:"categories,omitempty"`
}

type notionAccumulator struct {
	finalText string
	fallback  strings.Builder
	search    notionSearchMetadata
}

func readNotionNDJSON(ctx context.Context, cfg *config.Config, body io.Reader) (*notionAccumulator, []byte, error) {
	acc := &notionAccumulator{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, 52_428_800)

	var raw bytes.Buffer
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if len(line) == 0 {
			continue
		}
		raw.Write(line)
		raw.WriteByte('\n')
		acc.consumeLine(line)
	}
	if err := scanner.Err(); err != nil {
		return nil, raw.Bytes(), err
	}
	return acc, raw.Bytes(), nil
}

func (a *notionAccumulator) consumeLine(line []byte) {
	var payload map[string]any
	if err := json.Unmarshal(line, &payload); err != nil {
		return
	}
	collectNotionSearchMetadata(payload, &a.search)

	switch strings.ToLower(strings.TrimSpace(anyString(payload["type"]))) {
	case "record-map":
		if text := notionExtractFinalContentFromRecordMap(payload); text != "" {
			a.finalText = text
		}
	case "markdown-chat":
		if text := notionCleanText(notionExtractMarkdownChatText(payload["value"])); text != "" {
			a.finalText = text
		}
	case "patch":
		patches, _ := payload["v"].([]any)
		for _, patchRaw := range patches {
			patch, _ := patchRaw.(map[string]any)
			if patch == nil {
				continue
			}
			collectNotionSearchMetadata(patch, &a.search)
			if text := notionExtractMarkdownChatPatchText(patch); text != "" {
				a.finalText = text
				continue
			}
			if a.finalText != "" {
				continue
			}
			if text := notionCleanText(notionExtractTextFromPatch(patch)); text != "" {
				a.appendFallback(text)
			}
		}
	}
}

func (a *notionAccumulator) appendFallback(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	current := a.fallback.String()
	if current == text || strings.Contains(current, text) {
		return
	}
	a.fallback.WriteString(text)
}

func (a *notionAccumulator) finalContent() string {
	if a == nil {
		return ""
	}
	if trimmed := strings.TrimSpace(a.finalText); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(a.fallback.String())
}

func (a *notionAccumulator) searchMetadata() notionSearchMetadata {
	if a == nil {
		return notionSearchMetadata{}
	}
	return a.search
}

func notionExtractTextFromPatch(patch map[string]any) string {
	op := strings.TrimSpace(anyString(patch["o"]))
	switch op {
	case "a":
		if patchValue, ok := patch["v"].(map[string]any); ok {
			if value, ok := patchValue["value"]; ok {
				return notionExtractTextFromValueItems(value)
			}
		}
	case "x", "p":
		if value, ok := patch["v"].(string); ok {
			return value
		}
	}
	return ""
}

func notionExtractTextFromValueItems(value any) string {
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(anyString(record["type"]))) != "text" {
			continue
		}
		if content := strings.TrimSpace(anyString(record["content"])); content != "" {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "")
}

func notionExtractMarkdownChatPatchText(patch map[string]any) string {
	if strings.TrimSpace(anyString(patch["o"])) != "a" {
		return ""
	}
	patchValue, ok := patch["v"].(map[string]any)
	if !ok {
		return ""
	}
	if strings.ToLower(strings.TrimSpace(anyString(patchValue["type"]))) != "markdown-chat" {
		return ""
	}
	return notionCleanText(notionExtractMarkdownChatText(patchValue["value"]))
}

func notionExtractMarkdownChatText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			switch block := item.(type) {
			case string:
				parts = append(parts, block)
			case map[string]any:
				if strings.ToLower(strings.TrimSpace(anyString(block["type"]))) == "text" {
					if content := anyString(block["content"]); content != "" {
						parts = append(parts, content)
						continue
					}
				}
				if nested, ok := block["value"]; ok {
					if text := notionExtractMarkdownChatText(nested); text != "" {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "")
	case map[string]any:
		for _, key := range []string{"value", "content", "text"} {
			if nested, ok := typed[key]; ok {
				if text := notionExtractMarkdownChatText(nested); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func notionExtractFinalContentFromRecordMap(payload map[string]any) string {
	recordMap, ok := payload["recordMap"].(map[string]any)
	if !ok {
		return ""
	}
	threadMessages, ok := recordMap["thread_message"].(map[string]any)
	if !ok {
		return ""
	}

	best := ""
	bestPriority := -1
	for _, raw := range threadMessages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		outerValue, ok := message["value"].(map[string]any)
		if !ok {
			continue
		}
		innerValue, ok := outerValue["value"].(map[string]any)
		if !ok {
			continue
		}
		step, ok := innerValue["step"].(map[string]any)
		if !ok {
			continue
		}
		stepType := strings.ToLower(strings.TrimSpace(anyString(step["type"])))
		text := ""
		switch stepType {
		case "markdown-chat":
			text = notionExtractMarkdownChatText(step["value"])
		case "agent-inference":
			text = notionExtractTextFromValueItems(step["value"])
		case "text", "title":
			text = anyString(step["value"])
		}
		text = notionCleanText(text)
		if text == "" {
			continue
		}

		priority := 0
		switch stepType {
		case "text", "markdown-chat":
			priority = 3
		case "agent-inference":
			priority = 2
		case "title":
			priority = 1
		}
		if priority > bestPriority || (priority == bestPriority && len(text) > len(best)) {
			bestPriority = priority
			best = text
		}
	}
	return best
}

func notionCleanText(text string) string {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return ""
	}
	replacer := strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\ufeff", "")
	cleaned = strings.TrimSpace(replacer.Replace(cleaned))
	cleaned = notionLeadingLangTagPattern.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func collectNotionSearchMetadata(value any, out *notionSearchMetadata) {
	if out == nil || value == nil {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			switch lowerKey {
			case "queries", "questions":
				appendNotionQueries(out, nested)
			case "sources", "urls":
				appendNotionSources(out, nested)
			case "category", "categories":
				appendNotionCategories(out, nested)
			default:
				collectNotionSearchMetadata(nested, out)
			}
		}
	case []any:
		for _, item := range typed {
			collectNotionSearchMetadata(item, out)
		}
	}
}

func appendNotionQueries(out *notionSearchMetadata, value any) {
	items, ok := value.([]any)
	if !ok {
		if raw := strings.TrimSpace(anyString(value)); raw != "" {
			out.Queries = appendUniqueString(out.Queries, raw)
		}
		return
	}
	for _, item := range items {
		if query := strings.TrimSpace(anyString(item)); query != "" {
			out.Queries = appendUniqueString(out.Queries, query)
		}
	}
}

func appendNotionCategories(out *notionSearchMetadata, value any) {
	items, ok := value.([]any)
	if !ok {
		if raw := strings.TrimSpace(anyString(value)); raw != "" {
			out.Categories = appendUniqueString(out.Categories, raw)
		}
		return
	}
	for _, item := range items {
		if category := strings.TrimSpace(anyString(item)); category != "" {
			out.Categories = appendUniqueString(out.Categories, category)
		}
	}
}

func appendNotionSources(out *notionSearchMetadata, value any) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			appendNotionSources(out, item)
		}
	case map[string]any:
		source := notionSource{
			Title: strings.TrimSpace(anyString(typed["title"])),
			URL:   strings.TrimSpace(anyString(typed["url"])),
		}
		if source.URL == "" {
			source.URL = strings.TrimSpace(anyString(typed["href"]))
		}
		if source.Title == "" && source.URL == "" {
			return
		}
		out.Sources = appendUniqueSource(out.Sources, source)
	case string:
		raw := strings.TrimSpace(typed)
		if raw == "" {
			return
		}
		out.Sources = appendUniqueSource(out.Sources, notionSource{URL: raw})
	}
}

func appendUniqueString(items []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, existing := range items {
		if strings.EqualFold(existing, value) {
			return items
		}
	}
	return append(items, value)
}

func appendUniqueSource(items []notionSource, value notionSource) []notionSource {
	if value.Title == "" && value.URL == "" {
		return items
	}
	for _, existing := range items {
		if strings.EqualFold(existing.Title, value.Title) && strings.EqualFold(existing.URL, value.URL) {
			return items
		}
	}
	if len(items) >= notionSearchMetadataMaxURL {
		return items
	}
	return append(items, value)
}

func notionUsageDetail(model string, translatedReq []byte, content string) (usage.Detail, error) {
	enc, err := executorhelps.TokenizerForModel(model)
	if err != nil {
		return usage.Detail{}, fmt.Errorf("tokenizer init failed: %w", err)
	}

	promptTokens, err := executorhelps.CountOpenAIChatTokens(enc, translatedReq)
	if err != nil {
		return usage.Detail{}, fmt.Errorf("prompt token counting failed: %w", err)
	}

	completionTokens, err := executorhelps.CountPlainTextTokens(enc, content)
	if err != nil {
		return usage.Detail{}, fmt.Errorf("completion token counting failed: %w", err)
	}

	return usage.Detail{
		InputTokens:  promptTokens,
		OutputTokens: completionTokens,
		TotalTokens:  promptTokens + completionTokens,
	}, nil
}

func notionShouldIncludeStreamUsage(payload []byte, original []byte, translated []byte) bool {
	for _, candidate := range [][]byte{original, payload, translated} {
		if len(candidate) == 0 {
			continue
		}
		if gjson.GetBytes(candidate, "stream_options.include_usage").Bool() {
			return true
		}
	}
	return false
}

func buildOpenAIUsagePayload(detail usage.Detail) map[string]any {
	total := detail.TotalTokens
	if total == 0 {
		total = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}

	payload := map[string]any{
		"prompt_tokens":     detail.InputTokens,
		"completion_tokens": detail.OutputTokens,
		"total_tokens":      total,
	}
	if detail.CachedTokens > 0 {
		payload["prompt_tokens_details"] = map[string]any{
			"cached_tokens": detail.CachedTokens,
		}
	}
	if detail.ReasoningTokens > 0 {
		payload["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": detail.ReasoningTokens,
		}
	}
	return payload
}

func buildOpenAIChatCompletionResponse(model, content, conversationID string, search notionSearchMetadata, detail usage.Detail) ([]byte, error) {
	response := map[string]any{
		"id":      "chatcmpl_" + uuid.NewString(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": buildOpenAIUsagePayload(detail),
	}
	body, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	if conversationID != "" {
		if body, err = sjson.SetBytes(body, "conversation_id", conversationID); err != nil {
			return nil, err
		}
	}
	if len(search.Queries) > 0 || len(search.Sources) > 0 || len(search.Categories) > 0 {
		if body, err = sjson.SetBytes(body, "search_metadata", search); err != nil {
			return nil, err
		}
	}
	return body, nil
}

func buildOpenAIStreamLines(model, content, conversationID string, search notionSearchMetadata, detail usage.Detail, includeUsage bool) [][]byte {
	responseID := "chatcmpl_" + uuid.NewString()
	created := time.Now().Unix()
	lines := make([][]byte, 0, 5)

	lines = append(lines, mustJSONSSE(map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{"role": "assistant"},
			},
		},
	}))

	contentChunk := map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{"content": content},
			},
		},
	}
	if conversationID != "" {
		contentChunk["conversation_id"] = conversationID
	}
	if len(search.Queries) > 0 || len(search.Sources) > 0 || len(search.Categories) > 0 {
		contentChunk["search_metadata"] = search
	}
	lines = append(lines, mustJSONSSE(contentChunk))
	lines = append(lines, mustJSONSSE(map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	}))
	if includeUsage {
		lines = append(lines, mustJSONSSE(map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   buildOpenAIUsagePayload(detail),
		}))
	}
	return lines
}

func mustJSONSSE(payload map[string]any) []byte {
	body, _ := json.Marshal(payload)
	return []byte("data: " + string(body))
}

func notionJSONResponseHeaders(headers http.Header, threadType string) http.Header {
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "application/json")
	if strings.TrimSpace(threadType) != "" {
		headers.Set("X-Notion-Thread-Type", threadType)
	}
	return headers
}

func notionStreamResponseHeaders(headers http.Header, threadType string) http.Header {
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")
	if strings.TrimSpace(threadType) != "" {
		headers.Set("X-Notion-Thread-Type", threadType)
	}
	return headers
}
