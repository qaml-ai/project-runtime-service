package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

func parseCodexRolloutMessages(fileContent string, camelThreadID string) []parsedChatMessage {
	response := parseCodexRolloutThreadReadResponse(fileContent)
	return parseCodexThreadReadMessages(response, camelThreadID)
}

func parseCodexRolloutThreadReadResponse(fileContent string) codexThreadReadResponse {
	lines := strings.Split(fileContent, "\n")
	turns := make([]codexTurn, 0)
	current := codexTurn{ID: "codex_rollout_turn_0"}
	turnIndex := 0

	flushTurn := func() {
		if len(current.Items) == 0 {
			return
		}
		if current.ID == "" {
			current.ID = fmt.Sprintf("codex_rollout_turn_%d", turnIndex)
		}
		turns = append(turns, current)
		turnIndex++
		current = codexTurn{ID: fmt.Sprintf("codex_rollout_turn_%d", turnIndex)}
	}

	setStartedAt := func(timestamp any) {
		if current.StartedAt != nil {
			return
		}
		seconds := codexRolloutTimestampSeconds(timestamp)
		current.StartedAt = &seconds
	}

	appendItem := func(timestamp any, item map[string]any) {
		setStartedAt(timestamp)
		if firstString(item, "id", "call_id") == "" {
			item["id"] = fmt.Sprintf("codex_rollout_item_%d", len(turns)+len(current.Items))
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return
		}
		current.Items = append(current.Items, encoded)
	}

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		var eventMap map[string]any
		if err := json.Unmarshal([]byte(line), &eventMap); err != nil {
			continue
		}
		eventType := firstString(eventMap, "type")
		payload, _ := asMap(eventMap["payload"])
		timestamp := eventMap["timestamp"]

		switch eventType {
		case "event_msg":
			switch firstString(payload, "type") {
			case "task_started":
				if len(current.Items) > 0 {
					flushTurn()
				}
				if turnID := firstString(payload, "turn_id", "id"); turnID != "" {
					current.ID = turnID
				}
				setStartedAt(timestamp)
			case "user_message":
				text := firstString(payload, "message")
				if strings.TrimSpace(text) == "" {
					continue
				}
				appendItem(timestamp, map[string]any{
					"type": "userMessage",
					"id":   fmt.Sprintf("codex_rollout_user_%d", len(turns)+len(current.Items)),
					"content": []map[string]any{
						{"type": "text", "text": text},
					},
				})
			case "task_complete":
				seconds := codexRolloutTimestampSeconds(timestamp)
				current.CompletedAt = &seconds
				flushTurn()
			}
		case "response_item":
			itemType := firstString(payload, "type")
			itemID := firstString(payload, "id", "call_id")
			if itemID == "" {
				itemID = fmt.Sprintf("codex_rollout_item_%d", len(turns)+len(current.Items))
			}
			switch itemType {
			case "message":
				if firstString(payload, "role") != "assistant" {
					continue
				}
				text := codexRolloutMessageText(payload["content"])
				if strings.TrimSpace(text) == "" {
					continue
				}
				appendItem(timestamp, map[string]any{
					"type": "agentMessage",
					"id":   itemID,
					"text": text,
				})
			case "reasoning":
				payload["id"] = itemID
				appendItem(timestamp, payload)
			case "function_call":
				payload["id"] = itemID
				appendItem(timestamp, payload)
			case "function_call_output":
				payload["id"] = itemID
				appendItem(timestamp, payload)
			default:
				if itemType == "" {
					continue
				}
				payload["id"] = itemID
				appendItem(timestamp, payload)
			}
		}
	}

	flushTurn()
	return codexThreadReadResponse{Thread: codexThread{Turns: turns}}
}

func codexRolloutMessageText(content any) string {
	blocks, ok := asSlice(content)
	if !ok {
		text, _ := asString(content)
		return text
	}
	var out strings.Builder
	for _, rawBlock := range blocks {
		block, ok := asMap(rawBlock)
		if !ok {
			continue
		}
		switch firstString(block, "type") {
		case "output_text", "text":
			out.WriteString(firstString(block, "text"))
		}
	}
	return out.String()
}

func codexRolloutTimestampSeconds(timestamp any) int64 {
	millis := toCreatedAt(timestamp)
	if millis <= 0 {
		return 0
	}
	return millis / 1000
}

func codexRolloutFunctionArguments(value any) map[string]any {
	if arguments, ok := asMap(value); ok {
		return arguments
	}
	if text, ok := asString(value); ok && strings.TrimSpace(text) != "" {
		var arguments map[string]any
		if err := json.Unmarshal([]byte(text), &arguments); err == nil && arguments != nil {
			return arguments
		}
		return map[string]any{"arguments": text}
	}
	return map[string]any{}
}
