package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/qaml-ai/project-runtime-service/internal/container"
	"github.com/qaml-ai/project-runtime-service/internal/workspace"
)

type ProjectRoute struct {
	ID      string
	Name    string
	Subpath string
}

var projectRouteRegex = regexp.MustCompile(`^/v1/projects/([^/]+)(/.*)?$`)

func (s *Server) handleProjectRoute(w http.ResponseWriter, req *http.Request) error {
	route, ok := parseProjectRoute(req.URL.Path)
	if !ok {
		errorJSON(w, "Not found", http.StatusNotFound)
		return nil
	}

	if route.Subpath == "" && req.Method == http.MethodGet {
		return s.handleProjectGet(w, req, route)
	}
	if route.Subpath == "" && req.Method == http.MethodDelete {
		success, err := s.containers.TerminateContainer(route.Name, "project_delete")
		if err != nil {
			return err
		}
		if err := s.workspaces.Delete(route.Name); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "terminated": success})
		return nil
	}
	if route.Subpath == "/storage" && req.Method == http.MethodGet {
		return s.handleProjectStorageGet(w, req, route)
	}
	if route.Subpath == "/archive" && req.Method == http.MethodPost {
		return s.handleProjectArchive(w, req, route)
	}
	if route.Subpath == "/unarchive" && req.Method == http.MethodPost {
		return s.handleProjectUnarchive(w, req, route)
	}
	if route.Subpath == "/terminate" && req.Method == http.MethodPost {
		success, err := s.containers.TerminateContainer(route.Name, "project_terminate")
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": success})
		return nil
	}
	if route.Subpath == "/exec" && req.Method == http.MethodPost {
		if err := s.ensureProjectLocal(route); err != nil {
			return err
		}
		return s.handleExec(w, req, route.Name, container.EnsureContainerOptions{WorkspaceID: route.ID})
	}
	if strings.HasPrefix(route.Subpath, "/fs/") {
		if err := s.ensureProjectLocal(route); err != nil {
			return err
		}
		if req.Method != http.MethodGet && s.rejectInsufficientHeadroom(w) {
			return nil
		}
		if !isReadOnlyFSRoute(req.Method, route.Subpath) {
			if _, err := s.workspaces.Ensure(route.Name); err != nil {
				return err
			}
		}
		return s.handleProjectFSRoute(w, req, route)
	}
	if route.Subpath == "/clone" && req.Method == http.MethodPost {
		if err := s.ensureProjectLocal(route); err != nil {
			return err
		}
		return s.handleProjectClone(w, req, route)
	}
	if route.Subpath == "/legacy-import" && req.Method == http.MethodPost {
		if err := s.ensureProjectLocal(route); err != nil {
			return err
		}
		return s.handleProjectLegacyImport(w, req, route)
	}
	if route.Subpath == "/backups" && req.Method == http.MethodGet {
		return s.handleProjectBackupsList(w, req, route)
	}
	if route.Subpath == "/backups" && req.Method == http.MethodPost {
		if err := s.ensureProjectLocal(route); err != nil {
			return err
		}
		return s.handleProjectBackupCreate(w, req, route)
	}
	if route.Subpath == "/restore" && req.Method == http.MethodPost {
		return s.handleProjectRestore(w, req, route)
	}
	if route.Subpath == "/proxies" && req.Method == http.MethodGet {
		return s.handleProjectProxiesList(w, req, route)
	}

	errorJSON(w, "Not found", http.StatusNotFound)
	return nil
}

func (s *Server) handleProjectGet(w http.ResponseWriter, _ *http.Request, route ProjectRoute) error {
	exists, err := s.fs.Exists(route.Name, "/")
	if err != nil {
		return err
	}
	state, _ := s.projectStates.load(route)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           route.ID,
		"runtime":      route.Name,
		"exists":       exists.Exists,
		"storageState": state.StorageState,
		"archive":      state.Archive,
		"timestamp":    time.Now().UTC().Format(time.RFC3339Nano),
	})
	return nil
}

