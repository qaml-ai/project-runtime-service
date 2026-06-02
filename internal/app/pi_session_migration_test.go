package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildHostPiMigrationJSONLFromClaudeMessages(t *testing.T) {
	claudeJSONL := `{"type":"user","uuid":"u1","timestamp":"2026-01-02T03:04:05.000Z","message":{"content":[{"type":"text","text":"hello"}]}}
{"type":"assistant","timestamp":"2026-01-02T03:04:06.000Z","message":{"id":"a1","content":[{"type":"thinking","thinking":"checking"},{"type":"text","text":"I'll read it"},{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"README.md"}}]}}
{"type":"user","uuid":"tr1","timestamp":"2026-01-02T03:04:07.000Z","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"file contents"}]}}
{"type":"assistant","timestamp":"2026-01-02T03:04:08.000Z","message":{"id":"a2","content":[{"type":"text","text":"done"}]}}
{"type":"result","timestamp":"2026-01-02T03:04:09.000Z"}`

	legacyMessages := parseClaudeJSONLMessages(claudeJSONL, "thread-1")
	if len(legacyMessages) != 2 {
		t.Fatalf("expected 2 legacy messages, got %d", len(legacyMessages))
	}

	migrated, err := buildHostPiMigrationJSONL("thread-1", "/workspace", "claude", legacyMessages)
	if err != nil {
		t.Fatal(err)
	}

	piMessages := parsePiJSONLMessages(migrated, "thread-1")
	if len(piMessages) != 2 {
		t.Fatalf("expected 2 Pi messages, got %d: %#v", len(piMessages), piMessages)
	}
	if piMessages[0].Role != "user" {
		t.Fatalf("expected first Pi message user, got %#v", piMessages[0])
	}
	if piMessages[1].Role != "assistant" {
		t.Fatalf("expected second Pi message assistant, got %#v", piMessages[1])
	}
	assertMigratedPiAssistantMetadata(t, migrated)
	blocks, ok := asSlice(piMessages[1].Content)
	if !ok {
		t.Fatalf("expected assistant content blocks, got %#v", piMessages[1].Content)
	}
	if len(blocks) != 5 {
		t.Fatalf("expected thinking, text, tool_use, tool_result, text blocks, got %d: %#v", len(blocks), blocks)
	}
	toolUse, _ := asMap(blocks[2])
	if firstString(toolUse, "type") != "tool_use" || firstString(toolUse, "name") != "Read" {
		t.Fatalf("unexpected migrated tool use: %#v", toolUse)
	}
	toolResult, _ := asMap(blocks[3])
	if firstString(toolResult, "type") != "tool_result" || firstString(toolResult, "tool_use_id") != "tool-1" {
		t.Fatalf("unexpected migrated tool result: %#v", toolResult)
	}
	if firstString(toolResult, "content") != "file contents" {
		t.Fatalf("unexpected migrated tool result content: %#v", toolResult)
	}
}

func TestBuildHostPiMigrationJSONLFromCodexMessages(t *testing.T) {
	raw := `{"timestamp":"2026-04-14T18:53:31.000Z","type":"event_msg","payload":{"type":"user_message","message":"hello codex"}}
{"timestamp":"2026-04-14T18:53:32.000Z","type":"response_item","payload":{"type":"function_call","id":"item-3","name":"web_search","arguments":"{\"query\":\"docs\"}","call_id":"call_1"}}
{"timestamp":"2026-04-14T18:53:33.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"search results"}}
{"timestamp":"2026-04-14T18:53:34.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"hi from codex"}]}}
{"timestamp":"2026-04-14T18:53:35.000Z","type":"event_msg","payload":{"type":"task_complete"}}`

	legacyMessages := parseCodexRolloutMessages(raw, "thread-1")
	migrated, err := buildHostPiMigrationJSONL("thread-1", "/workspace", "codex", legacyMessages)
	if err != nil {
		t.Fatal(err)
	}

	piMessages := parsePiJSONLMessages(migrated, "thread-1")
	if len(piMessages) != 2 {
		t.Fatalf("expected 2 Pi messages, got %d: %#v", len(piMessages), piMessages)
	}
	blocks, ok := asSlice(piMessages[1].Content)
	if !ok || len(blocks) < 3 {
		t.Fatalf("unexpected assistant blocks: %#v", piMessages[1].Content)
	}
	var toolUse map[string]any
	for _, block := range blocks {
		blockMap, _ := asMap(block)
		if firstString(blockMap, "type") == "tool_use" {
			toolUse = blockMap
			break
		}
	}
	if firstString(toolUse, "type") != "tool_use" || firstString(toolUse, "name") != "WebSearch" {
		t.Fatalf("unexpected migrated Codex tool use: %#v", toolUse)
	}
	assertMigratedPiAssistantMetadata(t, migrated)
}

