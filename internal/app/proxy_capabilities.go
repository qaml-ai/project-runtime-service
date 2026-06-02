package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type ProxyCapability struct {
	Name            string            `json:"name"`
	Target          string            `json:"target"`
	BearerToken     string            `json:"bearerToken,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	AllowedProjects []string          `json:"allowedProjects,omitempty"`
}

type proxyCapabilityConfig struct {
	Capabilities []ProxyCapability `json:"capabilities"`
}

const camelArtifactsProxyCapabilityName = "camelai-artifacts"

func loadProxyCapabilities(cfg Config) map[string]ProxyCapability {
	raw := strings.TrimSpace(cfg.ProxyCapabilitiesJSON)
	if raw == "" && strings.TrimSpace(cfg.ProxyCapabilitiesFile) != "" {
		content, err := os.ReadFile(cfg.ProxyCapabilitiesFile)
		if err != nil {
			log.Printf("[SandboxHost] failed to read proxy capability file %s: %v", cfg.ProxyCapabilitiesFile, err)
			return defaultProxyCapabilities(cfg)
		}
		raw = string(content)
	}
	if raw == "" {
		return defaultProxyCapabilities(cfg)
	}

	var parsed proxyCapabilityConfig
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Printf("[SandboxHost] failed to parse proxy capabilities: %v", err)
		return defaultProxyCapabilities(cfg)
	}
	out := make(map[string]ProxyCapability, len(parsed.Capabilities))
	for _, capability := range parsed.Capabilities {
		capability.Name = strings.TrimSpace(capability.Name)
		capability.Target = strings.TrimRight(strings.TrimSpace(capability.Target), "/")
		if capability.Name == "" || capability.Target == "" {
			continue
		}
		out[capability.Name] = capability
	}
	for name, capability := range defaultProxyCapabilities(cfg) {
		if _, exists := out[name]; !exists {
			out[name] = capability
		}
	}
	return out
}

func defaultProxyCapabilities(cfg Config) map[string]ProxyCapability {
	workerBaseURL := strings.TrimRight(strings.TrimSpace(cfg.WorkerBaseURL), "/")
	secret := strings.TrimSpace(cfg.ProjectRuntimeProxySecret)
	if workerBaseURL == "" || secret == "" {
		return map[string]ProxyCapability{}
	}
	return map[string]ProxyCapability{
		camelArtifactsProxyCapabilityName: {
			Name:   camelArtifactsProxyCapabilityName,
			Target: workerBaseURL + "/api/internal/project-runtime/artifacts",
			Headers: map[string]string{
				"X-Project-Runtime-Secret": secret,
			},
		},
	}
}

func (s *Server) handleProjectProxiesList(w http.ResponseWriter, _ *http.Request, route ProjectRoute) error {
	items := make([]map[string]any, 0, len(s.proxyCapabilities))
	for _, capability := range s.proxyCapabilities {
		if !capabilityAllowsProject(capability, route.Name, route.ID) {
			continue
		}
		items = append(items, map[string]any{
			"name":   capability.Name,
			"target": capability.Target,
			"url":    "/p/" + capability.Name + "/",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projectId": route.ID, "proxies": items})
	return nil
}

func (s *Server) forwardGenericProxyRequest(w http.ResponseWriter, req *http.Request, sourceIP string) error {
	capabilityName, suffix, ok := parseGenericProxyPath(req.URL.Path)
	if !ok {
		errorJSON(w, "Not found", http.StatusNotFound)
		return nil
	}
	capability, ok := s.proxyCapabilities[capabilityName]
	if !ok {
		errorJSON(w, "Proxy capability not found", http.StatusNotFound)
		return nil
	}
	caller, err := s.containers.ResolveContainerBySourceIP(sourceIP)
	if err != nil {
		return fmt.Errorf("resolve proxy caller: %w", err)
	}
	if caller == nil {
		errorJSON(w, "Proxy capabilities are only available from project containers", http.StatusForbidden)
		return nil
	}
	if !capabilityAllowsProject(capability, caller.Name, caller.WorkspaceID) {
		errorJSON(w, "Proxy capability is not attached to this project", http.StatusForbidden)
		return nil
	}

	targetURL, err := buildProxyTargetURL(capability.Target, suffix, req.URL.RawQuery)
	if err != nil {
		errorJSON(w, err.Error(), http.StatusBadGateway)
		return nil
	}

	forwardReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, req.Body)
	if err != nil {
		return err
	}
	copyProxyHeaders(forwardReq.Header, req.Header)
	for key, value := range capability.Headers {
		forwardReq.Header.Set(key, value)
	}
	if strings.TrimSpace(capability.BearerToken) != "" {
		forwardReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(capability.BearerToken))
	}
	forwardReq.Header.Set("X-Project-Runtime-Project", caller.Name)
	if caller.WorkspaceID != "" {
		forwardReq.Header.Set("X-Project-Runtime-Workspace", caller.WorkspaceID)
	}

	resp, err := s.httpClient.Do(forwardReq)
	if err != nil {
		errorJSON(w, "Proxy upstream unavailable", http.StatusServiceUnavailable)
		return nil
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	return copyResponseBody(w, resp.Body)
}

func parseGenericProxyPath(path string) (string, string, bool) {
	trimmed := strings.TrimPrefix(path, "/p/")
	if trimmed == path || strings.TrimSpace(trimmed) == "" {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, "/", 2)
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return "", "", false
	}
	suffix := ""
	if len(parts) == 2 {
		suffix = "/" + parts[1]
	}
	return name, suffix, true
}

func buildProxyTargetURL(base, suffix, rawQuery string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid proxy target")
	}
	target := strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	return target, nil
}

func capabilityAllowsProject(capability ProxyCapability, projectRefs ...string) bool {
	if len(capability.AllowedProjects) == 0 {
		return true
	}
	for _, allowed := range capability.AllowedProjects {
		allowed = strings.TrimSpace(allowed)
		for _, ref := range projectRefs {
			if allowed == strings.TrimSpace(ref) {
				return true
			}
		}
	}
	return false
}

func copyProxyHeaders(dst, src http.Header) {
	for key, values := range src {
		if isInternalProxyHeader(key) || isSpoofableProjectHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isSpoofableProjectHeader(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	return strings.HasPrefix(normalized, "x-project-runtime-") ||
		strings.HasPrefix(normalized, "x-chiridion-") ||
		normalized == "x-sandbox-secret"
}
