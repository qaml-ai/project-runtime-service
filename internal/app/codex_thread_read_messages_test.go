package app

import (
	"encoding/json"
	"testing"
)

func TestParseCodexThreadReadMessages(t *testing.T) {
	const raw = `{
		"thread": {
			"turns": [
				{
					"id": "turn-1",
					"startedAt": 100,
					"completedAt": 105,
					"items": [
						{
							"type": "userMessage",
							"id": "item-1",
							"content": [{ "type": "text", "text": "hello" }]
						},
						{
							"type": "reasoning",
							"id": "item-2",
							"summary": [{ "type": "summary_text", "text": "thinking summary" }],
							"content": []
						},
						{
							"type": "agentMessage",
							"id": "item-3",
							"text": "hi there"
						}
					]
				}
			]
		}
	}`

	var response codexThreadReadResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	messages := parseCodexThreadReadMessages(response, "camel-thread")
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %#v", len(messages), messages)
	}

	if messages[0].ThreadID != "camel-thread" || messages[0].Role != "user" || messages[0].Content != "hello" {
		t.Fatalf("unexpected user message: %#v", messages[0])
	}
	if messages[0].CreatedAt != 100_000 {
		t.Fatalf("unexpected user created_at: %d", messages[0].CreatedAt)
	}

	if messages[1].ThreadID != "camel-thread" || messages[1].Role != "assistant" {
		t.Fatalf("unexpected assistant message: %#v", messages[1])
	}
	if messages[1].CreatedAt != 105_000 {
		t.Fatalf("unexpected assistant created_at: %d", messages[1].CreatedAt)
	}

	blocks, ok := messages[1].Content.([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("unexpected assistant blocks: %#v", messages[1].Content)
	}
	reasoning, ok := blocks[0].(map[string]any)
	if !ok || reasoning["type"] != "thinking" || reasoning["label"] != "Reasoning" {
		t.Fatalf("unexpected reasoning block: %#v", blocks[0])
	}
	summaries, ok := reasoning["summaries"].([]string)
	if !ok || len(summaries) != 1 || summaries[0] != "thinking summary" {
		t.Fatalf("unexpected reasoning summaries: %#v", reasoning["summaries"])
	}
	text, ok := blocks[1].(map[string]any)
	if !ok || text["type"] != "text" || text["text"] != "hi there" {
		t.Fatalf("unexpected text block: %#v", blocks[1])
	}
}
