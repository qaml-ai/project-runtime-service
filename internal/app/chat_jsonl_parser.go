package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type parsedChatMessage struct {
	ID               string `json:"id"`
	ThreadID         string `json:"thread_id"`
	Role             string `json:"role"`
	Content          any    `json:"content"`
	CreatedAt        int64  `json:"created_at"`
	ForkEntryID      string `json:"forkEntryId,omitempty"`
	IsMeta           bool   `json:"isMeta,omitempty"`
	SourceToolUseID  string `json:"sourceToolUseID,omitempty"`
	IsCompactSummary bool   `json:"isCompactSummary,omitempty"`
}

type assistantSegment struct {
	ID        string
	Content   any
	CreatedAt int64
}

func parseClaudeJSONLMessages(fileContent string, threadID string) []parsedChatMessage {
	lines := strings.Split(fileContent, "\n")
	messages := make([]parsedChatMessage, 0, len(lines))

	assistantSegments := make([]assistantSegment, 0)
	assistantGroupID := ""
	var assistantGroupCreatedAt *int64

	flushAssistantGroup := func() {
		if len(assistantSegments) == 0 {
			return
		}

		content := make([]any, 0)
		for _, segment := range assistantSegments {
			if blocks, ok := asSlice(segment.Content); ok {
				content = append(content, blocks...)
			}
		}

		id := assistantGroupID
		if id == "" {
			if assistantSegments[0].ID != "" {
				id = assistantSegments[0].ID
			} else {
				id = fmt.Sprintf("assistant_%d", len(messages))
			}
		}
		forkEntryID := id
		if lastID := assistantSegments[len(assistantSegments)-1].ID; lastID != "" {
			forkEntryID = lastID
		}

		createdAt := nowMillis()
		if assistantGroupCreatedAt != nil {
			createdAt = *assistantGroupCreatedAt
		} else if assistantSegments[0].CreatedAt > 0 {
			createdAt = assistantSegments[0].CreatedAt
		}

		messages = append(messages, parsedChatMessage{
			ID:          id,
			ThreadID:    threadID,
			Role:        "assistant",
			Content:     content,
			CreatedAt:   createdAt,
			ForkEntryID: forkEntryID,
		})

		assistantSegments = assistantSegments[:0]
		assistantGroupID = ""
		assistantGroupCreatedAt = nil
	}

	upsertAssistantSegment := func(id string, content any, createdAt int64) {
		if assistantGroupID == "" {
			assistantGroupID = id
		}
		if assistantGroupCreatedAt == nil || createdAt > *assistantGroupCreatedAt {
			ts := createdAt
			assistantGroupCreatedAt = &ts
		}
		if len(assistantSegments) > 0 {
			last := &assistantSegments[len(assistantSegments)-1]
			if last.ID == id {
				last.Content = mergeContentBlocks(last.Content, content)
				return
			}
		}
		assistantSegments = append(assistantSegments, assistantSegment{
			ID:        id,
			Content:   content,
			CreatedAt: createdAt,
		})
	}

	appendToolResult := func(content any, createdAt int64) {
		if len(assistantSegments) == 0 {
			id := fmt.Sprintf("tool_result_%d", len(messages))
			upsertAssistantSegment(id, content, createdAt)
			return
		}

		last := &assistantSegments[len(assistantSegments)-1]
		existingBlocks, _ := asSlice(last.Content)
		incomingBlocks, _ := asSlice(content)
		merged := make([]any, 0, len(existingBlocks)+len(incomingBlocks))
		merged = append(merged, existingBlocks...)
		merged = append(merged, incomingBlocks...)
		last.Content = merged
		if assistantGroupCreatedAt == nil || createdAt > *assistantGroupCreatedAt {
			ts := createdAt
			assistantGroupCreatedAt = &ts
		}
	}

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		var event any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		eventMap, ok := asMap(event)
		if !ok {
			continue
		}

		eventType, _ := asString(eventMap["type"])
		eventSubtype, _ := asString(eventMap["subtype"])
		if eventType == "system" && eventSubtype == "compact_boundary" {
			continue
		}

		messageMap, _ := asMap(eventMap["message"])
		messageContent, hasMessageContent := valueIfPresent(messageMap, "content")

		if eventType == "user" && hasMessageContent {
			isMeta := firstBool(eventMap, "isMeta", "is_meta")
			if !isMeta {
				isMeta = firstBool(messageMap, "isMeta", "is_meta")
			}

			sourceToolUseID := firstString(
				eventMap,
				"sourceToolUseID",
				"sourceToolUseId",
				"source_tool_use_id",
				"parentToolUseID",
				"parentToolUseId",
				"parent_tool_use_id",
			)
			if sourceToolUseID == "" {
				sourceToolUseID = firstString(
					messageMap,
					"sourceToolUseID",
					"sourceToolUseId",
					"source_tool_use_id",
					"parentToolUseID",
					"parentToolUseId",
					"parent_tool_use_id",
				)
			}

			isToolResult := false
			if contentBlocks, ok := asSlice(messageContent); ok && len(contentBlocks) > 0 {
				if firstBlock, ok := asMap(contentBlocks[0]); ok {
					firstType, _ := asString(firstBlock["type"])
					isToolResult = firstType == "tool_result"
				}
			}

			isCompactSummary := false
			if value, ok := eventMap["isCompactSummary"]; ok {
				if parsed, ok := asBool(value); ok {
					isCompactSummary = parsed
				}
			}

			createdAt := toCreatedAt(eventMap["timestamp"])

			switch {
			case isToolResult:
				appendToolResult(messageContent, createdAt)
			case isCompactSummary:
				flushAssistantGroup()
				id := firstString(eventMap, "uuid")
				if id == "" {
					id = fmt.Sprintf("compact_%d", len(messages))
				}
				messages = append(messages, parsedChatMessage{
					ID:               id,
					ThreadID:         threadID,
					Role:             "user",
					Content:          messageContent,
					CreatedAt:        createdAt,
					ForkEntryID:      id,
					IsCompactSummary: true,
				})
			case isMeta || sourceToolUseID != "":
				id := firstString(eventMap, "uuid")
				if id == "" {
					suffix := sourceToolUseID
					if suffix == "" {
						suffix = fmt.Sprintf("%d", len(messages))
					}
					id = "meta_" + suffix
				}
				messages = append(messages, parsedChatMessage{
					ID:              id,
					ThreadID:        threadID,
					Role:            "user",
					Content:         messageContent,
					CreatedAt:       createdAt,
					ForkEntryID:     id,
					IsMeta:          true,
					SourceToolUseID: sourceToolUseID,
				})
			default:
				flushAssistantGroup()
				id := firstString(eventMap, "uuid")
				if id == "" {
					id = fmt.Sprintf("user_%d", len(messages))
				}
				messages = append(messages, parsedChatMessage{
					ID:          id,
					ThreadID:    threadID,
					Role:        "user",
					Content:     messageContent,
					CreatedAt:   createdAt,
					ForkEntryID: id,
				})
			}
			continue
		}

		if eventType == "assistant" && messageMap != nil {
			if contentBlocks, ok := asSlice(messageMap["content"]); ok && len(contentBlocks) > 0 {
				id := firstString(messageMap, "id")
				if id == "" {
					id = firstString(eventMap, "uuid")
				}
				if id == "" {
					id = fmt.Sprintf("assistant_%d", len(messages))
				}
				upsertAssistantSegment(id, messageMap["content"], toCreatedAt(eventMap["timestamp"]))
			}
		}

		if eventType == "result" && len(assistantSegments) > 0 {
			if ts, ok := parseEventTimestamp(eventMap["timestamp"]); ok {
				if assistantGroupCreatedAt == nil || ts > *assistantGroupCreatedAt {
					value := ts
					assistantGroupCreatedAt = &value
				}
			}
		}
	}

	flushAssistantGroup()
	return messages
}

