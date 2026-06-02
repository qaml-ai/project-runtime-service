package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/qaml-ai/project-runtime-service/internal/workspace"
)

func TestProjectNameNormalization(t *testing.T) {
	got := projectName("Pizza Delivery/../../Main")
	want := "project-pizza-delivery-main"
	if got != want {
		t.Fatalf("projectName mismatch: got %q want %q", got, want)
	}
}

func TestParseProjectRoute(t *testing.T) {
	route, ok := parseProjectRoute("/v1/projects/pizza-delivery/fs/read")
	if !ok {
		t.Fatal("expected route to parse")
	}
	if route.ID != "pizza-delivery" || route.Name != "project-pizza-delivery" || route.Subpath != "/fs/read" {
		t.Fatalf("unexpected route: %+v", route)
	}
}

func TestBearerControlAuth(t *testing.T) {
	server := &Server{cfg: Config{ControlAuthType: "bearer", ControlBearerToken: "secret"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/host/capabilities", nil)
	rec := httptest.NewRecorder()

	if !server.rejectUnauthenticatedControlRequest(rec, req) {
		t.Fatal("expected missing bearer token to be rejected")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/host/capabilities", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	if server.rejectUnauthenticatedControlRequest(rec, req) {
		t.Fatal("expected matching bearer token to pass")
	}
}

func TestProxyCapabilityParsingAndHeaders(t *testing.T) {
	cfg := Config{ProxyCapabilitiesJSON: `{
		"capabilities": [
			{"name":"cf-api","target":"https://api.example.com","bearerToken":"tok","allowedProjects":["pizza-delivery"]}
		]
	}`}
	capabilities := loadProxyCapabilities(cfg)
	capability, ok := capabilities["cf-api"]
	if !ok {
		t.Fatal("expected cf-api capability")
	}
	if !capabilityAllowsProject(capability, "project-pizza-delivery", "pizza-delivery") {
		t.Fatal("expected project id attachment to match")
	}
	if capabilityAllowsProject(capability, "project-other", "other") {
		t.Fatal("did not expect unrelated project to match")
	}

	source := http.Header{}
	source.Set("X-Project-Runtime-Project", "spoofed")
	source.Set("X-Project-Runtime-Org", "spoofed")
	source.Set("X-Keep", "yes")
	target := http.Header{}
	copyProxyHeaders(target, source)
	if target.Get("X-Project-Runtime-Project") != "" || target.Get("X-Project-Runtime-Org") != "" {
		t.Fatalf("expected spoofable headers to be stripped: %+v", target)
	}
	if target.Get("X-Keep") != "yes" {
		t.Fatalf("expected normal header to be kept")
	}
}

func TestGenericProxyPathAndTarget(t *testing.T) {
	name, suffix, ok := parseGenericProxyPath("/p/github/repos/qaml-ai/project-runtime-service")
	if !ok || name != "github" || suffix != "/repos/qaml-ai/project-runtime-service" {
		t.Fatalf("unexpected proxy path parse: name=%q suffix=%q ok=%v", name, suffix, ok)
	}
	target, err := buildProxyTargetURL("https://api.github.com", suffix, "per_page=1")
	if err != nil {
		t.Fatalf("buildProxyTargetURL failed: %v", err)
	}
	if target != "https://api.github.com/repos/qaml-ai/project-runtime-service?per_page=1" {
		t.Fatalf("unexpected target URL: %s", target)
	}
}

func TestSafeArchiveTargetRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../secret", "/absolute", "nested/../../secret"} {
		if _, err := safeArchiveTarget("/tmp/project", name); err == nil {
			t.Fatalf("expected traversal path %q to be rejected", name)
		}
	}
	target, err := safeArchiveTarget("/tmp/project", "src/main.go")
	if err != nil {
		t.Fatalf("safeArchiveTarget failed: %v", err)
	}
	if !strings.HasSuffix(target, "/tmp/project/src/main.go") {
		t.Fatalf("unexpected target: %s", target)
	}
}

