package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePiJSONLMessagesBasicFlow(t *testing.T) {
	jsonl := `{"type":"session","version":3,"id":"session-1","timestamp":"2026-01-02T03:04:04.000Z","cwd":"/tmp/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-02T03:04:05.000Z","message":{"role":"user","content":[{"type":"text","text":"[Local Dev (local-dev@camelai.local)]: hello"}],"timestamp":1770000000000}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"checking"},{"type":"text","text":"hi"},{"type":"toolCall","id":"tool-1","name":"read","arguments":{"path":"README.md"}}],"timestamp":1770000000001}}
{"type":"message","id":"tr1","parentId":"a1","timestamp":"2026-01-02T03:04:07.000Z","message":{"role":"toolResult","toolCallId":"tool-1","toolName":"read","content":[{"type":"text","text":"file contents"}],"isError":false,"timestamp":1770000000002}}
{"type":"message","id":"a2","parentId":"tr1","timestamp":"2026-01-02T03:04:08.000Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":1770000000003}}`

	messages := parsePiJSONLMessages(jsonl, "thread-1")
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %#v", len(messages), messages)
	}

	if messages[0].ID != "u1" || messages[0].Role != "user" {
		t.Fatalf("unexpected user message: %#v", messages[0])
	}
	userBlocks, ok := asSlice(messages[0].Content)
	if !ok || len(userBlocks) != 1 {
		t.Fatalf("unexpected user blocks: %#v", messages[0].Content)
	}

	if messages[1].ID != "a1" || messages[1].Role != "assistant" {
		t.Fatalf("unexpected assistant message: %#v", messages[1])
	}
	if messages[1].ForkEntryID != "a2" {
		t.Fatalf("expected assistant fork entry id to use leaf a2, got %q", messages[1].ForkEntryID)
	}
	assistantBlocks, ok := asSlice(messages[1].Content)
	if !ok {
		t.Fatalf("expected assistant content blocks, got %#v", messages[1].Content)
	}
	if len(assistantBlocks) != 5 {
		t.Fatalf("expected thinking, text, tool_use, tool_result, text blocks, got %d: %#v", len(assistantBlocks), assistantBlocks)
	}

	toolUse, _ := asMap(assistantBlocks[2])
	if firstString(toolUse, "type") != "tool_use" || firstString(toolUse, "name") != "Read" {
		t.Fatalf("unexpected tool_use block: %#v", toolUse)
	}
	toolResult, _ := asMap(assistantBlocks[3])
	if firstString(toolResult, "type") != "tool_result" || firstString(toolResult, "tool_use_id") != "tool-1" {
		t.Fatalf("unexpected tool_result block: %#v", toolResult)
	}
	if content := firstString(toolResult, "content"); content != "file contents" {
		t.Fatalf("unexpected tool result content %q", content)
	}
}

func TestParsePiJSONLMessagesPreservesToolResultImages(t *testing.T) {
	jsonl := `{"type":"message","id":"a1","timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tool-1","name":"read","arguments":{"path":"image.png"}}],"timestamp":1770000000001}}
{"type":"message","id":"tr1","parentId":"a1","timestamp":"2026-01-02T03:04:07.000Z","message":{"role":"toolResult","toolCallId":"tool-1","toolName":"read","content":[{"type":"text","text":"Read image file [image/png]"},{"type":"image","data":"abc123","mimeType":"image/png"}],"isError":false,"timestamp":1770000000002}}`

	messages := parsePiJSONLMessages(jsonl, "thread-1")
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d: %#v", len(messages), messages)
	}
	blocks, ok := asSlice(messages[0].Content)
	if !ok || len(blocks) != 2 {
		t.Fatalf("unexpected blocks: %#v", messages[0].Content)
	}
	toolResult, _ := asMap(blocks[1])
	contentBlocks, ok := asSlice(toolResult["content"])
	if !ok || len(contentBlocks) != 2 {
		t.Fatalf("expected structured tool result content, got %#v", toolResult["content"])
	}
	imageBlock, _ := asMap(contentBlocks[1])
	if firstString(imageBlock, "type") != "image" || firstString(imageBlock, "mimeType") != "image/png" || firstString(imageBlock, "data") != "abc123" {
		t.Fatalf("image block was not preserved: %#v", imageBlock)
	}
}

func TestParsePiJSONLMessagesCanonicalizesToolNames(t *testing.T) {
	jsonl := `{"type":"message","id":"a1","timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tool-bash","name":"bash","arguments":{"command":"pwd"}},{"type":"toolCall","id":"tool-search","name":"web_search","arguments":{"query":"docs"}},{"type":"toolCall","id":"tool-fetch","name":"web_fetch","arguments":{"url":"https://example.com"}}],"timestamp":1770000000001}}`

	messages := parsePiJSONLMessages(jsonl, "thread-1")
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d: %#v", len(messages), messages)
	}
	blocks, ok := asSlice(messages[0].Content)
	if !ok || len(blocks) != 3 {
		t.Fatalf("unexpected blocks: %#v", messages[0].Content)
	}

	expectedNames := []string{"Bash", "WebSearch", "WebFetch"}
	for index, expected := range expectedNames {
		block, _ := asMap(blocks[index])
		if got := firstString(block, "name"); got != expected {
			t.Fatalf("block %d name = %q, want %q: %#v", index, got, expected, block)
		}
	}
}

