package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairHostPiSessionDirRepairsGrokMissingCostError(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "thread-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(sessionDir, "session.jsonl")
	content := strings.Join([]string{
		`{"type":"session","version":3,"id":"s1","timestamp":"2026-05-01T00:00:00.000Z","cwd":"/workspace"}`,
		`{"type":"message","id":"a1","parentId":null,"timestamp":"2026-05-01T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"api":"openai-responses","provider":"camelai-openrouter","model":"x-ai/grok-4.3","stopReason":"error","errorMessage":"Cannot read properties of undefined (reading 'input')"}}`,
		`{"type":"message","id":"u1","parentId":"a1","timestamp":"2026-05-01T00:00:02.000Z","message":{"role":"user","content":[{"type":"text","text":"next"}]}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	repaired, err := repairHostPiSessionDir(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 1 {
		t.Fatalf("repairHostPiSessionDir() repaired %d, want 1", repaired)
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), `"stopReason":"error"`) {
		t.Fatalf("expected error stopReason to be repaired:\n%s", updated)
	}
	if strings.Contains(string(updated), "errorMessage") {
		t.Fatalf("expected errorMessage to be removed:\n%s", updated)
	}
	if !strings.Contains(string(updated), `"stopReason":"stop"`) {
		t.Fatalf("expected stop stopReason:\n%s", updated)
	}
}

func TestRepairHostPiSessionDirIgnoresOtherErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := `{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"partial"}],"api":"openai-responses","model":"x-ai/grok-4.3","stopReason":"error","errorMessage":"upstream failed"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	repaired, err := repairHostPiSessionDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 0 {
		t.Fatalf("repairHostPiSessionDir() repaired %d, want 0", repaired)
	}
}
