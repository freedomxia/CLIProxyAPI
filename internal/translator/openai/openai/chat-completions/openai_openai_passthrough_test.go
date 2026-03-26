package chat_completions

import (
	"context"
	"encoding/json"
	"testing"
)

func TestConvertOpenAIRequestToOpenAIPreservesCustomFields(t *testing.T) {
	input := []byte(`{
		"model":"notion/claude-sonnet4.6",
		"messages":[{"role":"user","content":"hi"}],
		"conversation_id":"conv-123",
		"metadata":{"workspace":"clip"},
		"stream":false
	}`)

	got := ConvertOpenAIRequestToOpenAI("claude-sonnet4.6", input, false)

	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal translated payload: %v", err)
	}

	if payload["model"] != "claude-sonnet4.6" {
		t.Fatalf("model = %v, want %q", payload["model"], "claude-sonnet4.6")
	}
	if payload["conversation_id"] != "conv-123" {
		t.Fatalf("conversation_id = %v, want %q", payload["conversation_id"], "conv-123")
	}

	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing or wrong type: %#v", payload["metadata"])
	}
	if metadata["workspace"] != "clip" {
		t.Fatalf("metadata.workspace = %v, want %q", metadata["workspace"], "clip")
	}
}

func TestConvertOpenAIResponseToOpenAIStreamPreservesCustomEvent(t *testing.T) {
	var param any
	raw := []byte(`data: {"type":"search_metadata","searches":{"queries":["notion ai"]}}`)

	got := ConvertOpenAIResponseToOpenAI(context.Background(), "", nil, nil, raw, &param)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0] != `{"type":"search_metadata","searches":{"queries":["notion ai"]}}` {
		t.Fatalf("stream chunk = %q", got[0])
	}
}

func TestConvertOpenAIResponseToOpenAINonStreamPreservesCustomFields(t *testing.T) {
	var param any
	raw := []byte(`{"id":"chatcmpl-1","search_metadata":{"type":"search_metadata","searches":{"queries":["notion ai"]}}}`)

	got := ConvertOpenAIResponseToOpenAINonStream(context.Background(), "", nil, nil, raw, &param)
	if got != string(raw) {
		t.Fatalf("non-stream payload = %q, want %q", got, string(raw))
	}
}