func TestParsePiJSONLMessagesKeepsLateThinkingChronological(t *testing.T) {
	jsonl := `{"type":"message","id":"a1","timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"first"},{"type":"text","text":"final answer"},{"type":"thinking","thinking":"late redacted"}],"timestamp":1770000000001}}`

	messages := parsePiJSONLMessages(jsonl, "thread-1")
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d: %#v", len(messages), messages)
	}
	blocks, ok := asSlice(messages[0].Content)
	if !ok || len(blocks) != 3 {
		t.Fatalf("unexpected blocks: %#v", messages[0].Content)
	}
	got := make([]string, 0, len(blocks))
	for _, rawBlock := range blocks {
		block, _ := asMap(rawBlock)
		got = append(got, firstString(block, "type")+":"+firstString(block, "thinking", "text"))
	}
	want := []string{"thinking:first", "text:final answer", "thinking:late redacted"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("block order = %#v, want %#v", got, want)
		}
	}
}

func TestParsePiJSONLMessagesShowsAssistantProviderError(t *testing.T) {
	jsonl := `{"type":"message","id":"a1","timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"403 {\"error\":{\"message\":\"Key limit exceeded (total limit). Manage it using https://openrouter.ai/settings/keys\",\"code\":403}}"}}`

	messages := parsePiJSONLMessages(jsonl, "thread-1")
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d: %#v", len(messages), messages)
	}
	blocks, ok := asSlice(messages[0].Content)
	if !ok || len(blocks) != 1 {
		t.Fatalf("unexpected blocks: %#v", messages[0].Content)
	}
	block, _ := asMap(blocks[0])
	got := firstString(block, "text")
	want := "OpenRouter rejected the request: Key limit exceeded (total limit). Manage it using https://openrouter.ai/settings/keys"
	if got != want {
		t.Fatalf("provider error text = %q, want %q", got, want)
	}
}

func TestReadHostPiSessionMessagesCombinesJSONLFiles(t *testing.T) {
	root := t.TempDir()
	threadID := "thread-1"
	sessionDir := filepath.Join(root, threadID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	first := `{"type":"message","id":"u1","timestamp":"2026-01-02T03:04:05.000Z","message":{"role":"user","content":[{"type":"text","text":"one"}]}}`
	second := `{"type":"message","id":"u2","timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"user","content":[{"type":"text","text":"two"}]}}`
	if err := os.WriteFile(filepath.Join(sessionDir, "2026-01-02T03-04-05Z_a.jsonl"), []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "2026-01-02T03-04-06Z_b.jsonl"), []byte(second), 0o644); err != nil {
		t.Fatal(err)
	}

	messages, err := readHostPiSessionMessages(root, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].ID != "u1" || messages[1].ID != "u2" {
		t.Fatalf("messages not read in filename order: %#v", messages)
	}
}

func TestReadHostPiSessionMessagesAppliesLatestCompaction(t *testing.T) {
	root := t.TempDir()
	threadID := "thread-1"
	sessionDir := filepath.Join(root, threadID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	lines := strings.Join([]string{
		`{"type":"message","id":"u1","timestamp":"2026-01-02T03:04:05.000Z","message":{"role":"user","content":[{"type":"text","text":"old"}]}}`,
		`{"type":"message","id":"u2","timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"user","content":[{"type":"text","text":"kept"}]}}`,
		`{"type":"message","id":"a1","timestamp":"2026-01-02T03:04:07.000Z","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}`,
		`{"type":"compaction","timestamp":"2026-01-02T03:04:08.000Z","summary":"old summary","firstKeptEntryId":"u2"}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(sessionDir, "2026-01-02T03-04-05Z_a.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	messages, err := readHostPiSessionMessages(root, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("expected summary plus kept tail, got %d messages: %#v", len(messages), messages)
	}
	if !messages[0].IsCompactSummary || messages[0].Role != "user" {
		t.Fatalf("expected synthetic compaction summary user message, got %#v", messages[0])
	}
	blocks, ok := asSlice(messages[0].Content)
	if !ok || len(blocks) != 1 {
		t.Fatalf("unexpected summary content: %#v", messages[0].Content)
	}
	block, _ := asMap(blocks[0])
	if text := firstString(block, "text"); !strings.Contains(text, "[Context Summary]\n\nold summary") {
		t.Fatalf("unexpected summary text: %q", text)
	}
	if messages[1].ID != "u2" || messages[2].ID != "a1" {
		t.Fatalf("expected kept tail to start at u2, got %#v", messages)
	}
}

func TestReadHostPiSessionMessagesSkipsMalformedJSONLLines(t *testing.T) {
	root := t.TempDir()
	threadID := "thread-1"
	sessionDir := filepath.Join(root, threadID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	lines := strings.Join([]string{
		`{"type":"message","id":"u1","timestamp":"2026-01-02T03:04:05.000Z","message":{"role":"user","content":[{"type":"text","text":"old"}]}}`,
		`{"type":"compaction","timestamp":"2026-01-02T03:04:06.000Z","summary":"old summary","firstKeptEntryId":"u2"}`,
		`{"type":"message","id":"broken","timestamp":"2026-01-02T03:04:06.500Z","message":`,
		`{"type":"message","id":"u2","timestamp":"2026-01-02T03:04:07.000Z","message":{"role":"user","content":[{"type":"text","text":"kept"}]}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(sessionDir, "2026-01-02T03-04-05Z_a.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	messages, err := readHostPiSessionMessages(root, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected summary plus kept message, got %d messages: %#v", len(messages), messages)
	}
	if !messages[0].IsCompactSummary || messages[1].ID != "u2" {
		t.Fatalf("malformed line should be skipped without losing compaction: %#v", messages)
	}
}