func TestBuildHostPiMigrationJSONLConvertsOrphanToolResultsToUserText(t *testing.T) {
	messages := []parsedChatMessage{
		{
			ID:        "legacy-assistant",
			Role:      "assistant",
			CreatedAt: 1000,
			Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "missing-tool", "content": "orphan output"},
				map[string]any{"type": "text", "text": "after orphan"},
			},
		},
	}

	migrated, err := buildHostPiMigrationJSONL("thread-1", "/workspace", "claude", messages)
	if err != nil {
		t.Fatal(err)
	}

	var sawUserText bool
	for _, line := range strings.Split(migrated, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Type    string         `json:"type"`
			Message map[string]any `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "message" {
			continue
		}
		if firstString(event.Message, "role") == "toolResult" {
			t.Fatalf("orphan tool result should not be emitted as Pi toolResult: %#v", event.Message)
		}
		if firstString(event.Message, "role") == "user" {
			blocks, _ := asSlice(event.Message["content"])
			if len(blocks) > 0 {
				block, _ := asMap(blocks[0])
				text := firstString(block, "text")
				sawUserText = strings.Contains(text, "unmatched tool call missing-tool") && strings.Contains(text, "orphan output")
			}
		}
	}
	if !sawUserText {
		t.Fatalf("expected orphan tool result to become user text, got:\n%s", migrated)
	}
}

func TestBuildHostPiMigrationJSONLDeDuplicatesLegacyEntryIDs(t *testing.T) {
	messages := []parsedChatMessage{
		{
			ID:        "duplicate-id",
			Role:      "user",
			CreatedAt: 1000,
			Content:   []any{map[string]any{"type": "text", "text": "first"}},
		},
		{
			ID:        "duplicate-id",
			Role:      "user",
			CreatedAt: 2000,
			Content:   []any{map[string]any{"type": "text", "text": "second"}},
		},
		{
			ID:        "duplicate-id",
			Role:      "assistant",
			CreatedAt: 3000,
			Content: []any{
				map[string]any{"type": "text", "text": "third"},
				map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
				map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "result"},
				map[string]any{"type": "text", "text": "fourth"},
			},
		},
	}

	migrated, err := buildHostPiMigrationJSONL("thread-duplicates", "/workspace", "claude", messages)
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]int)
	for lineIndex, line := range strings.Split(migrated, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		id := firstString(event, "id")
		if id == "" {
			continue
		}
		if previousLine := seen[id]; previousLine > 0 {
			t.Fatalf("duplicate migrated entry id %q on lines %d and %d", id, previousLine, lineIndex+1)
		}
		seen[id] = lineIndex + 1
	}
	if seen["duplicate-id"] == 0 || seen["duplicate-id_dup_2"] == 0 {
		t.Fatalf("expected duplicate legacy id to be preserved then suffixed, saw ids %#v", seen)
	}
}

func TestBuildHostPiMigrationJSONLTrimsMediaAndLargeToolOutputs(t *testing.T) {
	largeToolOutput := strings.Repeat("tool-output ", hostPiMigrationMaxToolResultTextChars)
	largeArgument := strings.Repeat("argument-data ", hostPiMigrationMaxToolArgumentStringChars)
	largeUserText := strings.Repeat("user-text ", hostPiMigrationMaxTextBlockChars)
	messages := []parsedChatMessage{
		{
			ID:        "user-with-image",
			Role:      "user",
			CreatedAt: 1000,
			Content: []any{
				map[string]any{"type": "text", "text": largeUserText},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": strings.Repeat("a", 50000)}},
			},
		},
		{
			ID:        "assistant-tool",
			Role:      "assistant",
			CreatedAt: 2000,
			Content: []any{
				map[string]any{
					"type": "tool_use",
					"id":   "tool-1",
					"name": "Read",
					"input": map[string]any{
						"path":  "large.txt",
						"image": strings.Repeat("b", 50000),
						"query": largeArgument,
					},
				},
				map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": largeToolOutput},
				map[string]any{"type": "tool_result", "tool_use_id": "missing-image", "content": []any{
					map[string]any{"type": "image", "source": map[string]any{"data": strings.Repeat("c", 50000)}},
				}},
			},
		},
	}

	migrated, err := buildHostPiMigrationJSONL("thread-trim", "/workspace", "claude", messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(migrated, strings.Repeat("a", 1000)) || strings.Contains(migrated, strings.Repeat("b", 1000)) || strings.Contains(migrated, strings.Repeat("c", 1000)) {
		t.Fatalf("migrated session still contains large media payload")
	}
	if strings.Contains(migrated, largeToolOutput[:1000]) {
		t.Fatalf("migrated session still contains large tool output")
	}
	if strings.Contains(migrated, largeUserText[:5000]) {
		t.Fatalf("migrated session still contains untrimmed large text block")
	}
	if !strings.Contains(migrated, "Legacy text block truncated during migration") {
		t.Fatalf("expected text truncation marker, got:\n%s", migrated)
	}
	if !strings.Contains(migrated, "Legacy image attachment omitted during migration") {
		t.Fatalf("expected image omission marker, got:\n%s", migrated)
	}
	if !strings.Contains(migrated, "Legacy tool output omitted during migration") {
		t.Fatalf("expected tool output omission marker, got:\n%s", migrated)
	}
	if !strings.Contains(migrated, "legacy media attachment(s) omitted during migration") {
		t.Fatalf("expected tool-result media omission marker, got:\n%s", migrated)
	}
}

func TestBuildHostPiMigrationJSONLAddsPiCompactionForOversizedImport(t *testing.T) {
	largeText := strings.Repeat("legacy context ", 700)
	messages := make([]parsedChatMessage, 0, 90)
	for i := 0; i < 90; i++ {
		messages = append(messages, parsedChatMessage{
			ID:        fmt.Sprintf("user-%03d", i),
			Role:      "user",
			CreatedAt: int64(1000 + i),
			Content:   []any{map[string]any{"type": "text", "text": fmt.Sprintf("request %03d %s", i, largeText)}},
		})
		messages = append(messages, parsedChatMessage{
			ID:        fmt.Sprintf("assistant-%03d", i),
			Role:      "assistant",
			CreatedAt: int64(2000 + i),
			Content:   []any{map[string]any{"type": "text", "text": fmt.Sprintf("response %03d %s", i, largeText)}},
		})
	}

	migrated, err := buildHostPiMigrationJSONL("thread-oversized", "/workspace", "claude", messages)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(migrated), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected migrated lines, got %d", len(lines))
	}

	var compaction map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &compaction); err != nil {
		t.Fatal(err)
	}
	if firstString(compaction, "type") != "compaction" {
		t.Fatalf("expected last entry to be compaction, got %#v", compaction)
	}
	if firstString(compaction, "firstKeptEntryId") == "" {
		t.Fatalf("compaction missing firstKeptEntryId: %#v", compaction)
	}
	if !strings.Contains(firstString(compaction, "summary"), "automatically compacted during migration") {
		t.Fatalf("unexpected compaction summary: %#v", compaction)
	}
	if int(compaction["tokensBefore"].(float64)) <= 0 {
		t.Fatalf("expected positive tokensBefore: %#v", compaction)
	}
}

func assertMigratedPiAssistantMetadata(t *testing.T, migrated string) {
	t.Helper()
	for _, line := range strings.Split(migrated, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Type    string         `json:"type"`
			Message map[string]any `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "message" || firstString(event.Message, "role") != "assistant" {
			continue
		}
		if firstString(event.Message, "api") != "chiridion-legacy-migration" {
			t.Fatalf("assistant message missing migration api: %#v", event.Message)
		}
		if firstString(event.Message, "provider") == "" || firstString(event.Message, "model") == "" {
			t.Fatalf("assistant message missing model identity: %#v", event.Message)
		}
		if firstString(event.Message, "stopReason") == "" {
			t.Fatalf("assistant message missing stop reason: %#v", event.Message)
		}
		usage, _ := asMap(event.Message["usage"])
		if usage == nil {
			t.Fatalf("assistant message missing usage: %#v", event.Message)
		}
		if _, ok := usage["totalTokens"]; !ok {
			t.Fatalf("assistant usage missing totalTokens: %#v", usage)
		}
		cost, _ := asMap(usage["cost"])
		if cost == nil {
			t.Fatalf("assistant usage missing cost: %#v", usage)
		}
		return
	}
	t.Fatal("no assistant message found")
}

