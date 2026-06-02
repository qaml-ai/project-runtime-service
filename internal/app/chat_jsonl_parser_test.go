package app

import (
	"fmt"
	"testing"
	"time"
)

func TestParseClaudeJSONLMessagesBasicFlow(t *testing.T) {
	userTS := "2026-01-02T03:04:05.000Z"
	assistantTS := "2026-01-02T03:04:06.000Z"
	resultTS := "2026-01-02T03:04:07.000Z"

	jsonl := fmt.Sprintf(`{"type":"user","uuid":"u1","timestamp":"%s","message":{"content":[{"type":"text","text":"hello"}]}}
{"type":"assistant","timestamp":"%s","message":{"id":"a1","content":[{"type":"text","text":"hi"}]}}
{"type":"result","timestamp":"%s"}`, userTS, assistantTS, resultTS)

	messages := parseClaudeJSONLMessages(jsonl, "thread-1")
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	if messages[0].Role != "user" || messages[0].ID != "u1" {
		t.Fatalf("unexpected first message: %+v", messages[0])
	}

	expectedUserTS := mustParseRFC3339Millis(t, userTS)
	if messages[0].CreatedAt != expectedUserTS {
		t.Fatalf("unexpected user timestamp: got %d want %d", messages[0].CreatedAt, expectedUserTS)
	}

	if messages[1].Role != "assistant" || messages[1].ID != "a1" {
		t.Fatalf("unexpected second message: %+v", messages[1])
	}

	expectedAssistantTS := mustParseRFC3339Millis(t, resultTS)
	if messages[1].CreatedAt != expectedAssistantTS {
		t.Fatalf("unexpected assistant timestamp: got %d want %d", messages[1].CreatedAt, expectedAssistantTS)
	}
}

func TestParseClaudeJSONLMessagesMetaAndCompactSummary(t *testing.T) {
	jsonl := `{"type":"assistant","timestamp":"2026-01-02T03:04:06.000Z","message":{"id":"a1","content":[{"type":"text","text":"first"}]}}
{"type":"user","uuid":"compact-1","timestamp":"2026-01-02T03:04:07.000Z","isCompactSummary":true,"message":{"content":[{"type":"text","text":"summary"}]}}
{"type":"user","uuid":"meta-1","timestamp":"2026-01-02T03:04:08.000Z","message":{"is_meta":true,"source_tool_use_id":"tool-123","content":[{"type":"text","text":"meta"}]}}`

	messages := parseClaudeJSONLMessages(jsonl, "thread-2")
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}

	if messages[0].Role != "assistant" {
		t.Fatalf("expected first message assistant, got %+v", messages[0])
	}
	if !messages[1].IsCompactSummary || messages[1].ID != "compact-1" {
		t.Fatalf("expected compact summary message, got %+v", messages[1])
	}
	if !messages[2].IsMeta || messages[2].SourceToolUseID != "tool-123" {
		t.Fatalf("expected meta message with sourceToolUseID, got %+v", messages[2])
	}
}

func TestParseClaudeJSONLMessagesCamelCaseParentToolUseID(t *testing.T) {
	jsonl := `{"type":"assistant","timestamp":"2026-01-02T03:04:06.000Z","message":{"id":"a1","content":[{"type":"tool_use","id":"tool-parent","name":"Agent","input":{"prompt":"explore"}}]}}
{"type":"user","uuid":"meta-1","timestamp":"2026-01-02T03:04:07.000Z","parentToolUseID":"tool-parent","message":{"content":[{"type":"text","text":"meta"}]}}`

	messages := parseClaudeJSONLMessages(jsonl, "thread-3")
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	for _, message := range messages {
		if message.IsMeta {
			if message.SourceToolUseID != "tool-parent" {
				t.Fatalf("expected camel-case parentToolUseID to populate sourceToolUseID, got %+v", message)
			}
			return
		}
	}

	t.Fatalf("expected a meta message, got %+v", messages)
}

