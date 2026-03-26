package config

import "strings"

// NotionKey represents a single Notion AI account backed by token_v2 cookie auth.
type NotionKey struct {
	// TokenV2 is the Notion session cookie value used for authentication.
	TokenV2 string `yaml:"token-v2" json:"token-v2"`

	// SpaceID identifies the Notion workspace to execute AI requests against.
	SpaceID string `yaml:"space-id" json:"space-id"`

	// UserID identifies the active Notion user within the workspace.
	UserID string `yaml:"user-id" json:"user-id"`

	// Priority controls selection preference when multiple credentials match.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces model aliases for this credential.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL optionally overrides the Notion origin for testing or reverse proxies.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL optionally overrides the global proxy for this credential.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this account.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []NotionModel `yaml:"models,omitempty" json:"models,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
}

func (k NotionKey) GetAPIKey() string  { return k.TokenV2 }
func (k NotionKey) GetBaseURL() string { return k.BaseURL }

// NotionModel describes a mapping between a client-visible alias and an upstream model identifier.
type NotionModel struct {
	Name  string `yaml:"name" json:"name"`
	Alias string `yaml:"alias" json:"alias"`
}

func (m NotionModel) GetName() string  { return m.Name }
func (m NotionModel) GetAlias() string { return m.Alias }

// SanitizeNotionKeys deduplicates and normalizes Notion account credentials.
func (cfg *Config) SanitizeNotionKeys() {
	if cfg == nil {
		return
	}

	seen := make(map[string]struct{}, len(cfg.NotionKey))
	out := cfg.NotionKey[:0]
	for i := range cfg.NotionKey {
		entry := cfg.NotionKey[i]
		entry.TokenV2 = strings.TrimSpace(entry.TokenV2)
		entry.SpaceID = strings.TrimSpace(entry.SpaceID)
		entry.UserID = strings.TrimSpace(entry.UserID)
		if entry.TokenV2 == "" || entry.SpaceID == "" || entry.UserID == "" {
			continue
		}

		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)

		sanitizedModels := make([]NotionModel, 0, len(entry.Models))
		seenModel := make(map[string]struct{}, len(entry.Models))
		for _, model := range entry.Models {
			name := strings.TrimSpace(model.Name)
			alias := strings.TrimSpace(model.Alias)
			if alias == "" {
				alias = name
			}
			if name == "" || alias == "" {
				continue
			}
			key := strings.ToLower(alias)
			if _, exists := seenModel[key]; exists {
				continue
			}
			seenModel[key] = struct{}{}
			sanitizedModels = append(sanitizedModels, NotionModel{
				Name:  name,
				Alias: alias,
			})
		}
		entry.Models = sanitizedModels

		uniqueKey := entry.TokenV2 + "|" + entry.SpaceID + "|" + entry.UserID + "|" + entry.BaseURL
		if _, exists := seen[uniqueKey]; exists {
			continue
		}
		seen[uniqueKey] = struct{}{}
		out = append(out, entry)
	}
	cfg.NotionKey = out
}