func TestHostPiSessionDirHasJSONL(t *testing.T) {
	dir := t.TempDir()
	has, err := hostPiSessionDirHasJSONL(filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("missing dir should not have Pi JSONL")
	}
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	has, err = hostPiSessionDirHasJSONL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("expected session dir to have Pi JSONL")
	}
}

func TestLegacyCodexStatePathCandidatesUsesThreadIDWhenStoredCodexSessionIDMissing(t *testing.T) {
	paths, err := legacyCodexStatePathCandidates("camel-thread", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/home/claude/.codex/threads/camel-thread/state_5.sqlite",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestLegacyCodexStatePathCandidatesIncludesStoredCodexSessionID(t *testing.T) {
	paths, err := legacyCodexStatePathCandidates("camel-thread", "codex-session")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/home/claude/.codex/threads/camel-thread/state_5.sqlite",
		"/home/claude/.codex/threads/codex-session/state_5.sqlite",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestLegacyCodexStatePathCandidatesDeduplicatesStoredCodexSessionID(t *testing.T) {
	paths, err := legacyCodexStatePathCandidates("camel-thread", "camel-thread")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/home/claude/.codex/threads/camel-thread/state_5.sqlite",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestLegacyClaudeSessionCandidatesUsesThreadIDWhenStoredClaudeSessionIDMissing(t *testing.T) {
	sessions, err := legacyClaudeSessionCandidates("camel-thread", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"camel-thread"}
	if !reflect.DeepEqual(sessions, want) {
		t.Fatalf("sessions = %#v, want %#v", sessions, want)
	}
}

func TestLegacyClaudeSessionCandidatesIncludesStoredClaudeSessionID(t *testing.T) {
	sessions, err := legacyClaudeSessionCandidates("camel-thread", "claude-session")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"camel-thread", "claude-session"}
	if !reflect.DeepEqual(sessions, want) {
		t.Fatalf("sessions = %#v, want %#v", sessions, want)
	}
}

func TestLegacyClaudeSessionCandidatesDeduplicatesStoredClaudeSessionID(t *testing.T) {
	sessions, err := legacyClaudeSessionCandidates("camel-thread", "camel-thread")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"camel-thread"}
	if !reflect.DeepEqual(sessions, want) {
		t.Fatalf("sessions = %#v, want %#v", sessions, want)
	}
}

