package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/qaml-ai/project-runtime-service/internal/container"
	"github.com/qaml-ai/project-runtime-service/internal/fsops"
	"github.com/qaml-ai/project-runtime-service/internal/state"
	"github.com/qaml-ai/project-runtime-service/internal/workspace"
)

type WorkspaceRoute struct {
	Name        string
	OrgID       string
	WorkspaceID string
	Subpath     string
}

type Server struct {
	cfg        Config
	containers *container.Manager
	workspaces *workspace.Manager
	fs         *fsops.Manager
	usage      *state.UsageStore

	httpClient        *http.Client
	proxyCapabilities map[string]ProxyCapability
	objectStore       objectStore
	projectStates     *projectStateStore
}

func NewServer(cfg Config, containers *container.Manager, workspaces *workspace.Manager, fsManager *fsops.Manager, usageStore *state.UsageStore) *Server {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	s := &Server{
		cfg:               cfg,
		containers:        containers,
		workspaces:        workspaces,
		fs:                fsManager,
		usage:             usageStore,
		httpClient:        &http.Client{Transport: transport},
		proxyCapabilities: loadProxyCapabilities(cfg),
		projectStates:     newProjectStateStore(cfg.ProjectStateRoot),
	}
	objectStore, err := newObjectStore(cfg)
	if err != nil {
		log.Printf("[ProjectRuntime] object storage disabled: %v", err)
	} else {
		s.objectStore = objectStore
	}
	s.startArchiveSweeper()

	return s
}

func (s *Server) startArchiveSweeper() {
	if s.cfg.ArchiveAfter <= 0 || s.archiveStoreKind() != "object" || s.objectStore == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(s.cfg.ArchiveSweepInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := s.archiveInactiveProjects(); err != nil {
				log.Printf("[ProjectRuntime] archive sweep warning: %v", err)
			}
		}
	}()
}

func (s *Server) Handler() http.Handler {
	return s
}

func (s *Server) DockerProxyHandler() http.Handler {
	return http.HandlerFunc(s.serveDockerProxy)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	sourceIP := requestSourceIP(req)
	s.trace("request_start", map[string]any{
		"method":   req.Method,
		"pathname": req.URL.Path,
		"search":   req.URL.RawQuery,
		"sourceIp": sourceIP,
	})

	if req.URL.Path == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "sandbox-host"})
		return
	}

	if s.rejectUnauthenticatedControlRequest(w, req) {
		return
	}

	if req.URL.Path == "/v1/host/capabilities" && req.Method == http.MethodGet {
		s.handleHostCapabilities(w, req)
		return
	}
	if req.URL.Path == "/v1/host/stats" && req.Method == http.MethodGet {
		s.handleHostStats(w, req)
		return
	}

	if strings.HasPrefix(req.URL.Path, "/v1/projects/") {
		if err := s.handleProjectRoute(w, req); err != nil {
			log.Printf("[SandboxHost] project request error: %v", err)
			errorJSON(w, fmt.Sprintf("Internal error: %v", err), http.StatusInternalServerError)
		}
		return
	}

	// Usage/spend endpoints (org-scoped, control port only).
	if strings.HasPrefix(req.URL.Path, "/v1/usage/") {
		s.handleUsageRoute(w, req)
		return
	}

	if strings.HasPrefix(req.URL.Path, "/v1/virtual-ai/") {
		errorJSON(w, "Host-side virtual AI has been removed", http.StatusGone)
		return
	}

	route, ok := parseWorkspaceRoute(req.URL.Path)
	if !ok {
		errorJSON(w, "Not found", http.StatusNotFound)
		return
	}

	if isCloudflareAPIProxyRoute(route.Subpath) {
		if s.rejectCloudflareAPIProxyCaller(w, req, route, sourceIP) {
			return
		}
	} else if s.rejectControlRouteFromSandboxCaller(w, req, route, sourceIP) {
		return
	}

	s.containers.TouchContainer(route.Name, fmt.Sprintf("workspace_request:%s:%s", req.Method, route.Subpath))

	if err := s.handleWorkspaceRoute(w, req, route); err != nil {
		log.Printf("[SandboxHost] request error: %v", err)
		s.trace("request_error", map[string]any{
			"method":   req.Method,
			"pathname": req.URL.Path,
			"sourceIp": sourceIP,
			"error":    err.Error(),
		})
		errorJSON(w, fmt.Sprintf("Internal error: %v", err), http.StatusInternalServerError)
	}
}

