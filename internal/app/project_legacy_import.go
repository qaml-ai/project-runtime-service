package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type legacyImportResult struct {
	Success      bool     `json:"success"`
	Files        int64    `json:"files"`
	Bytes        int64    `json:"bytes"`
	SkippedPaths []string `json:"skippedPaths"`
}

func (s *Server) handleProjectLegacyImport(w http.ResponseWriter, req *http.Request, route ProjectRoute) error {
	var payload struct {
		OrgID       string   `json:"orgId"`
		WorkspaceID string   `json:"workspaceId"`
		SourcePaths []string `json:"sourcePaths"`
		IgnoreGlobs []string `json:"ignoreGlobs"`
	}
	if err := decodeJSON(req, &payload); err != nil {
		errorJSON(w, "invalid JSON body", http.StatusBadRequest)
		return nil
	}
	orgID := strings.TrimSpace(payload.OrgID)
	workspaceID := strings.TrimSpace(payload.WorkspaceID)
	if orgID == "" || workspaceID == "" {
		errorJSON(w, "orgId and workspaceId required", http.StatusBadRequest)
		return nil
	}
	if _, ok := s.migrationLocks.get(orgID + "/" + workspaceID); !ok {
		errorJSON(w, "legacy workspace must be locked before import", http.StatusPreconditionFailed)
		return nil
	}
	if len(payload.SourcePaths) == 0 {
		errorJSON(w, "sourcePaths required", http.StatusBadRequest)
		return nil
	}

	sourceRoot, err := s.legacyImportSourceRoot(workspaceID)
	if err != nil {
		return err
	}
	targetRoot, err := s.workspaces.Ensure(route.Name)
	if err != nil {
		return err
	}

	result := legacyImportResult{Success: true}
	singleSource := len(payload.SourcePaths) == 1
	for _, sourcePath := range payload.SourcePaths {
		sourceHostPath, err := legacyWorkspaceHostPath(sourceRoot, sourcePath)
		if err != nil {
			return err
		}
		info, err := os.Lstat(sourceHostPath)
		if err != nil {
			return fmt.Errorf("stat legacy import source %s: %w", sourcePath, err)
		}
		destination := targetRoot
		if !singleSource || !info.IsDir() {
			destination = filepath.Join(targetRoot, filepath.Base(sourceHostPath))
		}
		stats, err := copyLegacyImportPath(sourceHostPath, destination, payload.IgnoreGlobs)
		if err != nil {
			return err
		}
		result.Files += stats.Files
		result.Bytes += stats.Bytes
		result.SkippedPaths = append(result.SkippedPaths, stats.SkippedPaths...)
	}
	if err := reconcileImportedGitRepository(targetRoot); err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, result)
	return nil
}

func (s *Server) legacyImportSourceRoot(workspaceID string) (string, error) {
	if strings.TrimSpace(s.cfg.LegacyWorkspacesRoot) == "" {
		return s.workspaces.Ensure(sandboxName(workspaceID))
	}
	if strings.TrimSpace(workspaceID) == "" {
		return "", errors.New("workspace id required")
	}
	cleanWorkspaceID := filepath.Clean(workspaceID)
	if cleanWorkspaceID == "." || cleanWorkspaceID == ".." || strings.HasPrefix(cleanWorkspaceID, "../") || filepath.IsAbs(cleanWorkspaceID) {
		return "", fmt.Errorf("invalid workspace id: %s", workspaceID)
	}
	name := s.cfg.LegacyWorkspacePrefix + cleanWorkspaceID
	root := filepath.Join(s.cfg.LegacyWorkspacesRoot, name)
	base, err := filepath.Abs(s.cfg.LegacyWorkspacesRoot)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved != base && !strings.HasPrefix(resolved, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid legacy workspace path: %s", workspaceID)
	}
	return resolved, nil
}

type legacyImportCopyStats struct {
	Files        int64
	Bytes        int64
	SkippedPaths []string
}

func legacyWorkspaceHostPath(root, sandboxPath string) (string, error) {
	path := strings.TrimSpace(sandboxPath)
	if path == "" {
		return "", errors.New("source path required")
	}
	path = strings.TrimPrefix(path, "/home/claude")
	path = strings.TrimPrefix(path, "/")
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return root, nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("invalid source path: %s", sandboxPath)
	}
	return filepath.Join(root, cleaned), nil
}

func copyLegacyImportPath(source, destination string, ignoreGlobs []string) (legacyImportCopyStats, error) {
	var stats legacyImportCopyStats
	info, err := os.Lstat(source)
	if err != nil {
		return stats, err
	}
	if !info.IsDir() {
		if err := copyLegacyImportFile(source, destination, info, &stats); err != nil {
			return stats, err
		}
		return stats, nil
	}
	if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
		return stats, err
	}
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if shouldSkipLegacyImportPath(rel, ignoreGlobs) {
			stats.SkippedPaths = append(stats.SkippedPaths, rel)
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		return copyLegacyImportFile(path, target, info, &stats)
	})
	return stats, err
}

func copyLegacyImportFile(source, destination string, info os.FileInfo, stats *legacyImportCopyStats) error {
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		_ = os.Remove(destination)
		if err := os.Symlink(target, destination); err != nil {
			return err
		}
		stats.Files++
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	stats.Files++
	stats.Bytes += written
	return nil
}

func shouldSkipLegacyImportPath(rel string, ignoreGlobs []string) bool {
	normalized := filepath.ToSlash(rel)
	for _, rawPattern := range ignoreGlobs {
		pattern := strings.TrimSpace(filepath.ToSlash(rawPattern))
		if pattern == "" {
			continue
		}
		if pattern == ".*" || pattern == ".*/**" {
			for _, part := range strings.Split(normalized, "/") {
				if strings.HasPrefix(part, ".") {
					return true
				}
			}
			continue
		}
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "/**")
			if normalized == prefix || strings.HasPrefix(normalized, prefix+"/") {
				return true
			}
		}
		if ok, _ := filepath.Match(pattern, normalized); ok {
			return true
		}
	}
	return false
}

func reconcileImportedGitRepository(projectRoot string) error {
	gitDir := filepath.Join(projectRoot, ".git")
	info, err := os.Lstat(gitDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	hasCommits := gitCommand(projectRoot, "rev-parse", "--verify", "HEAD") == nil
	hasRemotes := gitCommand(projectRoot, "remote") == nil && strings.TrimSpace(gitCommandOutput(projectRoot, "remote")) != ""
	if !hasCommits || !hasRemotes {
		return os.RemoveAll(gitDir)
	}
	return nil
}

func gitCommand(projectRoot string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = projectRoot
	return cmd.Run()
}

func gitCommandOutput(projectRoot string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = projectRoot
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(output)
}