func TestHostPiMigrationExhaustiveFixtureRoot(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("CHIRIDION_PI_MIGRATION_EXHAUSTIVE_ROOT"))
	if root == "" {
		t.Skip("CHIRIDION_PI_MIGRATION_EXHAUSTIVE_ROOT is not set")
	}
	outputRoot := strings.TrimSpace(os.Getenv("CHIRIDION_PI_MIGRATION_EXHAUSTIVE_OUTPUT_ROOT"))

	var stats piMigrationExhaustiveStats
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}

		switch {
		case strings.HasSuffix(path, ".jsonl") && strings.Contains(path, string(filepath.Separator)+".claude"+string(filepath.Separator)+"projects"+string(filepath.Separator)):
			stats.ClaudeFiles++
			threadID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			messages := parseClaudeJSONLMessages(string(content), threadID)
			if len(messages) == 0 {
				stats.EmptyClaudeFiles++
				return nil
			}
			stats.ClaudeMessages += len(messages)
			return validateExhaustivePiMigration(outputRoot, path, threadID, "claude", messages)
		case filepath.Base(path) == "state_5.sqlite":
			stats.CodexFiles++
			threadID := filepath.Base(filepath.Dir(path))
			messages, err := readCodexStateMessages(context.Background(), path, threadID, "")
			if err != nil {
				return fmt.Errorf("read codex state %s: %w", path, err)
			}
			if len(messages) == 0 {
				stats.EmptyCodexFiles++
				return nil
			}
			stats.CodexMessages += len(messages)
			return validateExhaustivePiMigration(outputRoot, path, threadID, "codex", messages)
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ClaudeFiles == 0 && stats.CodexFiles == 0 {
		t.Fatalf("no legacy files found under %s", root)
	}
	t.Logf("validated Claude files=%d empty=%d messages=%d Codex files=%d empty=%d messages=%d", stats.ClaudeFiles, stats.EmptyClaudeFiles, stats.ClaudeMessages, stats.CodexFiles, stats.EmptyCodexFiles, stats.CodexMessages)
}