func valueIfPresent(values map[string]any, key string) (any, bool) {
	if values == nil {
		return nil, false
	}
	value, ok := values[key]
	return value, ok && value != nil
}

func firstString(values map[string]any, keys ...string) string {
	if values == nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if parsed, ok := asString(value); ok {
				return parsed
			}
		}
	}
	return ""
}

func firstBool(values map[string]any, keys ...string) bool {
	if values == nil {
		return false
	}
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if parsed, ok := asBool(value); ok {
				return parsed
			}
		}
	}
	return false
}

func asMap(value any) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	parsed, ok := value.(map[string]any)
	return parsed, ok
}

func asSlice(value any) ([]any, bool) {
	if value == nil {
		return nil, false
	}
	parsed, ok := value.([]any)
	return parsed, ok
}

func asString(value any) (string, bool) {
	parsed, ok := value.(string)
	return parsed, ok
}

func asBool(value any) (bool, bool) {
	parsed, ok := value.(bool)
	return parsed, ok
}

func parseEventTimestamp(timestamp any) (int64, bool) {
	value, ok := asString(timestamp)
	if !ok {
		return 0, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0, false
	}
	return parsed.UnixMilli(), true
}

func toCreatedAt(timestamp any) int64 {
	if parsed, ok := parseEventTimestamp(timestamp); ok {
		return parsed
	}
	return nowMillis()
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}

