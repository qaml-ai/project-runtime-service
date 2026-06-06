package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/qaml-ai/project-runtime-service/internal/container"
	"github.com/qaml-ai/project-runtime-service/internal/fsops"
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

func TestLegacyWorkspaceMigrationDiscoveryFindsWorkerAppsWithPrunes(t *testing.T) {
	root := t.TempDir()
	legacyWorkspacesRoot := filepath.Join(root, "legacy-workspaces")
	legacyRoot := filepath.Join(legacyWorkspacesRoot, "chiridion-ws-legacy-ws")
	for _, dir := range []string{
		filepath.Join(legacyRoot, "projects", "hello-world"),
		filepath.Join(legacyRoot, "projects", "sf-muni"),
		filepath.Join(legacyRoot, "projects", "node_modules", "fake-worker"),
		filepath.Join(legacyRoot, "app", "build", "fake-worker"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{
		filepath.Join(legacyRoot, "projects", "hello-world", "wrangler.jsonc"),
		filepath.Join(legacyRoot, "projects", "sf-muni", "wrangler.toml"),
		filepath.Join(legacyRoot, "projects", "node_modules", "fake-worker", "wrangler.jsonc"),
		filepath.Join(legacyRoot, "app", "build", "fake-worker", "wrangler.jsonc"),
	} {
		if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	server := &Server{
		cfg: Config{
			LegacyWorkspacesRoot:  legacyWorkspacesRoot,
			LegacyWorkspacePrefix: "chiridion-ws-",
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/org-1/legacy-ws/migration-discovery", nil)
	rec := httptest.NewRecorder()

	if err := server.handleWorkspaceMigrationDiscovery(rec, req, WorkspaceRoute{OrgID: "org-1", WorkspaceID: "legacy-ws"}); err != nil {
		t.Fatalf("handleWorkspaceMigrationDiscovery failed: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Files            []fsops.Entry `json:"files"`
		NestedWorkerApps []string      `json:"nestedWorkerApps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got, want := payload.NestedWorkerApps, []string{"/home/claude/projects/hello-world", "/home/claude/projects/sf-muni"}; !slices.Equal(got, want) {
		t.Fatalf("nested worker apps mismatch: got=%v want=%v", got, want)
	}
	if len(payload.Files) == 0 || payload.Files[0].RelativePath == "" {
		t.Fatalf("expected top-level files with relative paths, got=%+v", payload.Files)
	}
}

func TestForwardProjectCloudflareAPIProxyRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/client/v4/accounts/acct" {
			t.Fatalf("unexpected upstream path: %s", req.URL.Path)
		}
		if req.Header.Get("X-Project-Runtime-Secret") != "shared-secret" {
			t.Fatalf("missing project runtime secret header: %q", req.Header.Get("X-Project-Runtime-Secret"))
		}
		if req.Header.Get("X-Chiridion-Org-Id") != "" {
			t.Fatalf("unexpected org forwarding header: %q", req.Header.Get("X-Chiridion-Org-Id"))
		}
		if req.Header.Get("X-Chiridion-Workspace-Id") != "" {
			t.Fatalf("unexpected workspace forwarding header: %q", req.Header.Get("X-Chiridion-Workspace-Id"))
		}
		if req.Header.Get("X-Chiridion-Project-Id") != "pizza-delivery" {
			t.Fatalf("missing project forwarding header: %q", req.Header.Get("X-Chiridion-Project-Id"))
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer upstream.Close()

	server := &Server{
		cfg: Config{
			WorkerBaseURL:             upstream.URL,
			ProjectRuntimeProxySecret: "shared-secret",
		},
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	route := trustedProxyRoute{
		Name:      "project-runtime-pizza",
		ProjectID: "pizza-delivery",
		Subpath:   "/client/v4/accounts/acct",
	}
	req := httptest.NewRequest(http.MethodGet, "/deploy/client/v4/accounts/acct", nil)
	rec := httptest.NewRecorder()

	if err := server.forwardCloudflareAPIProxyRequest(rec, req, route); err != nil {
		t.Fatalf("forwardCloudflareAPIProxyRequest failed: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != `{"success":true}` {
		t.Fatalf("unexpected body: %q", got)
	}
}

func TestForwardProjectCloudflareAPIProxyRequestAllowsMTLSOnlyAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/client/v4/accounts/acct" {
			t.Fatalf("unexpected upstream path: %s", req.URL.Path)
		}
		if req.Header.Get("X-Project-Runtime-Secret") != "" {
			t.Fatalf("unexpected project runtime secret header: %q", req.Header.Get("X-Project-Runtime-Secret"))
		}
		if req.Header.Get("X-Chiridion-Project-Id") != "pizza-delivery" {
			t.Fatalf("missing project forwarding header: %q", req.Header.Get("X-Chiridion-Project-Id"))
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer upstream.Close()

	server := &Server{
		cfg: Config{
			WorkerBaseURL:           upstream.URL,
			ProxyMTLSClientCertFile: "/etc/qaml-project-runtime/mtls/client.crt",
			ProxyMTLSClientKeyFile:  "/etc/qaml-project-runtime/mtls/client.key",
		},
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	route := trustedProxyRoute{
		Name:      "project-runtime-pizza",
		ProjectID: "pizza-delivery",
		Subpath:   "/client/v4/accounts/acct",
	}
	req := httptest.NewRequest(http.MethodGet, "/deploy/client/v4/accounts/acct", nil)
	rec := httptest.NewRecorder()

	if err := server.forwardCloudflareAPIProxyRequest(rec, req, route); err != nil {
		t.Fatalf("forwardCloudflareAPIProxyRequest failed: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusOK)
	}
}

func TestForwardProjectCloudflareAPIProxyRequestRequiresAuthConfig(t *testing.T) {
	server := &Server{
		cfg: Config{
			WorkerBaseURL: "https://example.com",
		},
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	route := trustedProxyRoute{
		Name:      "project-runtime-pizza",
		ProjectID: "pizza-delivery",
		Subpath:   "/client/v4/accounts/acct",
	}
	req := httptest.NewRequest(http.MethodGet, "/deploy/client/v4/accounts/acct", nil)
	rec := httptest.NewRecorder()

	if err := server.forwardCloudflareAPIProxyRequest(rec, req, route); err != nil {
		t.Fatalf("forwardCloudflareAPIProxyRequest failed: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status: got=%d want=%d", rec.Code, http.StatusServiceUnavailable)
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
	configFile := filepath.Join(t.TempDir(), "proxies.json")
	if err := os.WriteFile(configFile, []byte(`{
		"capabilities": [
			{"name":"cf-api","target":"https://api.example.com","bearerToken":"tok","allowedProjects":["pizza-delivery"]}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ProxyCapabilitiesFile: configFile}
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
	source.Set("X-Chiridion-Project-Id", "spoofed")
	source.Set("X-Keep", "yes")
	target := http.Header{}
	copyProxyHeaders(target, source)
	if target.Get("X-Project-Runtime-Project") != "" || target.Get("X-Project-Runtime-Org") != "" {
		t.Fatalf("expected spoofable headers to be stripped: %+v", target)
	}
	if target.Get("X-Chiridion-Project-Id") != "" {
		t.Fatalf("expected chiridion identity headers to be stripped: %+v", target)
	}
	if target.Get("X-Keep") != "yes" {
		t.Fatalf("expected normal header to be kept")
	}
}

func TestProxyCapabilityParsingFromJSONConfig(t *testing.T) {
	cfg := Config{
		ProxyCapabilitiesJSON: `{
			"capabilities": [
				{
					"name": "camelai-artifacts",
					"target": "https://staging.camelai.dev/api/internal/project-runtime/artifacts",
					"headers": {"X-Project-Runtime-Secret": "secret"}
				}
			]
		}`,
	}

	capabilities := loadProxyCapabilities(cfg)
	capability, ok := capabilities["camelai-artifacts"]
	if !ok {
		t.Fatal("expected camelai-artifacts capability")
	}
	if capability.Target != "https://staging.camelai.dev/api/internal/project-runtime/artifacts" {
		t.Fatalf("unexpected target: %q", capability.Target)
	}
	if capability.Headers["X-Project-Runtime-Secret"] != "secret" {
		t.Fatalf("expected proxy secret header, got %+v", capability.Headers)
	}
}

func TestLegacyWorkspaceMigrationLockAndImport(t *testing.T) {
	root := t.TempDir()
	workspacesRoot := filepath.Join(root, "workspaces")
	legacyWorkspacesRoot := filepath.Join(root, "legacy-workspaces")
	workspaceManager := workspace.NewManager(workspace.ManagerConfig{WorkspacesRoot: workspacesRoot})
	server := &Server{
		cfg: Config{
			LegacyWorkspacesRoot:  legacyWorkspacesRoot,
			LegacyWorkspacePrefix: "chiridion-ws-",
		},
		containers:     container.NewTestManager(),
		workspaces:     workspaceManager,
		fs:             fsops.NewManager(workspacesRoot),
		projectStates:  newProjectStateStore(filepath.Join(root, "state")),
		migrationLocks: newMigrationLockStore(),
	}

	legacyRoot := filepath.Join(legacyWorkspacesRoot, "chiridion-ws-legacy-ws")
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacyRoot, "web-app", "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "web-app", "package.json"), []byte(`{"scripts":{"build":"vite"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "web-app", "src", "main.ts"), []byte("console.log('hello')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacyRoot, "web-app", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "web-app", ".git", "config"), []byte("[core]\nrepositoryformatversion = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacyRoot, "web-app", ".cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "web-app", ".cache", "ignored.txt"), []byte("skip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "web-app", ".env"), []byte("SECRET=skip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacyRoot, "web-app", ".ipynb_checkpoints"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "web-app", ".ipynb_checkpoints", "scratch.ipynb"), []byte("skip"), 0o600); err != nil {
		t.Fatal(err)
	}
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}

	importBody := `{"orgId":"org-1","workspaceId":"legacy-ws","sourcePaths":["/home/claude/web-app"],"ignoreGlobs":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/new-project/legacy-import", strings.NewReader(importBody))
	req.Header.Set("Content-Type", "application/json")
	rec := serve(req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected import without lock to fail precondition, got=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/workspaces/org-1/legacy-ws/migration-lock", strings.NewReader(`{"workflowId":"workflow-1","ttlMs":60000}`))
	req.Header.Set("Content-Type", "application/json")
	rec = serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected lock to succeed, got=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/v1/workspaces/org-1/legacy-ws/fs/write?path=/during-migration.txt", strings.NewReader("blocked"))
	rec = serve(req)
	if rec.Code != http.StatusLocked {
		t.Fatalf("expected legacy workspace write to be locked, got=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/projects/new-project/legacy-import", strings.NewReader(importBody))
	req.Header.Set("Content-Type", "application/json")
	rec = serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected locked import to succeed, got=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("expected success response, got %s", rec.Body.String())
	}

	projectRoot := filepath.Join(workspacesRoot, projectName("new-project"))
	if got, err := os.ReadFile(filepath.Join(projectRoot, "package.json")); err != nil || !strings.Contains(string(got), "vite") {
		t.Fatalf("expected package.json to be imported, err=%v got=%q", err, string(got))
	}
	if got, err := os.ReadFile(filepath.Join(projectRoot, "src", "main.ts")); err != nil || !strings.Contains(string(got), "hello") {
		t.Fatalf("expected src/main.ts to be imported, err=%v got=%q", err, string(got))
	}
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected empty .git to be removed, err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(projectRoot, ".cache", "ignored.txt")); err != nil || string(got) != "skip" {
		t.Fatalf("expected .cache file to be imported, err=%v got=%q", err, string(got))
	}
	if got, err := os.ReadFile(filepath.Join(projectRoot, ".env")); err != nil || string(got) != "SECRET=skip" {
		t.Fatalf("expected .env file to be imported, err=%v got=%q", err, string(got))
	}
	if got, err := os.ReadFile(filepath.Join(projectRoot, ".ipynb_checkpoints", "scratch.ipynb")); err != nil || string(got) != "skip" {
		t.Fatalf("expected hidden checkpoint dir to be imported, err=%v got=%q", err, string(got))
	}
}

func TestLegacyWorkspaceImportKeepsCommittedGitRepoWithExistingRemote(t *testing.T) {
	root := t.TempDir()
	workspacesRoot := filepath.Join(root, "workspaces")
	legacyWorkspacesRoot := filepath.Join(root, "legacy-workspaces")
	workspaceManager := workspace.NewManager(workspace.ManagerConfig{WorkspacesRoot: workspacesRoot})
	server := &Server{
		cfg: Config{
			LegacyWorkspacesRoot:  legacyWorkspacesRoot,
			LegacyWorkspacePrefix: "chiridion-ws-",
		},
		containers:     container.NewTestManager(),
		workspaces:     workspaceManager,
		fs:             fsops.NewManager(workspacesRoot),
		projectStates:  newProjectStateStore(filepath.Join(root, "state")),
		migrationLocks: newMigrationLockStore(),
	}

	legacyRoot := filepath.Join(legacyWorkspacesRoot, "chiridion-ws-legacy-ws")
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Join(legacyRoot, "committed-app")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "package.json"), []byte(`{"scripts":{"build":"vite"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyRemote := "https://legacy.example/repo.git"
	runGit(t, repoRoot, "init")
	runGit(t, repoRoot, "config", "user.name", "Legacy User")
	runGit(t, repoRoot, "config", "user.email", "legacy@example.com")
	runGit(t, repoRoot, "add", "package.json")
	runGit(t, repoRoot, "commit", "-m", "Initial commit")
	runGit(t, repoRoot, "remote", "add", "origin", legacyRemote)

	serve := func(req *http.Request) *httptest.ResponseRecorder {
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/org-1/legacy-ws/migration-lock", strings.NewReader(`{"workflowId":"workflow-1","ttlMs":60000}`))
	req.Header.Set("Content-Type", "application/json")
	rec := serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected lock to succeed, got=%d body=%s", rec.Code, rec.Body.String())
	}

	importBody := `{"orgId":"org-1","workspaceId":"legacy-ws","sourcePaths":["/home/claude/committed-app"],"ignoreGlobs":[]}`
	req = httptest.NewRequest(http.MethodPost, "/v1/projects/new-project/legacy-import", strings.NewReader(importBody))
	req.Header.Set("Content-Type", "application/json")
	rec = serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected import to succeed, got=%d body=%s", rec.Code, rec.Body.String())
	}

	projectRoot := filepath.Join(workspacesRoot, projectName("new-project"))
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err != nil {
		t.Fatalf("expected committed .git to be imported, err=%v", err)
	}
	if output := runGitOutput(t, projectRoot, "rev-list", "--count", "--all"); strings.TrimSpace(output) != "1" {
		t.Fatalf("expected one imported commit, got %q", output)
	}
	if output := runGitOutput(t, projectRoot, "remote", "get-url", "origin"); strings.TrimSpace(output) != legacyRemote {
		t.Fatalf("expected origin to be preserved, got %q", output)
	}
}

func TestLegacyWorkspaceImportConfiguresArtifactRemote(t *testing.T) {
	root := t.TempDir()
	workspacesRoot := filepath.Join(root, "workspaces")
	legacyWorkspacesRoot := filepath.Join(root, "legacy-workspaces")
	workspaceManager := workspace.NewManager(workspace.ManagerConfig{WorkspacesRoot: workspacesRoot})
	server := &Server{
		cfg: Config{
			LegacyWorkspacesRoot:  legacyWorkspacesRoot,
			LegacyWorkspacePrefix: "chiridion-ws-",
		},
		containers:     container.NewTestManager(),
		workspaces:     workspaceManager,
		fs:             fsops.NewManager(workspacesRoot),
		projectStates:  newProjectStateStore(filepath.Join(root, "state")),
		migrationLocks: newMigrationLockStore(),
	}

	legacyRoot := filepath.Join(legacyWorkspacesRoot, "chiridion-ws-legacy-ws")
	repoRoot := filepath.Join(legacyRoot, "committed-app")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".bun", "install", "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "package.json"), []byte(`{"scripts":{"build":"vite"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".bun", "install", "cache", "junk"), []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoRoot, "init")
	runGit(t, repoRoot, "config", "user.name", "Legacy User")
	runGit(t, repoRoot, "config", "user.email", "legacy@example.com")
	runGit(t, repoRoot, "add", "package.json")
	runGit(t, repoRoot, "commit", "-m", "Initial commit")
	runGit(t, repoRoot, "remote", "add", "origin", "https://legacy.example/repo.git")

	serve := func(req *http.Request) *httptest.ResponseRecorder {
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/org-1/legacy-ws/migration-lock", strings.NewReader(`{"workflowId":"workflow-1","ttlMs":60000}`))
	req.Header.Set("Content-Type", "application/json")
	rec := serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected lock to succeed, got=%d body=%s", rec.Code, rec.Body.String())
	}

	artifactRemote := "http://host.docker.internal:8089/p/camelai-artifacts/git/origin.git"
	importBody := fmt.Sprintf(
		`{"orgId":"org-1","workspaceId":"legacy-ws","sourcePaths":["/home/claude/committed-app"],"ignoreGlobs":[],"gitRemote":%q,"gitBranch":"main"}`,
		artifactRemote,
	)
	req = httptest.NewRequest(http.MethodPost, "/v1/projects/new-project/legacy-import", strings.NewReader(importBody))
	req.Header.Set("Content-Type", "application/json")
	rec = serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected import to succeed, got=%d body=%s", rec.Code, rec.Body.String())
	}

	projectRoot := filepath.Join(workspacesRoot, projectName("new-project"))
	if output := runGitOutput(t, projectRoot, "rev-list", "--count", "--all"); strings.TrimSpace(output) != "1" {
		t.Fatalf("expected one imported commit, got %q", output)
	}
	if output := runGitOutput(t, projectRoot, "remote", "get-url", "origin"); strings.TrimSpace(output) != artifactRemote {
		t.Fatalf("expected origin to be retargeted, got %q", output)
	}
	gitignore, err := os.ReadFile(filepath.Join(projectRoot, ".gitignore"))
	if err != nil {
		t.Fatalf("expected .gitignore to be written: %v", err)
	}
	for _, pattern := range []string{".bun/install/cache/", ".EasyOCR/", ".cache/"} {
		if !strings.Contains(string(gitignore), pattern) {
			t.Fatalf("expected .gitignore to contain %q, got %q", pattern, string(gitignore))
		}
	}
}

func TestLegacyWorkspaceImportReplacesExistingProjectContentsOnRetry(t *testing.T) {
	root := t.TempDir()
	workspacesRoot := filepath.Join(root, "workspaces")
	legacyWorkspacesRoot := filepath.Join(root, "legacy-workspaces")
	workspaceManager := workspace.NewManager(workspace.ManagerConfig{WorkspacesRoot: workspacesRoot})
	server := &Server{
		cfg: Config{
			LegacyWorkspacesRoot:  legacyWorkspacesRoot,
			LegacyWorkspacePrefix: "chiridion-ws-",
		},
		containers:     container.NewTestManager(),
		workspaces:     workspaceManager,
		fs:             fsops.NewManager(workspacesRoot),
		projectStates:  newProjectStateStore(filepath.Join(root, "state")),
		migrationLocks: newMigrationLockStore(),
	}

	legacyRoot := filepath.Join(legacyWorkspacesRoot, "chiridion-ws-legacy-ws")
	if err := os.MkdirAll(filepath.Join(legacyRoot, "web-app", "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "web-app", "src", "main.ts"), []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	serve := func(req *http.Request) *httptest.ResponseRecorder {
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/org-1/legacy-ws/migration-lock", strings.NewReader(`{"workflowId":"workflow-1","ttlMs":60000}`))
	req.Header.Set("Content-Type", "application/json")
	rec := serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected lock to succeed, got=%d body=%s", rec.Code, rec.Body.String())
	}

	importBody := `{"orgId":"org-1","workspaceId":"legacy-ws","sourcePaths":["/home/claude/web-app"],"ignoreGlobs":[]}`
	req = httptest.NewRequest(http.MethodPost, "/v1/projects/new-project/legacy-import", strings.NewReader(importBody))
	req.Header.Set("Content-Type", "application/json")
	rec = serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected first import to succeed, got=%d body=%s", rec.Code, rec.Body.String())
	}

	projectRoot := filepath.Join(workspacesRoot, projectName("new-project"))
	if err := os.WriteFile(filepath.Join(projectRoot, "stale.txt"), []byte("partial retry residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, "old-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "old-dir", "old.txt"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/projects/new-project/legacy-import", strings.NewReader(importBody))
	req.Header.Set("Content-Type", "application/json")
	rec = serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry import to succeed, got=%d body=%s", rec.Code, rec.Body.String())
	}

	if got, err := os.ReadFile(filepath.Join(projectRoot, "src", "main.ts")); err != nil || string(got) != "fresh\n" {
		t.Fatalf("expected source file after retry import, err=%v got=%q", err, string(got))
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "stale.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale file to be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "old-dir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale directory to be removed, err=%v", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
	return string(output)
}

func TestStaticCloudflareAPIProxySubpath(t *testing.T) {
	if !isStaticCloudflareAPIProxyRoute("/deploy/client/v4/accounts/acct") {
		t.Fatal("expected static deploy proxy route")
	}
	if got := staticCloudflareAPIProxySubpath("/deploy/client/v4/accounts/acct"); got != "/client/v4/accounts/acct" {
		t.Fatalf("unexpected static proxy subpath: %q", got)
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