func TestHostPiMigrationSelectedFixtureFiles(t *testing.T) {
	rawFiles := strings.TrimSpace(os.Getenv("CHIRIDION_PI_MIGRATION_SELECTED_FILES"))
	if rawFiles == "" {
		t.Skip("CHIRIDION_PI_MIGRATION_SELECTED_FILES is not set")
	}
	outputRoot := strings.TrimSpace(os.Getenv("CHIRIDION_PI_MIGRATION_EXHAUSTIVE_OUTPUT_ROOT"))

	var stats piMigrationExhaustiveStats
	for _, rawPath := range strings.Split(rawFiles, "\n") {
		path := strings.TrimSpace(rawPath)
		if path == "" {
			continue
		}
		switch {
		case strings.HasSuffix(path, ".jsonl"):
			stats.ClaudeFiles++
			threadID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			messages := parseClaudeJSONLMessages(string(content), threadID)
			if len(messages) == 0 {
				stats.EmptyClaudeFiles++
				continue
			}
			stats.ClaudeMessages += len(messages)
			if err := validateExhaustivePiMigration(outputRoot, path, threadID, "claude", messages); err != nil {
				t.Fatal(err)
			}
		case filepath.Base(path) == "state_5.sqlite":
			stats.CodexFiles++
			threadID := filepath.Base(filepath.Dir(path))
			messages, err := readCodexStateMessages(context.Background(), path, threadID, "")
			if err != nil {
				t.Fatalf("read codex state %s: %v", path, err)
			}
			if len(messages) == 0 {
				stats.EmptyCodexFiles++
				continue
			}
			stats.CodexMessages += len(messages)
			if err := validateExhaustivePiMigration(outputRoot, path, threadID, "codex", messages); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unsupported selected fixture file: %s", path)
		}
	}

	t.Logf("validated selected Claude files=%d empty=%d messages=%d Codex files=%d empty=%d messages=%d", stats.ClaudeFiles, stats.EmptyClaudeFiles, stats.ClaudeMessages, stats.CodexFiles, stats.EmptyCodexFiles, stats.CodexMessages)
}

type piMigrationExhaustiveStats struct {
	ClaudeFiles      int
	EmptyClaudeFiles int
	ClaudeMessages   int
	CodexFiles       int
	EmptyCodexFiles  int
	CodexMessages    int
}

func validateExhaustivePiMigration(outputRoot, sourcePath, threadID, source string, messages []parsedChatMessage) error {
	migrated, err := buildHostPiMigrationJSONL(threadID, "/home/claude", source, messages)
	if err != nil {
		return err
	}
	if err := validatePiMigrationJSONLines(migrated); err != nil {
		return err
	}
	piMessages := parsePiJSONLMessages(migrated, threadID)
	if len(piMessages) == 0 {
		return fmt.Errorf("migrated Pi session has no readable messages for %s", sourcePath)
	}
	if outputRoot != "" {
		relative, err := filepath.Rel(filepath.VolumeName(sourcePath)+string(filepath.Separator), sourcePath)
		if err != nil || strings.HasPrefix(relative, "..") {
			relative = strings.TrimPrefix(sourcePath, string(filepath.Separator))
		}
		target := filepath.Join(outputRoot, relative+".pi.jsonl")
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(migrated), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func validatePiMigrationJSONLines(content string) error {
	seenIDs := make(map[string]int)
	for index, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return fmt.Errorf("invalid Pi JSONL line %d: %w", index+1, err)
		}
		if firstString(event, "type") == "" {
			return fmt.Errorf("Pi JSONL line %d missing type", index+1)
		}
		id := firstString(event, "id")
		if id == "" {
			continue
		}
		if previousLine := seenIDs[id]; previousLine > 0 {
			return fmt.Errorf("Pi JSONL line %d duplicates entry id %q from line %d", index+1, id, previousLine)
		}
		seenIDs[id] = index + 1
	}
	return nil
}
