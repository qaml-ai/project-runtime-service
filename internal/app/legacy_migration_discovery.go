package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qaml-ai/project-runtime-service/internal/fsops"
)

const legacyMigrationDiscoveryTimeout = 20 * time.Second

type legacyMigrationDiscoveryResult struct {
	Files            []fsops.Entry `json:"files"`
	Count            int           `json:"count"`
	NestedWorkerApps []string      `json:"nestedWorkerApps"`
	Timestamp        string        `json:"timestamp"`
}

func (s *Server) handleWorkspaceMigrationDiscovery(w http.ResponseWriter, _ *http.Request, route WorkspaceRoute) error {
	root, err := s.legacyImportSourceRoot(route.WorkspaceID)
	if err != nil {
		return err
	}
	entries, err := legacyMigrationTopLevelEntries(root)
	if err != nil {
		return err
	}
	workerApps, err := legacyMigrationFindWorkerApps(root)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, legacyMigrationDiscoveryResult{
		Files:            entries,
		Count:            len(entries),
		NestedWorkerApps: workerApps,
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
	})
	return nil
}

func legacyMigrationTopLevelEntries(root string) ([]fsops.Entry, error) {
	dirEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	entries := make([]fsops.Entry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		entryType := "file"
		if info.IsDir() {
			entryType = "directory"
		}
		entries = append(entries, fsops.Entry{
			Name:         entry.Name(),
			Type:         entryType,
			Size:         info.Size(),
			ModifiedAt:   info.ModTime().UTC().Format(time.RFC3339Nano),
			RelativePath: entry.Name(),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

func legacyMigrationFindWorkerApps(root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), legacyMigrationDiscoveryTimeout)
	defer cancel()

	args := []string{
		root,
		"-maxdepth", "5",
		"(",
		"-name", ".git",
		"-o", "-name", ".wrangler",
		"-o", "-name", "node_modules",
		"-o", "-name", "build",
		"-o", "-name", "dist",
		")",
		"-prune",
		"-o",
		"-type", "f",
		"(",
		"-name", "wrangler.toml",
		"-o", "-name", "wrangler.json",
		"-o", "-name", "wrangler.jsonc",
		")",
		"-print",
	}
	cmd := exec.CommandContext(ctx, "find", args...)
	output, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("legacy migration discovery timed out after %s", legacyMigrationDiscoveryTimeout)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("find worker apps failed: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	apps := make(map[string]struct{})
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		dir := filepath.Dir(line)
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
			continue
		}
		apps["/home/claude/"+filepath.ToSlash(rel)] = struct{}{}
	}
	result := make([]string, 0, len(apps))
	for app := range apps {
		result = append(result, app)
	}
	sort.Strings(result)
	return result, nil
}