func hasTextBlocks(content any) bool {
	blocks, ok := asSlice(content)
	if !ok {
		return false
	}
	for _, block := range blocks {
		blockMap, ok := asMap(block)
		if !ok {
			continue
		}
		blockType, _ := asString(blockMap["type"])
		text, _ := asString(blockMap["text"])
		if blockType == "text" && text != "" {
			return true
		}
	}
	return false
}

func hasBlockType(blocks []any, expectedType string) bool {
	for _, block := range blocks {
		blockMap, ok := asMap(block)
		if !ok {
			continue
		}
		blockType, _ := asString(blockMap["type"])
		if blockType == expectedType {
			return true
		}
	}
	return false
}

func mergeContentBlocks(existing any, incoming any) any {
	existingBlocks, okExisting := asSlice(existing)
	incomingBlocks, okIncoming := asSlice(incoming)
	if !okExisting || !okIncoming {
		return incoming
	}

	if !hasTextBlocks(incomingBlocks) {
		merged := make([]any, len(existingBlocks))
		copy(merged, existingBlocks)

		existingKeys := make(map[string]int, len(existingBlocks))
		for i, block := range existingBlocks {
			blockMap, _ := asMap(block)
			key := keyForContentBlock(blockMap, i)
			existingKeys[key] = i
		}

		for i, block := range incomingBlocks {
			blockMap, _ := asMap(block)
			key := keyForContentBlock(blockMap, i)
			if existingIndex, ok := existingKeys[key]; ok {
				merged[existingIndex] = block
			} else {
				merged = append(merged, block)
			}
		}
		return merged
	}

	incomingHasToolResult := hasBlockType(incomingBlocks, "tool_result")
	incomingHasThinking := hasBlockType(incomingBlocks, "thinking")
	incomingHasRedactedThinking := hasBlockType(incomingBlocks, "redacted_thinking")

	preserved := make([]any, 0, len(existingBlocks))
	for _, block := range existingBlocks {
		blockMap, ok := asMap(block)
		if !ok {
			continue
		}
		blockType, _ := asString(blockMap["type"])
		switch blockType {
		case "tool_result":
			if incomingHasToolResult {
				continue
			}
		case "thinking":
			if incomingHasThinking {
				continue
			}
		case "redacted_thinking":
			if incomingHasRedactedThinking {
				continue
			}
		default:
			continue
		}
		preserved = append(preserved, block)
	}
	if len(preserved) == 0 {
		return incoming
	}

	merged := make([]any, 0, len(preserved)+len(incomingBlocks))
	merged = append(merged, preserved...)
	merged = append(merged, incomingBlocks...)
	return merged
}

func keyForContentBlock(block map[string]any, index int) string {
	blockType := firstString(block, "type")
	if blockType == "tool_use" {
		id := firstString(block, "id")
		if id == "" {
			id = firstString(block, "name")
		}
		if id == "" {
			id = fmt.Sprintf("%d", index)
		}
		return "tool_use:" + id
	}
	return fmt.Sprintf("%s:%d", blockType, index)
}