func TestObjectBackupsRoundTripAndPrune(t *testing.T) {
	root := t.TempDir()
	route := ProjectRoute{ID: "pizza", Name: "project-pizza"}
	if err := os.MkdirAll(filepath.Join(root, "workspaces", route.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workspaces", route.Name, "app.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newMemoryObjectStore()
	server := &Server{
		cfg: Config{
			BackupRoot:        filepath.Join(root, "backups"),
			BackupRetention:   1,
			ObjectStorePrefix: "runtime",
		},
		workspaces:  workspace.NewManager(workspace.ManagerConfig{WorkspacesRoot: filepath.Join(root, "workspaces")}),
		objectStore: store,
	}

	first, err := server.createBackup(route)
	if err != nil {
		t.Fatalf("createBackup first failed: %v", err)
	}
	time.Sleep(time.Millisecond)
	second, err := server.createBackup(route)
	if err != nil {
		t.Fatalf("createBackup second failed: %v", err)
	}
	if first.Store != "object" || second.Store != "object" || first.Key == "" || second.Key == "" {
		t.Fatalf("expected object backups, got first=%+v second=%+v", first, second)
	}
	backups, err := server.listBackups(route)
	if err != nil {
		t.Fatalf("listBackups failed: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("expected 2 backups before prune, got %d", len(backups))
	}
	if err := server.pruneBackups(route); err != nil {
		t.Fatalf("pruneBackups failed: %v", err)
	}
	backups, err = server.listBackups(route)
	if err != nil {
		t.Fatalf("listBackups after prune failed: %v", err)
	}
	if len(backups) != 1 || backups[0].Name != second.Name {
		t.Fatalf("expected newest backup only after prune, got %+v", backups)
	}
}

func TestArchiveRestoreKeepsProjectUnavailableOnDownloadFailure(t *testing.T) {
	root := t.TempDir()
	route := ProjectRoute{ID: "pizza", Name: "project-pizza"}
	stateRoot := filepath.Join(root, "state")
	store := newMemoryObjectStore()
	server := &Server{
		cfg: Config{
			BackupRoot:        filepath.Join(root, "tmp"),
			ProjectStateRoot:  stateRoot,
			ArchiveRetention:  2,
			ObjectStorePrefix: "runtime",
		},
		workspaces:    workspace.NewManager(workspace.ManagerConfig{WorkspacesRoot: filepath.Join(root, "workspaces")}),
		objectStore:   store,
		projectStates: newProjectStateStore(stateRoot),
	}
	unlock := server.projectStates.lock(route.Name)
	if err := server.projectStates.save(route, projectStorageState{
		ProjectID:    route.ID,
		Runtime:      route.Name,
		StorageState: projectStorageArchived,
		Archive: &projectArchiveRef{
			Name:  "missing.tar.gz",
			Key:   "runtime/archives/project-pizza/missing.tar.gz",
			Store: "object",
		},
	}); err != nil {
		unlock()
		t.Fatal(err)
	}
	_, err := server.ensureProjectLocalLocked(route)
	unlock()
	if err == nil {
		t.Fatal("expected restore to fail")
	}
	state, err := server.projectStates.load(route)
	if err != nil {
		t.Fatal(err)
	}
	if state.StorageState != projectStorageError {
		t.Fatalf("expected storage error state, got %+v", state)
	}
	if _, statErr := os.Stat(filepath.Join(root, "workspaces", route.Name)); !os.IsNotExist(statErr) {
		t.Fatalf("expected no empty project to be created, statErr=%v", statErr)
	}
}

type memoryObjectStore struct {
	items map[string]memoryObject
}

type memoryObject struct {
	content []byte
	info    objectInfo
}

func newMemoryObjectStore() *memoryObjectStore {
	return &memoryObjectStore{items: map[string]memoryObject{}}
}

func (m *memoryObjectStore) Put(_ context.Context, key string, body io.Reader, _ int64, _ string) (objectInfo, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return objectInfo{}, err
	}
	info := objectInfo{Key: key, Size: int64(len(content)), LastModified: time.Now().UTC()}
	m.items[key] = memoryObject{content: content, info: info}
	return info, nil
}

func (m *memoryObjectStore) Get(_ context.Context, key string) (io.ReadCloser, objectInfo, error) {
	item, ok := m.items[key]
	if !ok {
		return nil, objectInfo{}, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(item.content)), item.info, nil
}

func (m *memoryObjectStore) Delete(_ context.Context, key string) error {
	delete(m.items, key)
	return nil
}

func (m *memoryObjectStore) List(_ context.Context, prefix string) ([]objectInfo, error) {
	var out []objectInfo
	for key, item := range m.items {
		if strings.HasPrefix(key, prefix) {
			out = append(out, item.info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastModified.After(out[j].LastModified)
	})
	return out, nil
}
