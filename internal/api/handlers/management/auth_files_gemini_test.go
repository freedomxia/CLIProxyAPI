package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	geminiAuth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/gemini"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(status int, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func TestEnsureGeminiProjectAndOnboard_FallsBackToProjectListAfterAutoDiscovery(t *testing.T) {
	t.Helper()

	var sawProjectList bool

	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.URL.Host == "cloudcode-pa.googleapis.com" && strings.HasSuffix(req.URL.Path, ":loadCodeAssist"):
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatalf("decode loadCodeAssist body: %v", err)
				}
				projectID, _ := body["cloudaicompanionProject"].(string)
				return jsonResponse(http.StatusOK, map[string]any{
					"allowedTiers": []map[string]any{{"id": "FREE", "isDefault": true}},
					"echoProject":  projectID,
				})
			case req.URL.Host == "cloudcode-pa.googleapis.com" && strings.HasSuffix(req.URL.Path, ":onboardUser"):
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatalf("decode onboardUser body: %v", err)
				}
				projectID, _ := body["cloudaicompanionProject"].(string)
				if projectID == "" {
					return jsonResponse(http.StatusOK, map[string]any{
						"done":     true,
						"response": map[string]any{},
					})
				}
				return jsonResponse(http.StatusOK, map[string]any{
					"done": true,
					"response": map[string]any{
						"cloudaicompanionProject": projectID,
					},
				})
			case req.URL.Host == "cloudresourcemanager.googleapis.com" && req.URL.Path == "/v1/projects":
				sawProjectList = true
				return jsonResponse(http.StatusOK, map[string]any{
					"projects": []map[string]any{
						{"projectId": "fallback-proj", "displayName": "Fallback Project"},
					},
				})
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
				return nil, nil
			}
		}),
	}

	storage := &geminiAuth.GeminiTokenStorage{}
	if err := ensureGeminiProjectAndOnboard(context.Background(), client, storage, ""); err != nil {
		t.Fatalf("ensureGeminiProjectAndOnboard returned error: %v", err)
	}
	if !sawProjectList {
		t.Fatal("expected fallback project list request")
	}
	if got := strings.TrimSpace(storage.ProjectID); got != "fallback-proj" {
		t.Fatalf("expected fallback project ID, got %q", got)
	}
	if !storage.Auto {
		t.Fatal("expected auto flag to remain true for fallback-selected project")
	}
}

func TestEnsureGeminiProjectAndOnboard_ReturnsSelectionRequiredWhenNoProjectAvailable(t *testing.T) {
	t.Helper()

	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.URL.Host == "cloudcode-pa.googleapis.com" && strings.HasSuffix(req.URL.Path, ":loadCodeAssist"):
				return jsonResponse(http.StatusOK, map[string]any{
					"allowedTiers": []map[string]any{{"id": "FREE", "isDefault": true}},
				})
			case req.URL.Host == "cloudcode-pa.googleapis.com" && strings.HasSuffix(req.URL.Path, ":onboardUser"):
				return jsonResponse(http.StatusOK, map[string]any{
					"done":     true,
					"response": map[string]any{},
				})
			case req.URL.Host == "cloudresourcemanager.googleapis.com" && req.URL.Path == "/v1/projects":
				return jsonResponse(http.StatusOK, map[string]any{
					"projects": []map[string]any{},
				})
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
				return nil, nil
			}
		}),
	}

	storage := &geminiAuth.GeminiTokenStorage{}
	err := ensureGeminiProjectAndOnboard(context.Background(), client, storage, "")
	if err == nil {
		t.Fatal("expected project selection error, got nil")
	}
	var selectionErr *projectSelectionRequiredError
	if !errors.As(err, &selectionErr) {
		t.Fatalf("expected projectSelectionRequiredError, got %T: %v", err, err)
	}
	if got := selectionErr.Error(); got != "No Google Cloud projects available for this account; please specify project_id manually" {
		t.Fatalf("unexpected selection error message: %q", got)
	}
}

func TestEnsureGeminiProjectAndOnboard_ExplicitProjectSkipsProjectList(t *testing.T) {
	t.Helper()

	var sawProjectList bool

	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.URL.Host == "cloudcode-pa.googleapis.com" && strings.HasSuffix(req.URL.Path, ":loadCodeAssist"):
				return jsonResponse(http.StatusOK, map[string]any{
					"allowedTiers": []map[string]any{{"id": "STANDARD", "isDefault": true}},
				})
			case req.URL.Host == "cloudcode-pa.googleapis.com" && strings.HasSuffix(req.URL.Path, ":onboardUser"):
				return jsonResponse(http.StatusOK, map[string]any{
					"done": true,
					"response": map[string]any{
						"cloudaicompanionProject": "explicit-proj",
					},
				})
			case req.URL.Host == "cloudresourcemanager.googleapis.com":
				sawProjectList = true
				return jsonResponse(http.StatusOK, map[string]any{
					"projects": []map[string]any{},
				})
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
				return nil, nil
			}
		}),
	}

	storage := &geminiAuth.GeminiTokenStorage{}
	if err := ensureGeminiProjectAndOnboard(context.Background(), client, storage, "explicit-proj"); err != nil {
		t.Fatalf("ensureGeminiProjectAndOnboard returned error: %v", err)
	}
	if sawProjectList {
		t.Fatal("did not expect project list request for explicit project")
	}
	if got := strings.TrimSpace(storage.ProjectID); got != "explicit-proj" {
		t.Fatalf("expected explicit project ID, got %q", got)
	}
	if storage.Auto {
		t.Fatal("expected auto flag to be false for explicit project")
	}
}

func TestFormatGeminiOnboardingStatusError(t *testing.T) {
	t.Helper()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "project selection required",
			err:  newProjectSelectionRequiredError("No Google Cloud projects available for this account; please specify project_id manually"),
			want: "No Google Cloud projects available for this account; please specify project_id manually",
		},
		{
			name: "load code assist failure",
			err:  errors.New("load code assist: unexpected status 403"),
			want: "loadCodeAssist failed: unexpected status 403",
		},
		{
			name: "fetch project list failure",
			err:  errors.New("fetch project list: Get \"https://cloudresourcemanager.googleapis.com/v1/projects\": context deadline exceeded"),
			want: "Failed to fetch Google Cloud project list: Get \"https://cloudresourcemanager.googleapis.com/v1/projects\": context deadline exceeded",
		},
		{
			name: "generic onboarding failure",
			err:  errors.New("onboard project foo: onboard user: unexpected status 500"),
			want: "Failed to complete Gemini CLI onboarding: onboard project foo: onboard user: unexpected status 500",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatGeminiOnboardingStatusError(tc.err); got != tc.want {
				t.Fatalf("formatGeminiOnboardingStatusError() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatGeminiCloudAPIStatusError(t *testing.T) {
	t.Helper()

	if got := formatGeminiCloudAPIStatusError(errors.New("project foo: Cloud AI API not enabled")); got != "Failed to verify Cloud AI API status: project foo: Cloud AI API not enabled" {
		t.Fatalf("unexpected cloud API status error: %q", got)
	}
	if got := formatGeminiCloudAPINotEnabled("foo-project"); got != "Cloud AI API not enabled for project foo-project" {
		t.Fatalf("unexpected cloud API disabled message: %q", got)
	}
}
