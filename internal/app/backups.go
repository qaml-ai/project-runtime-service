package app

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type backupInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"createdAt"`
	ProjectID string `json:"projectId"`
	Runtime   string `json:"runtime"`
}

func (s *Server) handleProjectBackupsList(w http.ResponseWriter, _ *http.Request, route ProjectRoute) error {
	backups, err := s.listBackups(route)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"projectId": route.ID, "backups": backups})
	return nil
}

func (s *Server) handleProjectBackupCreate(w http.ResponseWriter, _ *http.Request, route ProjectRoute) error {
	if s.rejectInsufficientHeadroom(w) {
		return nil
	}
	started := time.Now()
	success, err := s.containers.TerminateContainer(route.Name, "project_backup_quiesce")
	if err != nil {
		return err
	}
	info, err := s.createBackup(route)
	if err != nil {
		return err
	}
	if err := s.pruneBackups(route); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"sourceStopped": success,
		"backup":        info,
		"durationMs":    time.Since(started).Milliseconds(),
	})
	return nil
}

func (s *Server) handleProjectRestore(w http.ResponseWriter, req *http.Request, route ProjectRoute) error {
	var payload struct {
		BackupName string `json:"backupName"`
	}
	if err := decodeJSON(req, &payload); err != nil {
		errorJSON(w, "invalid JSON body", http.StatusBadRequest)
		return nil
	}
	backupName := strings.TrimSpace(payload.BackupName)
	if backupName == "" {
		errorJSON(w, "backupName required", http.StatusBadRequest)
		return nil
	}
	backupPath, err := s.resolveBackupPath(route, backupName)
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadRequest)
		return nil
	}

	started := time.Now()
	if _, err := s.containers.TerminateContainer(route.Name, "project_restore_quiesce"); err != nil {
		return err
	}
	if err := s.restoreBackup(route, backupPath); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"projectId":  route.ID,
		"backupName": backupName,
		"durationMs": time.Since(started).Milliseconds(),
	})
	return nil
}

func (s *Server) createBackup(route ProjectRoute) (backupInfo, error) {
	sourceDir := filepath.Join(s.workspaces.Root(), route.Name)
	if stat, err := os.Stat(sourceDir); err != nil {
		return backupInfo{}, fmt.Errorf("source project unavailable: %w", err)
	} else if !stat.IsDir() {
		return backupInfo{}, errors.New("source project is not a directory")
	}

	projectBackupDir := s.projectBackupDir(route)
	if err := os.MkdirAll(projectBackupDir, 0o700); err != nil {
		return backupInfo{}, err
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + ".tar.gz"
	path := filepath.Join(projectBackupDir, name)
	tmpPath := path + ".tmp"
	if err := writeTarGz(sourceDir, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return backupInfo{}, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return backupInfo{}, err
	}
	stat, err := os.Stat(path)
	if err != nil {
		return backupInfo{}, err
	}
	return backupInfo{
		Name:      name,
		Path:      path,
		Size:      stat.Size(),
		CreatedAt: stat.ModTime().UTC().Format(time.RFC3339Nano),
		ProjectID: route.ID,
		Runtime:   route.Name,
	}, nil
}

func (s *Server) restoreBackup(route ProjectRoute, backupPath string) error {
	root := s.workspaces.Root()
	targetDir := filepath.Join(root, route.Name)
	extractDir := filepath.Join(root, "."+route.Name+".restore-"+fmt.Sprint(time.Now().UnixNano()))
	rollbackDir := filepath.Join(root, "."+route.Name+".rollback-"+fmt.Sprint(time.Now().UnixNano()))

	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return err
	}
	defer func() {
		_ = os.RemoveAll(extractDir)
	}()
	if err := extractTarGz(backupPath, extractDir); err != nil {
		return fmt.Errorf("restore extraction failed before replacing current project: %w", err)
	}

	targetExists := false
	if _, err := os.Stat(targetDir); err == nil {
		targetExists = true
		if err := os.Rename(targetDir, rollbackDir); err != nil {
			return fmt.Errorf("move current project aside: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat current project: %w", err)
	}

	if err := os.Rename(extractDir, targetDir); err != nil {
		if targetExists {
			_ = os.Rename(rollbackDir, targetDir)
		}
		return fmt.Errorf("publish restored project: %w", err)
	}
	if targetExists {
		_ = os.RemoveAll(rollbackDir)
	}
	return nil
}

func (s *Server) listBackups(route ProjectRoute) ([]backupInfo, error) {
	projectBackupDir := s.projectBackupDir(route)
	entries, err := os.ReadDir(projectBackupDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []backupInfo{}, nil
		}
		return nil, err
	}
	backups := make([]backupInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tar.gz") {
			continue
		}
		stat, err := entry.Info()
		if err != nil {
			continue
		}
		backups = append(backups, backupInfo{
			Name:      entry.Name(),
			Path:      filepath.Join(projectBackupDir, entry.Name()),
			Size:      stat.Size(),
			CreatedAt: stat.ModTime().UTC().Format(time.RFC3339Nano),
			ProjectID: route.ID,
			Runtime:   route.Name,
		})
	}
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt > backups[j].CreatedAt
	})
	return backups, nil
}

func (s *Server) pruneBackups(route ProjectRoute) error {
	backups, err := s.listBackups(route)
	if err != nil {
		return err
	}
	for i := s.cfg.BackupRetention; i < len(backups); i++ {
		if err := os.Remove(backups[i].Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Server) resolveBackupPath(route ProjectRoute, backupName string) (string, error) {
	if strings.ContainsAny(backupName, `/\`) || backupName == "." || backupName == ".." {
		return "", errors.New("invalid backupName")
	}
	path := filepath.Join(s.projectBackupDir(route), backupName)
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Server) projectBackupDir(route ProjectRoute) string {
	return filepath.Join(s.cfg.BackupRoot, route.Name)
}

func writeTarGz(sourceDir, targetPath string) error {
	file, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipWriter := gzip.NewWriter(file)
	defer gzipWriter.Close()
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	return filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == sourceDir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func extractTarGz(sourcePath, targetDir string) error {
	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		targetPath, err := safeArchiveTarget(targetDir, header.Name)
		if err != nil {
			return err
		}
		mode := header.FileInfo().Mode()
		if header.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, mode); err != nil {
				return err
			}
			continue
		}
		if !header.FileInfo().Mode().IsRegular() {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}
		output, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(output, tarReader); err != nil {
			_ = output.Close()
			return err
		}
		if err := output.Close(); err != nil {
			return err
		}
	}
}

func safeArchiveTarget(root, name string) (string, error) {
	if strings.TrimSpace(name) == "" || filepath.IsAbs(name) {
		return "", errors.New("invalid archive path")
	}
	cleaned := filepath.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("archive path traversal detected")
	}
	target := filepath.Join(root, cleaned)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("archive path traversal detected")
	}
	return target, nil
}
