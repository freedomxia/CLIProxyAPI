package auth

import (
	"strconv"
	"strings"
)

// MergeProviderMetadataAttributes projects provider-specific credential fields from
// auth JSON metadata into runtime Attributes so executors can consume them uniformly.
func MergeProviderMetadataAttributes(attrs map[string]string, provider string, metadata map[string]any) {
	if attrs == nil || metadata == nil {
		return
	}

	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "notion":
		mergeNotionMetadataAttributes(attrs, metadata)
	}
}

func mergeNotionMetadataAttributes(attrs map[string]string, metadata map[string]any) {
	if token := metadataString(metadata, "token_v2", "token-v2", "api_key"); token != "" {
		attrs["api_key"] = token
		attrs["token_v2"] = token
		attrs["auth_kind"] = "apikey"
	}
	if spaceID := metadataString(metadata, "space_id", "space-id"); spaceID != "" {
		attrs["space_id"] = spaceID
	}
	if userID := metadataString(metadata, "user_id", "user-id"); userID != "" {
		attrs["user_id"] = userID
	}
	if baseURL := metadataString(metadata, "base_url", "base-url"); baseURL != "" {
		attrs["base_url"] = baseURL
	}
	if priority := metadataIntString(metadata, "priority"); priority != "" {
		if _, exists := attrs["priority"]; !exists {
			attrs["priority"] = priority
		}
	}
	if rawHeaders, ok := metadata["headers"]; ok {
		mergeHeaderAttributes(attrs, rawHeaders)
	}
}

func metadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		raw, ok := metadata[key]
		if !ok {
			continue
		}
		if value, ok := raw.(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func metadataIntString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		raw, ok := metadata[key]
		if !ok {
			continue
		}
		switch value := raw.(type) {
		case float64:
			return strconv.Itoa(int(value))
		case string:
			trimmed := strings.TrimSpace(value)
			if trimmed == "" {
				continue
			}
			if _, err := strconv.Atoi(trimmed); err == nil {
				return trimmed
			}
		}
	}
	return ""
}

func mergeHeaderAttributes(attrs map[string]string, raw any) {
	switch headers := raw.(type) {
	case map[string]any:
		for key, value := range headers {
			headerKey := strings.TrimSpace(key)
			headerValue, _ := value.(string)
			headerValue = strings.TrimSpace(headerValue)
			if headerKey == "" || headerValue == "" {
				continue
			}
			attrs["header:"+headerKey] = headerValue
		}
	case map[string]string:
		for key, value := range headers {
			headerKey := strings.TrimSpace(key)
			headerValue := strings.TrimSpace(value)
			if headerKey == "" || headerValue == "" {
				continue
			}
			attrs["header:"+headerKey] = headerValue
		}
	}
}
