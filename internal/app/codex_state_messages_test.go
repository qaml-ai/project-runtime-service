package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestParseCodexRolloutMessages(t *testing.T) {
	const raw = `{"timestamp":"2026-04-14T18:53:31.464Z","type":"event_msg","payload":{"type":"user_message","message":"hello rollout"}}
{"timestamp":"2026-04-14T18:53:32.000Z","type":"response_item","payload":{"type":"reasoning","id":"rs_1","summary":["summary"],"content":[{"type":"text","text":"thinking"}]}}
{"timestamp":"2026-04-14T18:53:33.000Z","type":"response_item","payload":{"type":"function_call","name":"read","arguments":"{\"file\":\"README.md\"}","call_id":"call_1"}}
{"timestamp":"2026-04-14T18:53:34.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"contents"}}
{"timestamp":"2026-04-14T18:53:35.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"done"}]}}
{"timestamp":"2026-04-14T18:53:36.000Z","type":"event_msg","payload":{"type":"task_complete"}}`

	messages := parseCodexRolloutMessages(raw, "camel-thread")
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %#v", len(messages), messages)
	}
	if messages[0].Role != "user" || messages[0].Content != "hello rollout" {
		t.Fatalf("unexpected user message: %#v", messages[0])
	}
	if messages[1].Role != "assistant" {
		t.Fatalf("unexpected assistant message: %#v", messages[1])
	}
	blocks, ok := asSlice(messages[1].Content)
	if !ok || len(blocks) != 4 {
		t.Fatalf("unexpected assistant blocks: %#v", messages[1].Content)
	}
	toolUse, _ := asMap(blocks[1])
	if firstString(toolUse, "type") != "tool_use" || firstString(toolUse, "name") != "read" {
		t.Fatalf("unexpected tool use block: %#v", toolUse)
	}
}

func TestReadCodexStateMessagesMapsCopiedRolloutPath(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".codex", "threads", "camel-thread")
	sessionDir := filepath.Join(stateDir, "sessions", "2026", "04", "14")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}

	rolloutPath := filepath.Join(sessionDir, "rollout.jsonl")
	if err := os.WriteFile(rolloutPath, []byte(`{"timestamp":"2026-04-14T18:53:31.464Z","type":"event_msg","payload":{"type":"user_message","message":"hello"}}
{"timestamp":"2026-04-14T18:53:35.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"done"}]}}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(stateDir, "state_5.sqlite")
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY,
		rollout_path TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	remoteRolloutPath := "/srv/sandboxes/chiridion-ws-example/.codex/threads/camel-thread/sessions/2026/04/14/rollout.jsonl"
	if _, err := db.Exec(`INSERT INTO threads (id, rollout_path, created_at, updated_at) VALUES (?, ?, ?, ?)`, "codex-thread", remoteRolloutPath, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	messages, err := readCodexStateMessages(context.Background(), statePath, "camel-thread", "codex-thread")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %#v", len(messages), messages)
	}
}

func TestReadCodexStateMessagesReadsWalBackedState(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".codex", "threads", "camel-thread")
	sessionDir := filepath.Join(stateDir, "sessions", "2026", "04", "14")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}

	rolloutPath := filepath.Join(sessionDir, "rollout.jsonl")
	if err := os.WriteFile(rolloutPath, []byte(`{"timestamp":"2026-04-14T18:53:31.464Z","type":"event_msg","payload":{"type":"user_message","message":"hello wal"}}
{"timestamp":"2026-04-14T18:53:35.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"done"}]}}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(stateDir, "state_5.sqlite")
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY,
		rollout_path TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO threads (id, rollout_path, created_at, updated_at) VALUES (?, ?, ?, ?)`, "codex-thread", rolloutPath, 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath + "-wal"); err != nil {
		t.Fatalf("expected WAL file: %v", err)
	}

	messages, err := readCodexStateMessages(context.Background(), statePath, "camel-thread", "codex-thread")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %#v", len(messages), messages)
	}
}
