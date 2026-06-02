package app

import (
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

func (s *Server) handleProjectStorageGet(w http.ResponseWriter, _ *http.Request, route ProjectRoute) error {
	unlock := s.projectStates.lock(route.Name)
	defer unlock()
	state, err := s.projectStates.load(route)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, state)
	return nil
}

func (s *Server) handleProjectArchive(w http.ResponseWriter, _ *http.Request, route ProjectRoute) error {
	if s.objectStore == nil {
		errorJSON(w, "object storage is not configured; cold storage is disabled", http.StatusPreconditionFailed)
		return nil
	}
	if s.rejectInsufficientHeadroom(w) {
		return nil
	}

	started := time.Now()
	unlock := s.projectStates.lock(route.Name)
	defer unlock()
	state, err := s.projectStates.load(route)
	if err != nil {
		return err
	}
	if state.StorageState == projectStorageArchived && state.Archive != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":    true,
			"projectId":  route.ID,
			"already":    true,
			"state":      state,
			"durationMs": time.Since(started).Milliseconds(),
		})
		return nil
	}
	if state.StorageState != projectStorageLocal && state.StorageState != projectStorageError {
		errorJSON(w, "project is already in a storage transition", http.StatusConflict)
		return nil
	}

	state.StorageState = projectStorageArchiving
	state.Error = ""
	if err := s.projectStates.save(route, state); err != nil {
		return err
	}

	sourceStopped, err := s.containers.TerminateContainer(route.Name, "project_archive_quiesce")
	if err != nil {
		return s.markProjectStorageError(route, "archive terminate failed", err)
	}
	ref, err := s.createArchiveObject(route)
	if err != nil {
		return s.markProjectStorageError(route, "archive upload failed", err)
	}
	if err := s.workspaces.Delete(route.Name); err != nil {
		return s.markProjectStorageError(route, "archive local delete failed", err)
	}
	state.StorageState = projectStorageArchived
	state.Archive = &ref
	state.Error = ""
	if err := s.projectStates.save(route, state); err != nil {
		return err
	}
	if err := s.pruneArchives(route); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"projectId":     route.ID,
		"sourceStopped": sourceStopped,
		"archive":       ref,
		"state":         state,
		"durationMs":    time.Since(started).Milliseconds(),
	})
	return nil
}

func (s *Server) handleProjectUnarchive(w http.ResponseWriter, _ *http.Request, route ProjectRoute) error {
	started := time.Now()
	unlock := s.projectStates.lock(route.Name)
	defer unlock()
	state, err := s.ensureProjectLocalLocked(route)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"projectId":  route.ID,
		"state":      state,
		"durationMs": time.Since(started).Milliseconds(),
	})
	return nil
}

func (s *Server) ensureProjectLocal(route ProjectRoute) error {
	unlock := s.projectStates.lock(route.Name)
	defer unlock()
	_, err := s.ensureProjectLocalLocked(route)
	return err
}

func (s *Server) ensureProjectLocalLocked(route ProjectRoute) (projectStorageState, error) {
	state, err := s.projectStates.load(route)
	if err != nil {
		return projectStorageState{}, err
	}
	if state.StorageState == projectStorageLocal || state.StorageState == "" {
		state.StorageState = projectStorageLocal
		state.LastActivityAt = time.Now().UTC().Format(time.RFC3339Nano)
		return state, s.projectStates.save(route, state)
	}
	if state.StorageState != projectStorageArchived {
		return projectStorageState{}, fmt.Errorf("project storage is %s; cannot start project", state.StorageState)
	}
	if state.Archive == nil || state.Archive.Key == "" {
		state.StorageState = projectStorageError
		state.Error = "project is archived without an archive object"
		_ = s.projectStates.save(route, state)
		return projectStorageState{}, errors.New(state.Error)
	}
	state.StorageState = projectStorageRestoring
	state.Error = ""
	if err := s.projectStates.save(route, state); err != nil {
		return projectStorageState{}, err
	}
	if err := s.restoreArchiveObject(route, *state.Archive); err != nil {
		return projectStorageState{}, s.markProjectStorageError(route, "archive restore failed", err)
	}
	state.StorageState = projectStorageLocal
	state.LastActivityAt = time.Now().UTC().Format(time.RFC3339Nano)
	state.Error = ""
	return state, s.projectStates.save(route, state)
}

