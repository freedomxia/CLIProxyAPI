package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestNotionExecutorExecute_PreservesConversationAndSearchMetadata(t *testing.T) {
	const sourceConversationID = "thread-alpha"
	expectedThreadID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-notion:"+sourceConversationID)).String()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != notionRunTranscriptPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("x-notion-space-id"); got != "space-1" {
			t.Fatalf("unexpected x-notion-space-id: %q", got)
		}
		if got := r.Header.Get("x-notion-active-user-header"); got != "user-1" {
			t.Fatalf("unexpected x-notion-active-user-header: %q", got)
		}
		cookie, err := r.Cookie("token_v2")
		if err != nil {
			t.Fatalf("missing token_v2 cookie: %v", err)
		}
		if cookie.Value != "token-secret" {
			t.Fatalf("unexpected token_v2 cookie value: %q", cookie.Value)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if got := gjsonString(body, "threadId"); got != expectedThreadID {
			t.Fatalf("threadId = %q, want %q", got, expectedThreadID)
		}
		if got := gjsonString(body, "transcript.0.value.model"); got != notionUpstreamModelMap["gpt-5.4"] {
			t.Fatalf("transcript model = %q", got)
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"queries":["search me"],"sources":[{"title":"Doc","url":"https://example.com"}],"categories":["web"]}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"patch","v":[{"o":"a","v":{"type":"markdown-chat","value":["<lang primary=\"zh-CN\"/>\n\nHello from Notion"]}}]}` + "\n"))
	}))
	defer server.Close()

	exec := NewNotionExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "notion-auth",
		Provider: "notion",
		Attributes: map[string]string{
			"api_key":  "token-secret",
			"token_v2": "token-secret",
			"space_id": "space-1",
			"user_id":  "user-1",
			"base_url": server.URL,
		},
	}

	reqBody := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hello"}],
		"conversation_id":"` + sourceConversationID + `"
	}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: reqBody,
		Format:  sdktranslator.FromString("openai"),
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: reqBody,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(resp.Payload, &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got := nestedString(payload, "choices", 0, "message", "content"); got != "Hello from Notion" {
		t.Fatalf("content = %q", got)
	}
	if got := testStringValue(payload["conversation_id"]); got != expectedThreadID {
		t.Fatalf("conversation_id = %q, want %q", got, expectedThreadID)
	}
	if got := nestedString(payload, "search_metadata", "queries", 0); got != "search me" {
		t.Fatalf("search_metadata.queries[0] = %q", got)
	}
	if got := nestedInt(payload, "usage", "prompt_tokens"); got <= 0 {
		t.Fatalf("usage.prompt_tokens = %d, want > 0", got)
	}
	if got := nestedInt(payload, "usage", "completion_tokens"); got <= 0 {
		t.Fatalf("usage.completion_tokens = %d, want > 0", got)
	}
	if got := nestedInt(payload, "usage", "total_tokens"); got <= 0 {
		t.Fatalf("usage.total_tokens = %d, want > 0", got)
	}
}

func TestNotionExecutorExecuteStream_EmitsOpenAIChunks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"patch","v":[{"o":"a","v":{"type":"markdown-chat","value":["<lang primary=\"zh-CN\"/>\n\nstream hello"]}}]}` + "\n"))
	}))
	defer server.Close()

	exec := NewNotionExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "notion-auth",
		Provider: "notion",
		Attributes: map[string]string{
			"api_key":  "token-secret",
			"token_v2": "token-secret",
			"space_id": "space-1",
			"user_id":  "user-1",
			"base_url": server.URL,
		},
	}

	reqBody := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"conversation_id":"thread-alpha","stream_options":{"include_usage":true}}`)
	stream, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: reqBody,
		Format:  sdktranslator.FromString("openai"),
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: reqBody,
		Stream:          true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var chunks []string
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		chunks = append(chunks, string(chunk.Payload))
	}
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, `"content":"stream hello"`) {
		t.Fatalf("stream output missing content: %s", joined)
	}
	if strings.Contains(joined, `<lang primary=\"zh-CN\"/>`) {
		t.Fatalf("stream output still contained lang tag: %s", joined)
	}
	if !strings.Contains(joined, `"conversation_id":"`) {
		t.Fatalf("stream output missing conversation_id: %s", joined)
	}
	if !strings.Contains(joined, `"usage":{`) || !strings.Contains(joined, `"prompt_tokens":`) {
		t.Fatalf("stream output missing usage chunk: %s", joined)
	}
}

func TestNotionExecutorExecuteStream_SkipsUsageChunkByDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"patch","v":[{"o":"a","v":{"type":"markdown-chat","value":["<lang primary=\"zh-CN\"/>\n\nstream hello"]}}]}` + "\n"))
	}))
	defer server.Close()

	exec := NewNotionExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "notion-auth",
		Provider: "notion",
		Attributes: map[string]string{
			"api_key":  "token-secret",
			"token_v2": "token-secret",
			"space_id": "space-1",
			"user_id":  "user-1",
			"base_url": server.URL,
		},
	}

	reqBody := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"conversation_id":"thread-alpha"}`)
	stream, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: reqBody,
		Format:  sdktranslator.FromString("openai"),
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: reqBody,
		Stream:          true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var chunks []string
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		chunks = append(chunks, string(chunk.Payload))
	}
	joined := strings.Join(chunks, "\n")
	if strings.Contains(joined, `"usage":{`) {
		t.Fatalf("stream output unexpectedly contained usage chunk: %s", joined)
	}
}

func TestNotionExecutorExecute_MissingAuth(t *testing.T) {
	exec := NewNotionExecutor(&config.Config{})
	_, err := exec.Execute(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`),
		Format:  sdktranslator.FromString("openai"),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err == nil {
		t.Fatal("expected error for missing auth")
	}
}

func TestNotionCleanText_StripsLeadingLangTagsOnly(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "self closing lang tag",
			in:   `<lang primary="zh-CN"/>` + "\n\nOK",
			want: "OK",
		},
		{
			name: "paired empty lang tag",
			in:   `<lang primary="en-US"></lang>` + "\n\nHello",
			want: "Hello",
		},
		{
			name: "preserve normal markup in body",
			in:   `Hello <b>world</b>`,
			want: `Hello <b>world</b>`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := notionCleanText(tc.in); got != tc.want {
				t.Fatalf("notionCleanText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func gjsonString(body []byte, path string) string {
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	switch path {
	case "threadId":
		return testStringValue(payload["threadId"])
	case "transcript.0.value.model":
		transcript, _ := payload["transcript"].([]any)
		if len(transcript) == 0 {
			return ""
		}
		record, _ := transcript[0].(map[string]any)
		value, _ := record["value"].(map[string]any)
		return testStringValue(value["model"])
	default:
		return ""
	}
}

func nestedString(root any, path ...any) string {
	current := root
	for _, step := range path {
		switch typed := step.(type) {
		case string:
			record, _ := current.(map[string]any)
			current = record[typed]
		case int:
			items, _ := current.([]any)
			if typed < 0 || typed >= len(items) {
				return ""
			}
			current = items[typed]
		}
	}
	return testStringValue(current)
}

func testStringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func nestedInt(root any, path ...string) int64 {
	current := root
	for _, step := range path {
		record, _ := current.(map[string]any)
		current = record[step]
	}
	switch typed := current.(type) {
	case float64:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	default:
		return 0
	}
}
