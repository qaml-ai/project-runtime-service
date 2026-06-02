package fsops

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResolveHostPathMapsHomePrefix(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root)

	got, err := mgr.ResolveHostPath("sandbox-a", "/home/claude/src/main.ts")
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}

	want := filepath.Join(root, "sandbox-a", "src", "main.ts")
	if got != want {
		t.Fatalf("unexpected host path: got %q want %q", got, want)
	}
}

func TestResolveHostPathUsesConfiguredWorkspaceMount(t *testing.T) {
	t.Setenv("PROJECT_RUNTIME_WORKSPACE_MOUNT", "/workspace")
	root := t.TempDir()
	mgr := NewManager(root)

	got, err := mgr.ResolveHostPath("sandbox-a", "/workspace/src/main.ts")
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}

	want := filepath.Join(root, "sandbox-a", "src", "main.ts")
	if got != want {
		t.Fatalf("unexpected host path: got %q want %q", got, want)
	}
}

func TestResolveHostPathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root)

	_, err := mgr.ResolveHostPath("sandbox-a", "../../etc/passwd")
	if err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestWriteAndExists(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root)

	if err := mgr.Write("sandbox-a", "/notes.txt", []byte("hello")); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	result, err := mgr.Exists("sandbox-a", "/notes.txt")
	if err != nil {
		t.Fatalf("exists failed: %v", err)
	}
	if !result.Exists || !result.IsFile {
		t.Fatalf("unexpected exists result: %+v", result)
	}

	contents, err := os.ReadFile(filepath.Join(root, "sandbox-a", "notes.txt"))
	if err != nil {
		t.Fatalf("failed reading file directly: %v", err)
	}
	if string(contents) != "hello" {
		t.Fatalf("unexpected file contents: %q", string(contents))
	}
}

func TestListRecursiveIncludesRelativePaths(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root)
	sandbox := "sandbox-a"

	if err := mgr.Write(sandbox, "/src/main.ts", []byte("console.log('hi')")); err != nil {
		t.Fatalf("write src/main.ts failed: %v", err)
	}
	if err := mgr.Write(sandbox, "/README.md", []byte("# readme")); err != nil {
		t.Fatalf("write README.md failed: %v", err)
	}
	if err := mgr.Write(sandbox, "/.claude/projects/thread.jsonl", []byte("{}")); err != nil {
		t.Fatalf("write .claude/projects/thread.jsonl failed: %v", err)
	}

	entries, err := mgr.List(sandbox, "/", ListOptions{
		Recursive:     true,
		IncludeHidden: true,
	})
	if err != nil {
		t.Fatalf("list recursive failed: %v", err)
	}

	relativePaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		relativePaths = append(relativePaths, entry.RelativePath)
	}

	if !slices.Contains(relativePaths, "src/main.ts") {
		t.Fatalf("missing src/main.ts in recursive list: %+v", relativePaths)
	}
	if !slices.Contains(relativePaths, "README.md") {
		t.Fatalf("missing README.md in recursive list: %+v", relativePaths)
	}
	if !slices.Contains(relativePaths, ".claude/projects/thread.jsonl") {
		t.Fatalf("missing hidden file in recursive list: %+v", relativePaths)
	}
}

func TestListRecursiveExcludeHidden(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root)
	sandbox := "sandbox-a"

	if err := mgr.Write(sandbox, "/visible/index.ts", []byte("export {}")); err != nil {
		t.Fatalf("write visible/index.ts failed: %v", err)
	}
	if err := mgr.Write(sandbox, "/.env", []byte("SECRET=1")); err != nil {
		t.Fatalf("write .env failed: %v", err)
	}
	if err := mgr.Write(sandbox, "/.claude/projects/thread.jsonl", []byte("{}")); err != nil {
		t.Fatalf("write .claude/projects/thread.jsonl failed: %v", err)
	}

	entries, err := mgr.List(sandbox, "/", ListOptions{
		Recursive:     true,
		IncludeHidden: false,
	})
	if err != nil {
		t.Fatalf("list recursive failed: %v", err)
	}

	relativePaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		relativePaths = append(relativePaths, entry.RelativePath)
	}

	if !slices.Contains(relativePaths, "visible/index.ts") {
		t.Fatalf("missing visible/index.ts in recursive list: %+v", relativePaths)
	}
	for _, rel := range relativePaths {
		if strings.HasPrefix(rel, ".") || strings.Contains(rel, "/.") {
			t.Fatalf("found hidden entry in filtered list: %q", rel)
		}
	}
}