func (s *Server) createArchiveObject(route ProjectRoute) (projectArchiveRef, error) {
	sourceDir := filepath.Join(s.workspaces.Root(), route.Name)
	if stat, err := os.Stat(sourceDir); err != nil {
		return projectArchiveRef{}, fmt.Errorf("source project unavailable: %w", err)
	} else if !stat.IsDir() {
		return projectArchiveRef{}, errors.New("source project is not a directory")
	}
	if err := os.MkdirAll(s.cfg.BackupRoot, 0o700); err != nil {
		return projectArchiveRef{}, err
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + s.workspaces.BackupExtension()
	tmpPath := filepath.Join(s.cfg.BackupRoot, route.Name+"-archive-"+name+".tmp")
	if s.workspaces.UsesZFS() {
		if _, err := s.workspaces.WriteZFSBackup(route.Name, tmpPath); err != nil {
			_ = os.Remove(tmpPath)
			return projectArchiveRef{}, err
		}
	} else {
		if err := writeTarGz(sourceDir, tmpPath); err != nil {
			_ = os.Remove(tmpPath)
			return projectArchiveRef{}, err
		}
	}
	stat, err := os.Stat(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return projectArchiveRef{}, err
	}
	file, err := os.Open(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return projectArchiveRef{}, err
	}
	key := s.projectArchiveObjectKey(route, name)
	uploaded, putErr := s.objectStore.Put(contextWithTimeout(), key, file, stat.Size(), "application/gzip")
	closeErr := file.Close()
	_ = os.Remove(tmpPath)
	if putErr != nil {
		return projectArchiveRef{}, putErr
	}
	if closeErr != nil {
		return projectArchiveRef{}, closeErr
	}
	return projectArchiveRef{
		Name:      name,
		Key:       key,
		Store:     "object",
		Size:      uploaded.Size,
		ETag:      uploaded.ETag,
		CreatedAt: uploaded.LastModified.UTC().Format(time.RFC3339Nano),
	}, nil
}

func (s *Server) restoreArchiveObject(route ProjectRoute, archive projectArchiveRef) error {
	if s.objectStore == nil {
		return errors.New("object storage is not configured")
	}
	reader, info, err := s.objectStore.Get(contextWithTimeout(), archive.Key)
	if err != nil {
		return err
	}
	defer reader.Close()
	if archive.Size > 0 && info.Size > 0 && archive.Size != info.Size {
		return fmt.Errorf("archive size mismatch: state=%d object=%d", archive.Size, info.Size)
	}
	if err := os.MkdirAll(s.cfg.BackupRoot, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.cfg.BackupRoot, route.Name+"-unarchive-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, reader); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	return s.restoreBackup(route, backupInfo{
		Name:  archive.Name,
		Path:  tmpPath,
		Store: "local",
	})
}

func (s *Server) markProjectStorageError(route ProjectRoute, message string, err error) error {
	state, loadErr := s.projectStates.load(route)
	if loadErr == nil {
		state.StorageState = projectStorageError
		state.Error = fmt.Sprintf("%s: %v", message, err)
		_ = s.projectStates.save(route, state)
	}
	return fmt.Errorf("%s: %w", message, err)
}

func (s *Server) archiveInactiveProjects() error {
	states, err := s.projectStates.list()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, state := range states {
		if state.StorageState != projectStorageLocal || strings.TrimSpace(state.LastActivityAt) == "" {
			continue
		}
		lastActivity, err := time.Parse(time.RFC3339Nano, state.LastActivityAt)
		if err != nil || now.Sub(lastActivity) < s.cfg.ArchiveAfter {
			continue
		}
		route := ProjectRoute{ID: state.ProjectID, Name: state.Runtime}
		if err := s.archiveProject(route); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) archiveProject(route ProjectRoute) error {
	unlock := s.projectStates.lock(route.Name)
	defer unlock()
	state, err := s.projectStates.load(route)
	if err != nil {
		return err
	}
	if state.StorageState != projectStorageLocal {
		return nil
	}
	state.StorageState = projectStorageArchiving
	state.Error = ""
	if err := s.projectStates.save(route, state); err != nil {
		return err
	}
	if _, err := s.containers.TerminateContainer(route.Name, "project_inactive_archive_quiesce"); err != nil {
		return s.markProjectStorageError(route, "inactive archive terminate failed", err)
	}
	ref, err := s.createArchiveObject(route)
	if err != nil {
		return s.markProjectStorageError(route, "inactive archive upload failed", err)
	}
	if err := s.workspaces.Delete(route.Name); err != nil {
		return s.markProjectStorageError(route, "inactive archive local delete failed", err)
	}
	state.StorageState = projectStorageArchived
	state.Archive = &ref
	state.Error = ""
	if err := s.projectStates.save(route, state); err != nil {
		return err
	}
	return s.pruneArchives(route)
}

func (s *Server) pruneArchives(route ProjectRoute) error {
	if s.objectStore == nil {
		return nil
	}
	objects, err := s.objectStore.List(contextWithTimeout(), s.projectArchiveObjectPrefix(route))
	if err != nil {
		return err
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].LastModified.After(objects[j].LastModified)
	})
	for i := s.cfg.ArchiveRetention; i < len(objects); i++ {
		if err := s.objectStore.Delete(contextWithTimeout(), objects[i].Key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) projectArchiveObjectPrefix(route ProjectRoute) string {
	return objectKey(s.cfg.ObjectStorePrefix, "archives", route.Name) + "/"
}

func (s *Server) projectArchiveObjectKey(route ProjectRoute, name string) string {
	return objectKey(s.cfg.ObjectStorePrefix, "archives", route.Name, name)
}
