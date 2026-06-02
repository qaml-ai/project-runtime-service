package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	projectStorageLocal     = "local"
	projectStorageArchiving = "archiving"
	projectStorageArchived  = "archived"
	projectStorageRestoring = "restoring"
	projectStorageError     = "error"
)

type projectStorageState struct {
	ProjectID      string             `json:"projectId"`
	Runtime        string             `json:"runtime"`
	StorageState   string             `json:"storageState"`
	LastActivityAt string             `json:"lastActivityAt,omitempty"`
	UpdatedAt      string             `json:"updatedAt"`
	Archive        *projectArchiveRef `json:"archive,omitempty"`
	Error          string             `json:"error,omitempty"`
}

type projectArchiveRef struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	Store     string `json:"store"`
	Size      int64  `json:"size"`
	ETag      string `json:"etag,omitempty"`
	CreatedAt string `json:"createdAt"`
}

type projectStateStore struct {
	root string
	mu   sync.Map // runtime name -> *sync.Mutex
}

func newProjectStateStore(root string) *projectStateStore {
	return &projectStateStore{root: root}
}

func (s *projectStateStore) lock(runtime string) func() {
	value, _ := s.mu.LoadOrStore(runtime, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (s *projectStateStore) load(route ProjectRoute) (projectStorageState, error) {
	path := s.path(route)
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			return projectStorageState{
				ProjectID:    route.ID,
				Runtime:      route.Name,
				StorageState: projectStorageLocal,
				UpdatedAt:    now,
			}, nil
		}
		return projectStorageState{}, err
	}
	var state projectStorageState
	if err := json.Unmarshal(content, &state); err != nil {
		return projectStorageState{}, err
	}
	if strings.TrimSpace(state.StorageState) == "" {
		state.StorageState = projectStorageLocal
	}
	if state.ProjectID == "" {
		state.ProjectID = route.ID
	}
	if state.Runtime == "" {
		state.Runtime = route.Name
	}
	return state, nil
}

func (s *projectStateStore) save(route ProjectRoute, state projectStorageState) error {
	state.ProjectID = route.ID
	state.Runtime = route.Name
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := os.MkdirAll(filepath.Dir(s.path(route)), 0o700); err != nil {
		return err
	}
	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(route) + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(route))
}

func (s *projectStateStore) touch(route ProjectRoute) {
	unlock := s.lock(route.Name)
	defer unlock()
	state, err := s.load(route)
	if err != nil {
		return
	}
	state.LastActivityAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = s.save(route, state)
}

func (s *projectStateStore) list() ([]projectStorageState, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []projectStorageState{}, nil
		}
		return nil, err
	}
	var states []projectStorageState
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(s.root, entry.Name()))
		if err != nil {
			continue
		}
		var state projectStorageState
		if err := json.Unmarshal(content, &state); err != nil {
			continue
		}
		if state.ProjectID == "" || state.Runtime == "" {
			continue
		}
		if strings.TrimSpace(state.StorageState) == "" {
			state.StorageState = projectStorageLocal
		}
		states = append(states, state)
	}
	return states, nil
}

func (s *projectStateStore) path(route ProjectRoute) string {
	return filepath.Join(s.root, route.Name+".json")
}
