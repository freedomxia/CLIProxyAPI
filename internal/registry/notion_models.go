package registry

import "time"

var notionModels = []*ModelInfo{
	{
		ID:          "claude-opus4.6",
		Object:      "model",
		Created:     0,
		OwnedBy:     "notion",
		Type:        "notion",
		DisplayName: "Claude Opus 4.6",
		Description: "Notion AI native Claude Opus 4.6 route.",
	},
	{
		ID:          "claude-sonnet4.6",
		Object:      "model",
		Created:     0,
		OwnedBy:     "notion",
		Type:        "notion",
		DisplayName: "Claude Sonnet 4.6",
		Description: "Notion AI native Claude Sonnet 4.6 route.",
	},
	{
		ID:          "gemini-3.1pro",
		Object:      "model",
		Created:     0,
		OwnedBy:     "notion",
		Type:        "notion",
		DisplayName: "Gemini 3.1 Pro",
		Description: "Notion AI native Gemini 3.1 Pro route.",
	},
	{
		ID:          "gpt-5.2",
		Object:      "model",
		Created:     0,
		OwnedBy:     "notion",
		Type:        "notion",
		DisplayName: "GPT-5.2",
		Description: "Notion AI native GPT-5.2 route.",
	},
	{
		ID:          "gpt-5.4",
		Object:      "model",
		Created:     0,
		OwnedBy:     "notion",
		Type:        "notion",
		DisplayName: "GPT-5.4",
		Description: "Notion AI native GPT-5.4 route.",
	},
}

// GetNotionModels returns the standard Notion AI model definitions.
func GetNotionModels() []*ModelInfo {
	models := cloneModelInfos(notionModels)
	now := time.Now().Unix()
	for _, model := range models {
		if model != nil && model.Created == 0 {
			model.Created = now
		}
	}
	return models
}
