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
	"sync"
	"time"
)

const legacyImportCopyConcurrency = 32

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
		GitRemote   string   `json:"gitRemote"`
		GitBranch   string   `json:"gitBranch"`
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
	if err := clearLegacyImportTarget(targetRoot); err != nil {
		return fmt.Errorf("clear legacy import target: %w", err)
	}
	stagingRoot, err := createLegacyImportStagingRoot(targetRoot)
	if err != nil {
		return err
	}

	result := legacyImportResult{Success: true}
	singleSource := len(payload.SourcePaths) == 1
	var tasks []legacyImportCopyTask
	for _, sourcePath := range payload.SourcePaths {
		sourceHostPath, err := legacyWorkspaceHostPath(sourceRoot, sourcePath)
		if err != nil {
			return err
		}
		info, err := os.Lstat(sourceHostPath)
		if err != nil {
			return fmt.Errorf("stat legacy import source %s: %w", sourcePath, err)
		}
		destination := stagingRoot
		if !singleSource || !info.IsDir() {
			destination = filepath.Join(stagingRoot, filepath.Base(sourceHostPath))
		}
		stats, sourceTasks, err := planLegacyImportPath(sourceHostPath, destination, payload.IgnoreGlobs)
		if err != nil {
			return err
		}
		result.SkippedPaths = append(result.SkippedPaths, stats.SkippedPaths...)
		tasks = append(tasks, sourceTasks...)
	}
	copyStats, err := copyLegacyImportTasks(tasks)
	if err != nil {
		return err
	}
	result.Files += copyStats.Files
	result.Bytes += copyStats.Bytes
	if err := reconcileImportedGitRepository(stagingRoot); err != nil {
		return err
	}
	if err := configureImportedGitRepository(stagingRoot, payload.GitRemote, payload.GitBranch); err != nil {
		return err
	}
	if err := publishLegacyImportStagingRoot(targetRoot, stagingRoot); err != nil {
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

type legacyImportCopyTask struct {
	Source      string
	Destination string
	Info        os.FileInfo
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

func clearLegacyImportTarget(targetRoot string) error {
	entries, err := os.ReadDir(targetRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(targetRoot, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func createLegacyImportStagingRoot(targetRoot string) (string, error) {
	stagingRoot := filepath.Join(targetRoot, fmt.Sprintf(".legacy-import-staging-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return "", err
	}
	return stagingRoot, nil
}

func publishLegacyImportStagingRoot(targetRoot, stagingRoot string) error {
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(stagingRoot, entry.Name()), filepath.Join(targetRoot, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(stagingRoot)
}

func planLegacyImportPath(source, destination string, ignoreGlobs []string) (legacyImportCopyStats, []legacyImportCopyTask, error) {
	var stats legacyImportCopyStats
	var tasks []legacyImportCopyTask
	info, err := os.Lstat(source)
	if err != nil {
		return stats, nil, err
	}
	if !info.IsDir() {
		return stats, []legacyImportCopyTask{{Source: source, Destination: destination, Info: info}}, nil
	}
	if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
		return stats, nil, err
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
		tasks = append(tasks, legacyImportCopyTask{Source: path, Destination: target, Info: info})
		return nil
	})
	return stats, tasks, err
}

func copyLegacyImportTasks(tasks []legacyImportCopyTask) (legacyImportCopyStats, error) {
	var stats legacyImportCopyStats
	if len(tasks) == 0 {
		return stats, nil
	}

	workerCount := legacyImportCopyConcurrency
	if len(tasks) < workerCount {
		workerCount = len(tasks)
	}
	taskCh := make(chan legacyImportCopyTask)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	setErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	hasErr := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskCh {
				if hasErr() {
					continue
				}
				files, bytes, err := copyLegacyImportFile(task.Source, task.Destination, task.Info)
				if err != nil {
					setErr(err)
					continue
				}
				mu.Lock()
				stats.Files += files
				stats.Bytes += bytes
				mu.Unlock()
			}
		}()
	}

	for _, task := range tasks {
		taskCh <- task
	}
	close(taskCh)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return stats, firstErr
	}
	return stats, nil
}

func copyLegacyImportFile(source, destination string, info os.FileInfo) (int64, int64, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return 0, 0, err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return 0, 0, err
		}
		_ = os.Remove(destination)
		if err := os.Symlink(target, destination); err != nil {
			return 0, 0, err
		}
		return 1, 0, nil
	}
	if !info.Mode().IsRegular() {
		return 0, 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return 0, 0, err
	}
	input, err := os.Open(source)
	if err != nil {
		return 0, 0, err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return 0, 0, err
	}
	written, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return 0, 0, copyErr
	}
	if closeErr != nil {
		return 0, 0, closeErr
	}
	return 1, written, nil
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

func configureImportedGitRepository(projectRoot, remote, branch string) error {
	if err := ensureImportedGitignore(projectRoot); err != nil {
		return err
	}
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return nil
	}
	if err := ensureGitRepository(projectRoot, branch); err != nil {
		return err
	}
	if err := gitCommand(projectRoot, "config", "user.name", "camelAI Agent"); err != nil {
		return err
	}
	if err := gitCommand(projectRoot, "config", "user.email", "agent@camelai.dev"); err != nil {
		return err
	}
	_ = gitCommand(projectRoot, "config", "--global", "--add", "safe.directory", projectRoot)
	if gitCommand(projectRoot, "remote", "get-url", "origin") == nil {
		return gitCommand(projectRoot, "remote", "set-url", "origin", remote)
	}
	return gitCommand(projectRoot, "remote", "add", "origin", remote)
}

func ensureGitRepository(projectRoot, branch string) error {
	if info, err := os.Stat(filepath.Join(projectRoot, ".git")); err == nil && info.IsDir() {
		return nil
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch = "main"
	}
	if err := gitCommand(projectRoot, "init", "-b", branch); err == nil {
		return nil
	}
	return gitCommand(projectRoot, "init")
}

func ensureImportedGitignore(projectRoot string) error {
	path := filepath.Join(projectRoot, ".gitignore")
	existingBytes, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	existing := string(existingBytes)
	lines := map[string]bool{}
	for _, line := range strings.Split(existing, "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	var additions []string
	for _, pattern := range defaultImportedGitignorePatterns {
		if !lines[pattern] {
			additions = append(additions, pattern)
		}
	}
	if len(additions) == 0 {
		return nil
	}
	var builder strings.Builder
	builder.WriteString(existing)
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		builder.WriteString("\n")
	}
	for _, pattern := range additions {
		builder.WriteString(pattern)
		builder.WriteString("\n")
	}
	return os.WriteFile(path, []byte(builder.String()), 0o600)
}

var defaultImportedGitignorePatterns = []string{
	"node_modules/",
	".bun/install/cache/",
	".EasyOCR/",
	".cache/",
	".ipynb_checkpoints/",
	".wrangler/",
	".dev.vars",
	"*.log",
	"dist/",
	"build/",
	"coverage/",
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