func TestParseClaudeJSONLMessagesThinkingPreserved(t *testing.T) {
	ts1 := "2026-01-02T03:04:05.000Z"
	ts2 := "2026-01-02T03:04:06.000Z"
	resultTS := "2026-01-02T03:04:07.000Z"

	jsonl := fmt.Sprintf(
		`{"type":"user","uuid":"u1","timestamp":"%s","message":{"content":[{"type":"text","text":"hello"}]}}
{"type":"assistant","timestamp":"%s","message":{"id":"a1","content":[{"type":"thinking","thinking":"Let me analyze this","signature":"sig123"}]}}
{"type":"assistant","timestamp":"%s","message":{"id":"a1","content":[{"type":"text","text":"Here is my response"}]}}
{"type":"result","timestamp":"%s"}`,
		ts1, ts1, ts2, resultTS)

	messages := parseClaudeJSONLMessages(jsonl, "thread-1")
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	assistantMsg := messages[1]
	contentBlocks, ok := asSlice(assistantMsg.Content)
	if !ok {
		t.Fatalf("expected content to be a slice")
	}
	if len(contentBlocks) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d", len(contentBlocks))
	}

	thinkingBlock, _ := asMap(contentBlocks[0])
	blockType, _ := asString(thinkingBlock["type"])
	if blockType != "thinking" {
		t.Fatalf("expected first block type 'thinking', got %q", blockType)
	}
	thinkingText, _ := asString(thinkingBlock["thinking"])
	if thinkingText != "Let me analyze this" {
		t.Fatalf("expected thinking text preserved, got %q", thinkingText)
	}

	textBlock, _ := asMap(contentBlocks[1])
	textType, _ := asString(textBlock["type"])
	if textType != "text" {
		t.Fatalf("expected second block type 'text', got %q", textType)
	}
}

func TestParseClaudeJSONLMessagesThinkingTextToolUsePreserved(t *testing.T) {
	ts := "2026-01-02T03:04:05.000Z"
	resultTS := "2026-01-02T03:04:07.000Z"

	jsonl := fmt.Sprintf(
		`{"type":"user","uuid":"u1","timestamp":"%s","message":{"content":[{"type":"text","text":"hello"}]}}
{"type":"assistant","timestamp":"%s","message":{"id":"a1","content":[{"type":"thinking","thinking":"reasoning"}]}}
{"type":"assistant","timestamp":"%s","message":{"id":"a1","content":[{"type":"text","text":"response"},{"type":"tool_use","id":"tool1","name":"bash","input":{"cmd":"ls"}}]}}
{"type":"result","timestamp":"%s"}`,
		ts, ts, ts, resultTS)

	messages := parseClaudeJSONLMessages(jsonl, "thread-1")
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	contentBlocks, ok := asSlice(messages[1].Content)
	if !ok {
		t.Fatalf("expected content to be a slice")
	}
	if len(contentBlocks) != 3 {
		t.Fatalf("expected 3 content blocks (thinking + text + tool_use), got %d", len(contentBlocks))
	}

	types := make([]string, len(contentBlocks))
	for i, block := range contentBlocks {
		blockMap, _ := asMap(block)
		types[i], _ = asString(blockMap["type"])
	}
	expected := []string{"thinking", "text", "tool_use"}
	for i, exp := range expected {
		if types[i] != exp {
			t.Fatalf("block %d: expected type %q, got %q (all types: %v)", i, exp, types[i], types)
		}
	}
}

func TestParseClaudeJSONLMessagesThinkingNotDuplicatedOnCumulativeSnapshot(t *testing.T) {
	ts1 := "2026-01-02T03:04:05.000Z"
	ts2 := "2026-01-02T03:04:06.000Z"
	resultTS := "2026-01-02T03:04:07.000Z"

	jsonl := fmt.Sprintf(
		`{"type":"user","uuid":"u1","timestamp":"%s","message":{"content":[{"type":"text","text":"hello"}]}}
{"type":"assistant","timestamp":"%s","message":{"id":"a1","content":[{"type":"thinking","thinking":"Let me analyze this","signature":"sig123"}]}}
{"type":"assistant","timestamp":"%s","message":{"id":"a1","content":[{"type":"thinking","thinking":"Let me analyze this","signature":"sig123"},{"type":"text","text":"Here is my response"}]}}
{"type":"result","timestamp":"%s"}`,
		ts1, ts1, ts2, resultTS)

	messages := parseClaudeJSONLMessages(jsonl, "thread-1")
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	contentBlocks, ok := asSlice(messages[1].Content)
	if !ok {
		t.Fatalf("expected content to be a slice")
	}
	if len(contentBlocks) != 2 {
		t.Fatalf("expected 2 content blocks (thinking + text), got %d", len(contentBlocks))
	}

	first, _ := asMap(contentBlocks[0])
	firstType, _ := asString(first["type"])
	if firstType != "thinking" {
		t.Fatalf("expected first block type 'thinking', got %q", firstType)
	}

	second, _ := asMap(contentBlocks[1])
	secondType, _ := asString(second["type"])
	if secondType != "text" {
		t.Fatalf("expected second block type 'text', got %q", secondType)
	}
}

func mustParseRFC3339Millis(t *testing.T, value string) int64 {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UnixMilli()
}