func (s *Server) serveDockerProxy(w http.ResponseWriter, req *http.Request) {
	sourceIP := requestSourceIP(req)
	if strings.HasPrefix(req.URL.Path, "/p/") {
		if err := s.forwardGenericProxyRequest(w, req, sourceIP); err != nil {
			log.Printf("[SandboxHost] generic proxy request error: %v", err)
			errorJSON(w, fmt.Sprintf("Internal error: %v", err), http.StatusInternalServerError)
		}
		return
	}

	route, ok := parseWorkspaceRoute(req.URL.Path)
	if !ok || !isCloudflareAPIProxyRoute(route.Subpath) {
		errorJSON(w, "Not found", http.StatusNotFound)
		return
	}

	if s.rejectCloudflareAPIProxyCaller(w, req, route, sourceIP) {
		return
	}

	s.containers.TouchContainer(route.Name, fmt.Sprintf("workspace_cf_api_proxy:%s:%s", req.Method, route.Subpath))
	if err := s.forwardCloudflareAPIProxyRequest(w, req, route); err != nil {
		log.Printf("[SandboxHost] docker proxy request error: %v", err)
		errorJSON(w, fmt.Sprintf("Internal error: %v", err), http.StatusInternalServerError)
	}
}

func (s *Server) rejectControlRouteFromSandboxCaller(
	w http.ResponseWriter,
	req *http.Request,
	route WorkspaceRoute,
	sourceIP string,
) bool {
	if strings.TrimSpace(sourceIP) == "" || isLoopbackSourceIP(sourceIP) {
		return false
	}

	caller, err := s.containers.ResolveContainerBySourceIP(sourceIP)
	if err != nil {
		s.trace("control_request_caller_resolution_error", map[string]any{
			"method":        req.Method,
			"pathname":      req.URL.Path,
			"sourceIp":      sourceIP,
			"targetSandbox": route.Name,
			"error":         err.Error(),
		})
		errorJSON(w, "Caller resolution failed", http.StatusInternalServerError)
		return true
	}
	if caller == nil {
		return false
	}

	s.trace("control_request_rejected_container_source", map[string]any{
		"method":          req.Method,
		"pathname":        req.URL.Path,
		"sourceIp":        sourceIP,
		"targetSandbox":   route.Name,
		"callerSandbox":   caller.Name,
		"callerWorkspace": caller.WorkspaceID,
		"targetWorkspace": route.WorkspaceID,
	})
	errorJSON(w, "Sandbox containers cannot access sandbox-host control APIs", http.StatusForbidden)
	return true
}

func (s *Server) rejectCloudflareAPIProxyCaller(
	w http.ResponseWriter,
	req *http.Request,
	route WorkspaceRoute,
	sourceIP string,
) bool {
	if strings.TrimSpace(sourceIP) == "" || isLoopbackSourceIP(sourceIP) {
		return false
	}
	if s.containers == nil {
		errorJSON(w, "Sandbox caller validation unavailable", http.StatusForbidden)
		return true
	}
	caller, err := s.containers.ResolveContainerBySourceIP(sourceIP)
	if err != nil {
		s.trace("cf_api_proxy_caller_resolution_error", map[string]any{
			"method":        req.Method,
			"pathname":      req.URL.Path,
			"sourceIp":      sourceIP,
			"targetSandbox": route.Name,
			"error":         err.Error(),
		})
		errorJSON(w, "Caller resolution failed", http.StatusInternalServerError)
		return true
	}
	if caller == nil {
		errorJSON(w, "Cloudflare API proxy is only available from the workspace sandbox", http.StatusForbidden)
		return true
	}

	sameSandbox := caller.Name == route.Name
	sameWorkspace := caller.WorkspaceID == "" || caller.WorkspaceID == route.WorkspaceID
	sameOrg := caller.OrgID == "" || caller.OrgID == route.OrgID
	if !sameSandbox || !sameWorkspace || !sameOrg {
		s.trace("cf_api_proxy_rejected_container_source", map[string]any{
			"method":          req.Method,
			"pathname":        req.URL.Path,
			"sourceIp":        sourceIP,
			"targetSandbox":   route.Name,
			"callerSandbox":   caller.Name,
			"callerWorkspace": caller.WorkspaceID,
			"targetWorkspace": route.WorkspaceID,
		})
		errorJSON(w, "Sandbox cannot proxy Cloudflare API requests for another workspace", http.StatusForbidden)
		return true
	}
	return false
}

