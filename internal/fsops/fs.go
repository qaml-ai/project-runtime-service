package fsops

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Entry struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	ModifiedAt   string `json:"modifiedAt"`
	RelativePath string `json:"relativePath,omitempty"`
}

type ListOptions struct {
	Recursive     bool
	IncludeHidden bool
}

type ExistsResult struct {
	Exists      bool   `json:"exists"`
	IsFile      bool   `json:"isFile,omitempty"`
	IsDirectory bool   `json:"isDirectory,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ModifiedAt  string `json:"modifiedAt,omitempty"`
}

type ReadInfo struct {
	HostPath string
	Size     int64
}

type Manager struct {
	workspacesRoot     string
	workspaceMountPath string
	fileOwnerUID       int
	fileOwnerGID       int
}

func NewManager(workspacesRoot string) *Manager {
	if workspacesRoot == "" {
		workspacesRoot = defaultWorkspaceRoot()
	}
	return &Manager{
		workspacesRoot:     workspacesRoot,
		workspaceMountPath: cleanMountPath(envStringAny([]string{"PROJECT_RUNTIME_WORKSPACE_MOUNT", "CONTAINER_WORKSPACE_MOUNT"}, "/home/claude")),
		fileOwnerUID:       envIntAny([]string{"PROJECT_RUNTIME_FILE_OWNER_UID", "CONTAINER_UID"}, 1001),
		fileOwnerGID:       envIntAny([]string{"PROJECT_RUNTIME_FILE_OWNER_GID", "CONTAINER_GID"}, 1001),
	}
}

func (m *Manager) ResolveHostPath(name, sandboxPath string) (string, error) {
	if name == "" {
		return "", errors.New("workspace name required")
	}

	wsRoot := filepath.Clean(filepath.Join(m.workspacesRoot, name))
	cleaned := sandboxPath

	if strings.HasPrefix(cleaned, m.workspaceMountPath+"/") {
		cleaned = strings.TrimPrefix(cleaned, m.workspaceMountPath)
	} else if cleaned == m.workspaceMountPath {
		cleaned = "/"
	}

	candidate := cleaned
	if strings.HasPrefix(candidate, "/") {
		candidate = strings.TrimPrefix(candidate, "/")
	}

	resolved := filepath.Clean(filepath.Join(wsRoot, candidate))
	rel, err := filepath.Rel(wsRoot, resolved)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal detected: %s", sandboxPath)
	}

	wsRootWithSep := wsRoot + string(filepath.Separator)
	if resolved != wsRoot && !strings.HasPrefix(resolved, wsRootWithSep) {
		return "", fmt.Errorf("path traversal detected: %s", sandboxPath)
	}

	return resolved, nil
}

func (m *Manager) ReadInfo(name, path string) (ReadInfo, error) {
	hostPath, err := m.ResolveHostPath(name, path)
	if err != nil {
		return ReadInfo{}, err
	}
	stat, err := os.Stat(hostPath)
	if err != nil {
		return ReadInfo{}, err
	}
	return ReadInfo{HostPath: hostPath, Size: stat.Size()}, nil
}

func (m *Manager) Write(name, path string, data []byte) error {
	hostPath, err := m.ResolveHostPath(name, path)
	if err != nil {
		return err
	}
	parentDir := filepath.Dir(hostPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return err
	}
	m.chownWorkspaceFile(parentDir)
	if err := os.WriteFile(hostPath, data, 0o644); err != nil {
		return err
	}
	m.chownWorkspaceFile(hostPath)
	return nil
}

func (m *Manager) List(name, path string, options ListOptions) ([]Entry, error) {
	hostPath, err := m.ResolveHostPath(name, path)
	if err != nil {
		return nil, err
	}

	includeHidden := options.IncludeHidden
	if !options.Recursive {
		dirs, err := os.ReadDir(hostPath)
		if err != nil {
			return nil, err
		}

		entries := make([]Entry, 0, len(dirs))
		for _, de := range dirs {
			if !includeHidden && strings.HasPrefix(de.Name(), ".") {
				continue
			}

			full := filepath.Join(hostPath, de.Name())
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			typeValue := "file"
			if info.IsDir() {
				typeValue = "directory"
			}
			entries = append(entries, Entry{
				Name:         de.Name(),
				Type:         typeValue,
				Size:         info.Size(),
				ModifiedAt:   info.ModTime().UTC().Format(time.RFC3339Nano),
				RelativePath: filepath.ToSlash(de.Name()),
			})
		}

		return entries, nil
	}

	entries := make([]Entry, 0, 64)
	walkErr := filepath.WalkDir(hostPath, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Best-effort traversal: skip unreadable entries.
			return nil
		}
		if current == hostPath {
			return nil
		}

		relPath, err := filepath.Rel(hostPath, current)
		if err != nil {
			return nil
		}
		if relPath == "." || relPath == "" {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		name := d.Name()
		if !includeHidden && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		typeValue := "file"
		if info.IsDir() {
			typeValue = "directory"
		}

		entries = append(entries, Entry{
			Name:         name,
			Type:         typeValue,
			Size:         info.Size(),
			ModifiedAt:   info.ModTime().UTC().Format(time.RFC3339Nano),
			RelativePath: relPath,
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	return entries, nil
}

func (m *Manager) Delete(name, path string, recursive bool) error {
	hostPath, err := m.ResolveHostPath(name, path)
	if err != nil {
		return err
	}

	if recursive {
		if removeErr := os.RemoveAll(hostPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
		return nil
	}
	if removeErr := os.Remove(hostPath); removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return nil
}

func (m *Manager) Move(name, source, dest string) error {
	srcPath, err := m.ResolveHostPath(name, source)
	if err != nil {
		return err
	}
	dstPath, err := m.ResolveHostPath(name, dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	m.chownWorkspaceFile(filepath.Dir(dstPath))
	return os.Rename(srcPath, dstPath)
}

func (m *Manager) Mkdir(name, path string) error {
	hostPath, err := m.ResolveHostPath(name, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		return err
	}
	m.chownWorkspaceFile(hostPath)
	return nil
}

func (m *Manager) chownWorkspaceFile(path string) {
	if m.fileOwnerUID < 0 && m.fileOwnerGID < 0 {
		return
	}
	_ = os.Chown(path, m.fileOwnerUID, m.fileOwnerGID)
}

func (m *Manager) Exists(name, path string) (ExistsResult, error) {
	hostPath, err := m.ResolveHostPath(name, path)
	if err != nil {
		return ExistsResult{}, err
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ExistsResult{Exists: false}, nil
		}
		return ExistsResult{}, err
	}
	return ExistsResult{
		Exists:      true,
		IsFile:      info.Mode().IsRegular(),
		IsDirectory: info.IsDir(),
		Size:        info.Size(),
		ModifiedAt:  info.ModTime().UTC().Format(time.RFC3339Nano),
	}, nil
}

func StreamFile(path string, w io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(w, file)
	return err
}

func defaultWorkspaceRoot() string {
	if runtime.GOOS == "linux" {
		return "/srv/sandboxes"
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".sandbox-host/workspaces"
	}
	return filepath.Join(wd, ".sandbox-host", "workspaces")
}

func cleanMountPath(path string) string {
	cleaned := filepath.ToSlash(filepath.Clean("/" + strings.TrimSpace(path)))
	if cleaned == "." || cleaned == "/" {
		return "/"
	}
	return cleaned
}

func envStringAny(keys []string, fallback string) string {
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			if strings.TrimSpace(value) != "" {
				return value
			}
		}
	}
	return fallback
}

func envIntAny(keys []string, fallback int) int {
	for _, key := range keys {
		raw, ok := os.LookupEnv(key)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		if err == nil {
			return parsed
		}
	}
	return fallback
}