func (s *Server) handleProjectFSRoute(w http.ResponseWriter, req *http.Request, route ProjectRoute) error {
	switch {
	case route.Subpath == "/fs/read" && req.Method == http.MethodGet:
		legacyRoute := WorkspaceRoute{Name: route.Name, WorkspaceID: route.ID}
		return s.handleFSRead(w, req, legacyRoute)
	case route.Subpath == "/fs/write" && req.Method == http.MethodPut:
		return s.handleFSWrite(w, req, route.Name)
	case route.Subpath == "/fs/list" && req.Method == http.MethodGet:
		return s.handleFSList(w, req, route.Name)
	case route.Subpath == "/fs/delete" && req.Method == http.MethodDelete:
		return s.handleFSDelete(w, req, route.Name)
	case route.Subpath == "/fs/move" && req.Method == http.MethodPost:
		return s.handleFSMove(w, req, route.Name)
	case route.Subpath == "/fs/mkdir" && req.Method == http.MethodPost:
		return s.handleFSMkdir(w, req, route.Name)
	case route.Subpath == "/fs/exists" && req.Method == http.MethodGet:
		return s.handleFSExists(w, req, route.Name)
	default:
		errorJSON(w, "Not found", http.StatusNotFound)
		return nil
	}
}

func (s *Server) handleProjectClone(w http.ResponseWriter, req *http.Request, route ProjectRoute) error {
	var payload struct {
		TargetProjectID string `json:"targetProjectId"`
	}
	if err := decodeJSON(req, &payload); err != nil {
		errorJSON(w, "invalid JSON body", http.StatusBadRequest)
		return nil
	}
	targetProjectID := strings.TrimSpace(payload.TargetProjectID)
	if targetProjectID == "" {
		errorJSON(w, "targetProjectId required", http.StatusBadRequest)
		return nil
	}
	if s.rejectInsufficientHeadroom(w) {
		return nil
	}
	targetRoute := ProjectRoute{ID: targetProjectID, Name: projectName(targetProjectID)}

	started := time.Now()
	terminated, err := s.containers.TerminateContainer(route.Name, "project_clone_source_quiesce")
	if err != nil {
		return err
	}
	if err := s.workspaces.CloneReflink(route.Name, targetRoute.Name); err != nil {
		if errors.Is(err, workspace.ErrReflinkCloneUnavailable) {
			errorJSON(w, err.Error(), http.StatusPreconditionFailed)
			return nil
		}
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"sourceProjectId": route.ID,
		"targetProjectId": targetProjectID,
		"sourceStopped":   terminated,
		"durationMs":      time.Since(started).Milliseconds(),
	})
	return nil
}

func parseProjectRoute(path string) (ProjectRoute, bool) {
	matches := projectRouteRegex.FindStringSubmatch(path)
	if len(matches) == 0 {
		return ProjectRoute{}, false
	}
	projectID, err := url.PathUnescape(matches[1])
	if err != nil {
		return ProjectRoute{}, false
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return ProjectRoute{}, false
	}
	return ProjectRoute{ID: projectID, Name: projectName(projectID), Subpath: matches[2]}, true
}

func (s *Server) trustedProjectProxyRouteFromSourceIP(sourceIP, subpath string) (trustedProxyRoute, int, error) {
	if strings.TrimSpace(sourceIP) == "" || isLoopbackSourceIP(sourceIP) {
		return trustedProxyRoute{}, http.StatusForbidden, errors.New("deploy proxy is only available from project containers")
	}
	if s.containers == nil {
		return trustedProxyRoute{}, http.StatusForbidden, errors.New("project container resolution unavailable")
	}
	caller, err := s.containers.ResolveContainerBySourceIP(sourceIP)
	if err != nil {
		return trustedProxyRoute{}, http.StatusInternalServerError, fmt.Errorf("resolve project container: %w", err)
	}
	if caller == nil {
		return trustedProxyRoute{}, http.StatusForbidden, errors.New("deploy proxy is only available from project containers")
	}
	projectID := strings.TrimSpace(caller.WorkspaceID)
	if projectID == "" {
		return trustedProxyRoute{}, http.StatusConflict, errors.New("project container is missing project id")
	}
	return trustedProxyRoute{
		Name:      caller.Name,
		ProjectID: projectID,
		Subpath:   subpath,
	}, http.StatusOK, nil
}