func (s *Server) handleWorkspaceRoute(w http.ResponseWriter, req *http.Request, route WorkspaceRoute) error {
	name := route.Name
	opts := container.EnsureContainerOptions{OrgID: route.OrgID, WorkspaceID: route.WorkspaceID}

	if route.Subpath == "" && req.Method == http.MethodDelete {
		success, err := s.containers.TerminateContainer(name, "workspace_purge")
		if err != nil {
			return err
		}
		if err := s.workspaces.Delete(name); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "terminated": success})
		return nil
	}

	if route.Subpath == "/terminate" && req.Method == http.MethodPost {
		success, err := s.containers.TerminateContainer(name, "explicit_terminate_route")
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": success})
		return nil
	}

	if strings.HasPrefix(route.Subpath, "/fs/") || route.Subpath == "/chat/messages" {
		if _, err := s.workspaces.Ensure(name); err != nil {
			return err
		}
	}

	switch {
	case route.Subpath == "/fs/read" && req.Method == http.MethodGet:
		return s.handleFSRead(w, req, route)
	case route.Subpath == "/fs/write" && req.Method == http.MethodPut:
		return s.handleFSWrite(w, req, name)
	case route.Subpath == "/fs/list" && req.Method == http.MethodGet:
		return s.handleFSList(w, req, name)
	case route.Subpath == "/fs/delete" && req.Method == http.MethodDelete:
		return s.handleFSDelete(w, req, name)
	case route.Subpath == "/fs/move" && req.Method == http.MethodPost:
		return s.handleFSMove(w, req, name)
	case route.Subpath == "/fs/mkdir" && req.Method == http.MethodPost:
		return s.handleFSMkdir(w, req, name)
	case route.Subpath == "/fs/exists" && req.Method == http.MethodGet:
		return s.handleFSExists(w, req, name)
	case route.Subpath == "/exec" && req.Method == http.MethodPost:
		return s.handleExec(w, req, name, opts)
	case route.Subpath == "/chat/messages" && req.Method == http.MethodGet:
		return s.handleChatMessages(w, req, name)
	case strings.HasPrefix(route.Subpath, "/data-proxy/"):
		return s.forwardDataProxyRequest(w, req, route)
	case isCloudflareAPIProxyRoute(route.Subpath):
		return s.forwardCloudflareAPIProxyRequest(w, req, route)
	case route.Subpath == "/health" && req.Method == http.MethodGet:
		if _, err := s.containers.EnsureContainer(name, opts); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "status": "ok"})
		return nil
	default:
		errorJSON(w, "Not found", http.StatusNotFound)
		return nil
	}
}

