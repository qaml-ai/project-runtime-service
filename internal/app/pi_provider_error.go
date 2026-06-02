package app

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var piSecretPattern = regexp.MustCompile(`\b(?:sk|rk)-[A-Za-z0-9_-]{12,}\b`)

func piAssistantProviderErrorText(message map[string]any) string {
	if firstString(message, "stopReason") != "error" {
		return ""
	}
	return formatPiProviderErrorMessage(firstString(message, "errorMessage"))
}

func formatPiProviderErrorMessage(raw string) string {
	detail := piProviderErrorDetail(raw)
	if detail == "" {
		return "The upstream AI provider rejected the request. Check the API key budget, billing settings, or rate limits, then try again."
	}

	lowerDetail := strings.ToLower(detail)
	lowerRaw := strings.ToLower(raw)
	provider := "upstream AI provider"
	if strings.Contains(lowerDetail, "openrouter") || strings.Contains(lowerRaw, "openrouter") {
		provider = "OpenRouter"
	}
	return fmt.Sprintf("%s rejected the request: %s", provider, detail)
}

func piProviderErrorDetail(raw string) string {
	raw = strings.TrimSpace(piSecretPattern.ReplaceAllString(raw, "[redacted-key]"))
	if raw == "" {
		return ""
	}

	if start := strings.Index(raw, "{"); start >= 0 {
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw[start:]), &payload); err == nil {
			if errorObject, ok := asMap(payload["error"]); ok {
				if message := firstString(errorObject, "message"); message != "" {
					return strings.TrimSpace(piSecretPattern.ReplaceAllString(message, "[redacted-key]"))
				}
			}
			if message := firstString(payload, "message", "error"); message != "" {
				return strings.TrimSpace(piSecretPattern.ReplaceAllString(message, "[redacted-key]"))
			}
		}
	}

	lowerRaw := strings.ToLower(raw)
	for _, prefix := range []string{
		"provider returned error:",
		"provider-returned-error:",
		"error:",
	} {
		if strings.HasPrefix(lowerRaw, prefix) {
			return strings.TrimSpace(raw[len(prefix):])
		}
	}
	return raw
}