func projectName(projectID string) string {
	replacer := regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	safeID := replacer.ReplaceAllString(projectID, "_")
	raw := "project-" + safeID
	normalized := strings.ToLower(raw)
	normalized = regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(normalized, "-")
	normalized = regexp.MustCompile(`-+`).ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-")
	if normalized == "" {
		normalized = fmt.Sprintf("project-%d", time.Now().UnixMilli())
	}
	if len(normalized) > 63 {
		normalized = normalized[:63]
	}
	return normalized
}

func (s *Server) handleHostCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "project-runtime-service",
		"features": map[string]any{
			"projectApi":       true,
			"containerRuntime": "docker",
			"gvisor":           true,
			"xfsProjectQuotas": s.workspaces.ProjectQuotasEnabled(),
			"zfsStorage":       s.workspaces.UsesZFS(),
			"zfsClone":         s.workspaces.UsesZFS() && runtime.GOOS == "linux",
			"zfsBackups":       s.workspaces.UsesZFS() && runtime.GOOS == "linux",
			"reflinkClone":     !s.workspaces.UsesZFS() && runtime.GOOS == "linux",
			"genericProxy":     true,
			"localBackups":     true,
			"objectBackups":    s.objectStore != nil,
			"projectArchive":   s.objectStore != nil,
			"legacyMigration":  true,
			"mtls":             true,
			"bearerAuth":       true,
		},
		"storageDriver": s.workspaces.StorageDriver(),
		"routes": []string{
			"GET /v1/host/capabilities",
			"GET /v1/host/stats",
			"GET /v1/projects/:id",
			"POST /v1/projects/:id/exec",
			"GET|PUT|POST|DELETE /v1/projects/:id/fs/*",
			"POST /v1/projects/:id/clone",
			"GET|POST /v1/projects/:id/backups",
			"POST /v1/projects/:id/restore",
			"GET /v1/projects/:id/storage",
			"POST /v1/projects/:id/archive",
			"POST /v1/projects/:id/unarchive",
			"GET /v1/projects/:id/proxies",
		},
	})
}

func (s *Server) handleHostStats(w http.ResponseWriter, _ *http.Request) {
	stats, err := diskStats(s.workspaces.Root())
	if err != nil {
		errorJSON(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stats["reserveBytes"] = s.cfg.DiskReserveBytes
	if free, ok := stats["freeBytes"].(uint64); ok {
		stats["headroomOk"] = free >= uint64(s.cfg.DiskReserveBytes)
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) rejectInsufficientHeadroom(w http.ResponseWriter) bool {
	stats, err := diskStats(s.workspaces.Root())
	if err != nil {
		errorJSON(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	free, _ := stats["freeBytes"].(uint64)
	if free < uint64(s.cfg.DiskReserveBytes) {
		writeJSON(w, http.StatusInsufficientStorage, map[string]any{
			"error":        "Insufficient runtime disk headroom",
			"freeBytes":    free,
			"reserveBytes": s.cfg.DiskReserveBytes,
		})
		return true
	}
	return false
}

func diskStats(path string) (map[string]any, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, err
	}
	blockSize := uint64(st.Bsize)
	return map[string]any{
		"path":           path,
		"totalBytes":     st.Blocks * blockSize,
		"freeBytes":      st.Bavail * blockSize,
		"availableBytes": st.Bfree * blockSize,
		"files":          st.Files,
		"freeFiles":      st.Ffree,
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}