func (s *Server) handleChatMessages(w http.ResponseWriter, req *http.Request, name string) error {
	threadID := strings.TrimSpace(req.URL.Query().Get("threadId"))
	if threadID == "" {
		errorJSON(w, "threadId query param required", http.StatusBadRequest)
		return nil
	}
	if strings.ContainsAny(threadID, `/\`) {
		errorJSON(w, "invalid threadId", http.StatusBadRequest)
		return nil
	}

	claudeSessionID := strings.TrimSpace(req.URL.Query().Get("claudeSessionId"))
	if strings.ContainsAny(claudeSessionID, `/\`) {
		errorJSON(w, "invalid claudeSessionId", http.StatusBadRequest)
		return nil
	}

	codexSessionID := strings.TrimSpace(req.URL.Query().Get("codexSessionId"))
	if strings.ContainsAny(codexSessionID, `/\`) {
		errorJSON(w, "invalid codexSessionId", http.StatusBadRequest)
		return nil
	}

	started := time.Now()
	s.containers.AddInFlightRequest(name, "chat_messages")
	defer func() {
		s.containers.RemoveInFlightRequest(name, "chat_messages", http.StatusOK, time.Since(started).Milliseconds())
	}()

	if messages, err := readHostPiSessionMessages(s.cfg.HostPiSessionRoot, threadID); err != nil {
		log.Printf("[SandboxHost] host Pi message history unavailable thread=%s sessionRoot=%s: %v", threadID, s.cfg.HostPiSessionRoot, err)
	} else if len(messages) > 0 {
		log.Printf("[SandboxHost] chat messages loaded from host Pi thread=%s messages=%d", threadID, len(messages))
		writeJSON(w, http.StatusOK, map[string]any{
			"success":  true,
			"messages": messages,
		})
		return nil
	} else {
		log.Printf("[SandboxHost] host Pi message history empty thread=%s sessionRoot=%s; checking legacy history", threadID, s.cfg.HostPiSessionRoot)
	}

	sessionIDs, err := legacyClaudeSessionCandidates(threadID, claudeSessionID)
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadRequest)
		return nil
	}
	log.Printf("[SandboxHost] chat messages scanning Claude legacy history thread=%s container=%s claudeSession=%s candidateSessions=%d", threadID, name, claudeSessionID, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		jsonlPath := fmt.Sprintf("/home/claude/.claude/projects/-home-claude/%s.jsonl", sessionID)
		info, err := s.fs.ReadInfo(name, jsonlPath)
		if err != nil {
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "no such file") || strings.Contains(lower, "not exist") {
				log.Printf("[SandboxHost] chat messages Claude candidate missing thread=%s session=%s path=%s", threadID, sessionID, jsonlPath)
				continue
			}
			log.Printf("[SandboxHost] chat messages Claude candidate stat failed thread=%s session=%s path=%s: %v", threadID, sessionID, jsonlPath, err)
			return s.handleFSError(w, err, "Chat messages unavailable")
		}

		file, err := os.Open(info.HostPath)
		if err != nil {
			if os.IsNotExist(err) {
				log.Printf("[SandboxHost] chat messages Claude candidate host file missing thread=%s session=%s containerPath=%s hostPath=%s", threadID, sessionID, jsonlPath, info.HostPath)
				continue
			}
			log.Printf("[SandboxHost] chat messages Claude candidate open failed thread=%s session=%s containerPath=%s hostPath=%s: %v", threadID, sessionID, jsonlPath, info.HostPath, err)
			return err
		}
		defer file.Close()

		content, err := io.ReadAll(file)
		if err != nil {
			log.Printf("[SandboxHost] chat messages Claude candidate read failed thread=%s session=%s containerPath=%s hostPath=%s: %v", threadID, sessionID, jsonlPath, info.HostPath, err)
			return err
		}
		messages := parseClaudeJSONLMessages(string(content), threadID)
		if len(messages) == 0 {
			log.Printf("[SandboxHost] chat messages Claude candidate parsed empty thread=%s session=%s path=%s bytes=%d", threadID, sessionID, jsonlPath, len(content))
			continue
		}

		log.Printf("[SandboxHost] chat messages loaded from Claude legacy thread=%s session=%s path=%s bytes=%d messages=%d", threadID, sessionID, jsonlPath, len(content), len(messages))
		writeJSON(w, http.StatusOK, map[string]any{
			"success":  true,
			"messages": messages,
		})
		return nil
	}

	codexThreadPaths, err := legacyCodexStatePathCandidates(threadID, codexSessionID)
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadRequest)
		return nil
	}
	log.Printf("[SandboxHost] chat messages scanning Codex legacy history thread=%s container=%s codexSession=%s candidatePaths=%d", threadID, name, codexSessionID, len(codexThreadPaths))
	for _, codexThreadPath := range codexThreadPaths {
		info, err := s.fs.ReadInfo(name, codexThreadPath)
		if err != nil {
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "no such file") || strings.Contains(lower, "not exist") {
				log.Printf("[SandboxHost] chat messages Codex candidate missing thread=%s path=%s", threadID, codexThreadPath)
				continue
			}
			log.Printf("[SandboxHost] chat messages Codex candidate stat failed thread=%s path=%s: %v", threadID, codexThreadPath, err)
			return s.handleFSError(w, err, "Chat messages unavailable")
		}
		if messages, err := readCodexStateMessages(req.Context(), info.HostPath, threadID, codexSessionID); err != nil {
			log.Printf("[SandboxHost] chat messages Codex candidate read failed thread=%s path=%s hostPath=%s codexSession=%s: %v", threadID, codexThreadPath, info.HostPath, codexSessionID, err)
		} else if len(messages) > 0 {
			log.Printf("[SandboxHost] chat messages loaded from Codex legacy thread=%s path=%s hostPath=%s codexSession=%s messages=%d", threadID, codexThreadPath, info.HostPath, codexSessionID, len(messages))
			writeJSON(w, http.StatusOK, map[string]any{
				"success":  true,
				"messages": messages,
			})
			return nil
		} else {
			log.Printf("[SandboxHost] chat messages Codex candidate parsed empty thread=%s path=%s hostPath=%s codexSession=%s", threadID, codexThreadPath, info.HostPath, codexSessionID)
		}
	}

	log.Printf("[SandboxHost] chat messages found no history thread=%s container=%s claudeSession=%s codexSession=%s", threadID, name, claudeSessionID, codexSessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"messages": []parsedChatMessage{},
	})
	return nil
}

func (s *Server) forwardCloudflareAPIProxyRequest(w http.ResponseWriter, req *http.Request, route WorkspaceRoute) error {
	base := strings.TrimRight(strings.TrimSpace(s.cfg.WorkerBaseURL), "/")
	if base == "" {
		errorJSON(w, "Cloudflare API proxy upstream not configured", http.StatusServiceUnavailable)
		return nil
	}
	secret := strings.TrimSpace(s.cfg.SandboxProxySecret)
	if secret == "" {
		errorJSON(w, "Cloudflare API proxy auth not configured", http.StatusServiceUnavailable)
		return nil
	}

	targetURL := base + route.Subpath
	if req.URL.RawQuery != "" {
		targetURL += "?" + req.URL.RawQuery
	}

	forwardReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, req.Body)
	if err != nil {
		return err
	}
	copyHeaders(forwardReq.Header, req.Header)
	forwardReq.Header.Set("X-Sandbox-Secret", secret)
	forwardReq.Header.Set("X-Chiridion-Org-Id", route.OrgID)
	forwardReq.Header.Set("X-Chiridion-Workspace-Id", route.WorkspaceID)

	resp, err := s.httpClient.Do(forwardReq)
	if err != nil {
		errorJSON(w, "Cloudflare API proxy upstream unavailable", http.StatusServiceUnavailable)
		return nil
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if err := copyResponseBody(w, resp.Body); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) forwardDataProxyRequest(w http.ResponseWriter, req *http.Request, route WorkspaceRoute) error {
	base := strings.TrimRight(strings.TrimSpace(s.cfg.DataProxyUpstreamURL), "/")
	if base == "" {
		errorJSON(w, "Data proxy upstream not configured", http.StatusServiceUnavailable)
		return nil
	}

	targetURL := base + route.Subpath
	if req.URL.RawQuery != "" {
		targetURL += "?" + req.URL.RawQuery
	}

	forwardReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, req.Body)
	if err != nil {
		return err
	}
	copyHeaders(forwardReq.Header, req.Header)
	forwardReq.Header.Set("X-Chiridion-Org-Id", route.OrgID)
	forwardReq.Header.Set("X-Chiridion-Workspace-Id", route.WorkspaceID)

	resp, err := s.httpClient.Do(forwardReq)
	if err != nil {
		errorJSON(w, "Data proxy upstream unavailable", http.StatusServiceUnavailable)
		return nil
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if err := copyResponseBody(w, resp.Body); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) handleFSRead(w http.ResponseWriter, req *http.Request, route WorkspaceRoute) error {
	path := req.URL.Query().Get("path")
	if strings.TrimSpace(path) == "" {
		errorJSON(w, "path query param required", http.StatusBadRequest)
		return nil
	}

	// Resolve /mnt/user-outputs/ and /mnt/user-uploads/ to host R2 FUSE paths.
	if hostPath, ok := s.containers.ResolveR2HostPath(route.Name, path); ok {
		return s.serveHostFile(w, hostPath)
	}
	if hostPath, ok := s.resolveLegacyR2MountPath(path, route.OrgID, route.WorkspaceID); ok {
		return s.serveHostFile(w, hostPath)
	}

	info, err := s.fs.ReadInfo(route.Name, path)
	if err != nil {
		return s.handleFSError(w, err, "File not found")
	}

	return s.serveHostFile(w, info.HostPath)
}

// resolveLegacyR2MountPath checks if a sandbox path targets /mnt/user-outputs/ or
// /mnt/user-uploads/ and returns the legacy global host R2 FUSE path.
// Returns ("", false) if the path doesn't match or is invalid.
func (s *Server) resolveLegacyR2MountPath(sandboxPath, orgID, workspaceID string) (string, bool) {
	for _, mountDir := range []string{"user-outputs", "user-uploads"} {
		prefix := "/mnt/" + mountDir + "/"
		if !strings.HasPrefix(sandboxPath, prefix) {
			continue
		}
		subpath := strings.TrimPrefix(sandboxPath, prefix)
		cleaned := filepath.Clean(subpath)
		// Reject traversal: cleaned must stay within the mount subtree
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") || filepath.IsAbs(cleaned) {
			return "", false
		}
		hostPath := filepath.Join("/mnt/r2", orgID, workspaceID, mountDir, cleaned)
		return hostPath, true
	}
	return "", false
}

func (s *Server) serveHostFile(w http.ResponseWriter, hostPath string) error {
	stat, err := os.Stat(hostPath)
	if err != nil {
		if os.IsNotExist(err) {
			errorJSON(w, "File not found", http.StatusNotFound)
			return nil
		}
		return err
	}

	ext := filepath.Ext(hostPath)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	file, err := os.Open(hostPath)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(w, file)
	return err
}

func (s *Server) handleFSWrite(w http.ResponseWriter, req *http.Request, name string) error {
	path := req.URL.Query().Get("path")
	if strings.TrimSpace(path) == "" {
		errorJSON(w, "path query param required", http.StatusBadRequest)
		return nil
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	if err := s.fs.Write(name, path, data); err != nil {
		return s.handleFSError(w, err, "Write failed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
	return nil
}

func (s *Server) handleFSList(w http.ResponseWriter, req *http.Request, name string) error {
	path := req.URL.Query().Get("path")
	if strings.TrimSpace(path) == "" {
		path = "/"
	}
	recursive := parseBoolQuery(req.URL.Query().Get("recursive"), false)
	includeHidden := parseBoolQuery(req.URL.Query().Get("includeHidden"), true)

	files, err := s.fs.List(name, path, fsops.ListOptions{
		Recursive:     recursive,
		IncludeHidden: includeHidden,
	})
	if err != nil {
		return s.handleFSError(w, err, "Path not found")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files":     files,
		"count":     len(files),
		"path":      path,
		"recursive": recursive,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	})
	return nil
}

func parseBoolQuery(raw string, defaultValue bool) bool {
	value := strings.TrimSpace(raw)
	if value == "" {
		return defaultValue
	}
	switch strings.ToLower(value) {
	case "1", "true":
		return true
	case "0", "false":
		return false
	default:
		return defaultValue
	}
}

func (s *Server) handleFSDelete(w http.ResponseWriter, req *http.Request, name string) error {
	var payload struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := decodeJSON(req, &payload); err != nil {
		errorJSON(w, "invalid JSON body", http.StatusBadRequest)
		return nil
	}
	if strings.TrimSpace(payload.Path) == "" {
		errorJSON(w, "path required", http.StatusBadRequest)
		return nil
	}
	if err := s.fs.Delete(name, payload.Path, payload.Recursive); err != nil {
		return s.handleFSError(w, err, "Delete failed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
	return nil
}

func (s *Server) handleFSMove(w http.ResponseWriter, req *http.Request, name string) error {
	var payload struct {
		Source string `json:"source"`
		Dest   string `json:"dest"`
	}
	if err := decodeJSON(req, &payload); err != nil {
		errorJSON(w, "invalid JSON body", http.StatusBadRequest)
		return nil
	}
	if strings.TrimSpace(payload.Source) == "" || strings.TrimSpace(payload.Dest) == "" {
		errorJSON(w, "source and dest required", http.StatusBadRequest)
		return nil
	}
	if err := s.fs.Move(name, payload.Source, payload.Dest); err != nil {
		return s.handleFSError(w, err, "Move failed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "timestamp": time.Now().UTC().Format(time.RFC3339Nano)})
	return nil
}

func (s *Server) handleFSMkdir(w http.ResponseWriter, req *http.Request, name string) error {
	path := req.URL.Query().Get("path")
	if strings.TrimSpace(path) == "" {
		errorJSON(w, "path query param required", http.StatusBadRequest)
		return nil
	}
	if err := s.fs.Mkdir(name, path); err != nil {
		return s.handleFSError(w, err, "mkdir failed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "timestamp": time.Now().UTC().Format(time.RFC3339Nano)})
	return nil
}

func (s *Server) handleFSExists(w http.ResponseWriter, req *http.Request, name string) error {
	path := req.URL.Query().Get("path")
	if strings.TrimSpace(path) == "" {
		errorJSON(w, "path query param required", http.StatusBadRequest)
		return nil
	}
	result, err := s.fs.Exists(name, path)
	if err != nil {
		return s.handleFSError(w, err, "exists failed")
	}
	writeJSON(w, http.StatusOK, result)
	return nil
}

func (s *Server) handleExec(w http.ResponseWriter, req *http.Request, name string, opts container.EnsureContainerOptions) error {
	var body container.ExecRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		errorJSON(w, "Invalid JSON body", http.StatusBadRequest)
		return nil
	}
	if _, err := s.containers.EnsureContainer(name, opts); err != nil {
		return err
	}
	started := time.Now()
	s.containers.AddInFlightRequest(name, "container_exec")
	defer func() {
		s.containers.RemoveInFlightRequest(name, "container_exec", http.StatusOK, time.Since(started).Milliseconds())
	}()
	result, err := s.containers.Exec(req.Context(), name, opts, body)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, result)
	return nil
}

func (s *Server) handleFSError(w http.ResponseWriter, err error, fallback string) error {
	message := err.Error()
	lower := strings.ToLower(message)
	if strings.Contains(lower, "traversal") {
		errorJSON(w, message, http.StatusForbidden)
		return nil
	}
	if strings.Contains(lower, "no such file") || strings.Contains(lower, "not exist") {
		errorJSON(w, fallback, http.StatusNotFound)
		return nil
	}
	return err
}

func parseWorkspaceRoute(path string) (WorkspaceRoute, bool) {
	matches := workspaceRouteRegex.FindStringSubmatch(path)
	if len(matches) == 0 {
		return WorkspaceRoute{}, false
	}
	orgID, err := url.PathUnescape(matches[1])
	if err != nil {
		return WorkspaceRoute{}, false
	}
	workspaceID, err := url.PathUnescape(matches[2])
	if err != nil {
		return WorkspaceRoute{}, false
	}
	return WorkspaceRoute{
		Name:        sandboxName(workspaceID),
		OrgID:       orgID,
		WorkspaceID: workspaceID,
		Subpath:     matches[3],
	}, true
}

var workspaceRouteRegex = regexp.MustCompile(`^/v1/workspaces/([^/]+)/([^/]+)(/.*)?$`)

func isCloudflareAPIProxyRoute(subpath string) bool {
	return subpath == "/client/v4" || strings.HasPrefix(subpath, "/client/v4/")
}

func sandboxName(workspaceID string) string {
	replacer := regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	safeID := replacer.ReplaceAllString(workspaceID, "_")
	raw := "chiridion-ws-" + safeID

	normalized := strings.ToLower(raw)
	normalized = regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(normalized, "-")
	normalized = regexp.MustCompile(`-+`).ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-")
	if normalized == "" {
		normalized = fmt.Sprintf("chiridion-%d", time.Now().UnixMilli())
	}
	if len(normalized) > 63 {
		normalized = normalized[:63]
	}
	return normalized
}

func requestSourceIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(req.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(req.RemoteAddr)
	}
	return host
}

func isLoopbackSourceIP(sourceIP string) bool {
	ip := net.ParseIP(strings.TrimSpace(sourceIP))
	if ip == nil {
		return false
	}
	if mapped := ip.To4(); mapped != nil {
		return mapped.IsLoopback()
	}
	return ip.IsLoopback()
}

func copyResponseBody(w http.ResponseWriter, body io.Reader) error {
	if w == nil || body == nil {
		return nil
	}

	writer := io.Writer(w)
	if flusher, ok := w.(http.Flusher); ok {
		writer = &flushWriter{writer: w, flusher: flusher}
	}

	_, err := io.Copy(writer, body)
	return err
}

type flushWriter struct {
	writer  io.Writer
	flusher http.Flusher
}

func (w *flushWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.flusher.Flush()
	}
	return n, err
}

func randomID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if isInternalProxyHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isInternalProxyHeader(key string) bool {
	// Miniflare injects MF-* headers for local service-binding proxying. They
	// are not valid user/Worker headers and must not be replayed downstream.
	return strings.HasPrefix(strings.ToLower(key), "mf-")
}

func decodeJSON(req *http.Request, target any) error {
	decoder := json.NewDecoder(req.Body)
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func errorJSON(w http.ResponseWriter, message string, status int) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) trace(event string, details map[string]any) {
	if !s.cfg.TraceSandboxHost {
		return
	}
	log.Printf("[SandboxHost][trace] %s %+v", event, details)
}
