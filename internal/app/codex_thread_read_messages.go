package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

type codexThreadReadResponse struct {
	Thread codexThread `json:"thread"`
}

type codexThread struct {
	Turns []codexTurn `json:"turns"`
}

type codexTurn struct {
	ID          string            `json:"id"`
	Items       []json.RawMessage `json:"items"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
}

func parseCodexThreadReadMessages(response codexThreadReadResponse, camelThreadID string) []parsedChatMessage {
	messages := make([]parsedChatMessage, 0)
	for _, turn := range response.Thread.Turns {
		assistantBlocks := make([]any, 0)
		assistantCreatedAt := codexTurnCreatedAt(turn)

		for _, rawItem := range turn.Items {
			item := codexItemMap(rawItem)
			itemType := codexString(item["type"])
			itemID := codexString(item["id"])
			if itemID == "" {
				itemID = fmt.Sprintf("codex_item_%d", len(messages)+len(assistantBlocks))
			}

			switch itemType {
			case "userMessage":
				text := extractCodexItemText(item)
				if strings.TrimSpace(text) == "" {
					continue
				}
				if len(assistantBlocks) > 0 {
					messages = append(messages, codexAssistantMessage(turn, camelThreadID, assistantBlocks, assistantCreatedAt))
					assistantBlocks = make([]any, 0)
				}
				messages = append(messages, parsedChatMessage{
					ID:        itemID,
					ThreadID:  camelThreadID,
					Role:      "user",
					Content:   text,
					CreatedAt: codexTurnStartedAt(turn),
				})
			case "hookPrompt":
				continue
			case "agentMessage":
				text := codexString(item["text"])
				if text == "" {
					text = extractCodexItemText(item)
				}
				if strings.TrimSpace(text) == "" {
					continue
				}
				assistantBlocks = append(assistantBlocks, map[string]any{
					"type":     "text",
					"text":     text,
					"itemId":   itemID,
					"itemKind": itemType,
				})
			case "plan":
				text := codexString(item["text"])
				if strings.TrimSpace(text) == "" {
					continue
				}
				assistantBlocks = append(assistantBlocks, map[string]any{
					"type":      "thinking",
					"thinking":  text,
					"itemId":    itemID,
					"itemKind":  itemType,
					"label":     "Plan",
					"summaries": []string{},
				})
			case "reasoning":
				text := extractCodexReasoningContent(item)
				summaries := extractCodexStringSlice(item["summary"])
				if strings.TrimSpace(text) == "" && len(summaries) == 0 {
					continue
				}
				assistantBlocks = append(assistantBlocks, map[string]any{
					"type":      "thinking",
					"thinking":  text,
					"itemId":    itemID,
					"itemKind":  itemType,
					"label":     "Reasoning",
					"summaries": summaries,
				})
			case "function_call":
				name := codexString(item["name"])
				if name == "" {
					name = "function_call"
				}
				toolID := codexString(item["call_id"])
				if toolID == "" {
					toolID = itemID
				}
				assistantBlocks = append(assistantBlocks, map[string]any{
					"type":     "tool_use",
					"id":       toolID,
					"name":     name,
					"input":    codexRolloutFunctionArguments(item["arguments"]),
					"itemKind": itemType,
				})
			case "function_call_output":
				callID := codexString(item["call_id"])
				content := item["output"]
				if text, ok := asString(content); ok {
					content = text
				}
				assistantBlocks = append(assistantBlocks, map[string]any{
					"type":        "tool_result",
					"tool_use_id": callID,
					"content":     content,
					"itemId":      itemID,
					"itemKind":    itemType,
				})
			default:
				if itemType == "" {
					continue
				}
				assistantBlocks = append(assistantBlocks, codexGenericToolBlocks(itemID, itemType, item)...)
			}
		}

		if len(assistantBlocks) > 0 {
			messages = append(messages, codexAssistantMessage(turn, camelThreadID, assistantBlocks, assistantCreatedAt))
		}
	}
	return messages
}

func codexAssistantMessage(turn codexTurn, camelThreadID string, blocks []any, createdAt int64) parsedChatMessage {
	id := turn.ID
	if id == "" {
		id = fmt.Sprintf("codex_assistant_%d", createdAt)
	}
	return parsedChatMessage{
		ID:        "assistant_" + id,
		ThreadID:  camelThreadID,
		Role:      "assistant",
		Content:   blocks,
		CreatedAt: createdAt,
	}
}

func codexGenericToolBlocks(itemID, itemType string, item map[string]any) []any {
	input := make(map[string]any, len(item))
	for key, value := range item {
		if key == "id" || key == "type" {
			continue
		}
		input[key] = value
	}

	result := ""
	if len(input) > 0 {
		if encoded, err := json.Marshal(input); err == nil {
			result = string(encoded)
		}
	}
	if result == "" {
		result = itemType
	}

	return []any{
		map[string]any{
			"type":     "tool_use",
			"id":       itemID,
			"name":     "Codex:" + itemType,
			"input":    input,
			"itemKind": itemType,
		},
		map[string]any{
			"type":        "tool_result",
			"tool_use_id": itemID,
			"content":     result,
			"itemId":      itemID,
			"itemKind":    itemType,
		},
	}
}

func codexItemMap(raw json.RawMessage) map[string]any {
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return map[string]any{}
	}
	return item
}

func codexString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func extractCodexStringSlice(value any) []string {
	values, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text := codexString(value); text != "" {
			out = append(out, text)
			continue
		}
		valueMap, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if text := codexString(valueMap["text"]); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func extractCodexItemText(item map[string]any) string {
	if text := codexString(item["text"]); text != "" {
		return text
	}

	parts, ok := item["content"].([]any)
	if !ok {
		return ""
	}

	var text strings.Builder
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		partType := codexString(partMap["type"])
		if partType == "text" || partType == "input_text" || partType == "output_text" {
			text.WriteString(codexString(partMap["text"]))
		}
	}
	return text.String()
}

func extractCodexReasoningContent(item map[string]any) string {
	if text := codexString(item["content"]); text != "" {
		return text
	}

	parts, ok := item["content"].([]any)
	if !ok {
		return ""
	}

	var text strings.Builder
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		text.WriteString(codexString(partMap["text"]))
	}
	return text.String()
}

func codexTurnStartedAt(turn codexTurn) int64 {
	if turn.StartedAt != nil && *turn.StartedAt > 0 {
		return *turn.StartedAt * 1000
	}
	return nowMillis()
}

func codexTurnCreatedAt(turn codexTurn) int64 {
	if turn.CompletedAt != nil && *turn.CompletedAt > 0 {
		return *turn.CompletedAt * 1000
	}
	return codexTurnStartedAt(turn)
}
