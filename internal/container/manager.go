package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockernetwork "github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	dockererrdefs "github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	dockerunits "github.com/docker/go-units"
	"github.com/qaml-ai/project-runtime-service/internal/workspace"
)

type EnsureContainerOptions struct {
	OrgID       string
	WorkspaceID string
}

type ContainerRecord struct {
	Name             string
	ContainerID      string
	HostPort         int
	ContainerIP      string
	Status           string
	CreatedAt        int64
	LastAccessedAt   int64
	InFlightRequests int
	OrgID            string
	WorkspaceID      string
}

type ExecRequest struct {
	Cmd []string          `json:"cmd"`
	Cwd string            `json:"cwd"`
	Env map[string]string `json:"env,omitempty"`
}

type ExecResponse struct {
	Success  bool   `json:"success"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

const (
	labelOrgID                    = "com.chiridion.org-id"
	labelWorkspaceID              = "com.chiridion.workspace-id"
	labelRuntimeScope             = "com.qaml.project-runtime.scope"
	labelRuntimeProjectID         = "com.qaml.project-runtime.project-id"
	defaultContainerIdleTimeoutMS = 300_000
	minContainerIdleTimeout       = 10 * time.Second
)

type ensureWait struct {
	done chan struct{}
	rec  *ContainerRecord
	err  error
}

type Manager struct {
	workspaces *workspace.Manager
	docker     *dockerclient.Client

	workspacesRoot             string
	sandboxImage               string
	containerMemory            string
	containerCPUShares         string
	containerRuntime           string
	containerNamePrefix        string
	containerScope             string
	containerHTTPPort          int
	idleTimeout                time.Duration
	r2CredentialsRoot          string
	r2BucketName               string
	r2AccountID                string
	r2AccessKeyID              string
	r2SecretAccessKey          string
	r2TempCredentialTTLSeconds int
	healthPollInterval         time.Duration
	traceLifecycle             bool

	mu                sync.Mutex
	containers        map[string]*ContainerRecord
	containerIPIndex  map[string]string
	pendingWorkspaces map[string]int
	ensureInFlight    map[string]*ensureWait
	idleTimers        map[string]*time.Timer
}

func NewManager(workspaces *workspace.Manager) *Manager {
	docker, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Fatalf("[ContainerManager] failed to initialize Docker API client: %v", err)
	}

	workspacesRoot := envString("WORKSPACES_ROOT", defaultWorkspaceRoot())
	containerRuntime := envString("CONTAINER_RUNTIME", defaultContainerRuntime())

	m := &Manager{
		workspaces:                 workspaces,
		docker:                     docker,
		workspacesRoot:             workspacesRoot,
		sandboxImage:               envString("SANDBOX_IMAGE", "chiridion-sandbox:latest"),
		containerMemory:            envString("CONTAINER_MEMORY", "16g"),
		containerCPUShares:         envString("CONTAINER_CPU_SHARES", "2048"),
		containerRuntime:           containerRuntime,
		containerNamePrefix:        envString("CONTAINER_NAME_PREFIX", "prs-"),
		containerScope:             envString("CONTAINER_SCOPE", "project-runtime-service"),
		containerHTTPPort:          8080,
		idleTimeout:                containerIdleTimeoutFromEnv(),
		r2CredentialsRoot:          envString("R2_CREDENTIALS_ROOT", defaultR2CredentialsRoot()),
		r2BucketName:               envString("R2_BUCKET_NAME", ""),
		r2AccountID:                envString("R2_ACCOUNT_ID", ""),
		r2AccessKeyID:              envString("R2_ACCESS_KEY_ID", ""),
		r2SecretAccessKey:          envString("R2_SECRET_ACCESS_KEY", ""),
		r2TempCredentialTTLSeconds: envInt("R2_TEMP_CREDENTIAL_TTL_SECONDS", defaultR2TempCredentialTTLSeconds()),
		healthPollInterval:         maxDuration(10*time.Millisecond, time.Duration(envInt("HEALTH_POLL_INTERVAL_MS", 50))*time.Millisecond),
		traceLifecycle:             envString("TRACE_SANDBOX_LIFECYCLE", "") == "1",
		containers:                 make(map[string]*ContainerRecord),
		containerIPIndex:           make(map[string]string),
		pendingWorkspaces:          make(map[string]int),
		ensureInFlight:             make(map[string]*ensureWait),
		idleTimers:                 make(map[string]*time.Timer),
	}

	m.discoverRunningContainers()

	log.Printf("[ContainerManager] container idle timeout enabled (timeout=%ds)", int(m.idleTimeout/time.Second))
	return m
}

// NewTestManager returns a Manager suitable for tests that don't need Docker.
func NewTestManager() *Manager {
	return &Manager{
		containers:        make(map[string]*ContainerRecord),
		containerIPIndex:  make(map[string]string),
		pendingWorkspaces: make(map[string]int),
		ensureInFlight:    make(map[string]*ensureWait),
		idleTimers:        make(map[string]*time.Timer),
	}
}

func (m *Manager) TouchContainer(name, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.containers[name]
	if rec == nil {
		m.trace("touch_container_miss", map[string]any{"name": name, "reason": reason})
		return
	}
	rec.LastAccessedAt = nowMillis()
	m.resetIdleTimerLocked(name, rec)
	m.trace("touch_container", map[string]any{"reason": reason, "container": rec})
}

func (m *Manager) AddInFlightRequest(name, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.containers[name]
	if rec == nil {
		m.trace("inflight_request_open_missing_container", map[string]any{"name": name, "reason": reason})
		return
	}
	rec.InFlightRequests++
	rec.LastAccessedAt = nowMillis()
	m.resetIdleTimerLocked(name, rec)
	m.trace("inflight_request_open", map[string]any{"reason": reason, "container": rec})
}

func (m *Manager) RemoveInFlightRequest(name, reason string, status int, durationMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.containers[name]
	if rec == nil {
		m.trace("inflight_request_close_missing_container", map[string]any{"name": name, "reason": reason, "status": status, "durationMs": durationMs})
		return
	}
	if rec.InFlightRequests > 0 {
		rec.InFlightRequests--
	}
	rec.LastAccessedAt = nowMillis()
	m.resetIdleTimerLocked(name, rec)
	m.trace("inflight_request_close", map[string]any{"reason": reason, "status": status, "durationMs": durationMs, "container": rec})
}

func (m *Manager) resetIdleTimerLocked(name string, rec *ContainerRecord) {
	if m.idleTimeout <= 0 || rec == nil {
		return
	}
	if m.idleTimers == nil {
		m.idleTimers = make(map[string]*time.Timer)
	}
	if timer := m.idleTimers[name]; timer != nil {
		timer.Stop()
	}
	m.idleTimers[name] = time.AfterFunc(m.idleTimeout, func() {
		m.expireIdleContainer(name)
	})
}

func (m *Manager) clearIdleTimerLocked(name string) {
	if timer := m.idleTimers[name]; timer != nil {
		timer.Stop()
	}
	delete(m.idleTimers, name)
}

func (m *Manager) removeContainerRecordLocked(name string) {
	if current := m.containers[name]; current != nil && current.ContainerIP != "" {
		m.unindexContainerIPLocked(current.ContainerIP)
	}
	delete(m.containers, name)
	m.clearIdleTimerLocked(name)
}

func (m *Manager) expireIdleContainer(name string) {
	now := nowMillis()

	m.mu.Lock()
	current := m.containers[name]
	if current == nil {
		m.clearIdleTimerLocked(name)
		m.mu.Unlock()
		return
	}
	pending := m.pendingWorkspaces[name]
	inFlight := current.InFlightRequests
	lastAccessed := current.LastAccessedAt
	snapshot := copyRecord(current)

	if pending > 0 || inFlight > 0 || now-lastAccessed < m.idleTimeout.Milliseconds() {
		m.resetIdleTimerLocked(name, current)
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	idleSeconds := int((now - lastAccessed) / 1000)
	log.Printf("[ContainerManager] stopping idle container %s (idle=%ds)", name, idleSeconds)
	m.trace("idle_timeout_terminate", map[string]any{
		"name":   name,
		"idleMs": now - lastAccessed,
		"state":  snapshot,
	})
	_, _ = m.TerminateContainer(name, "idle_timeout")
}

func (m *Manager) ResolveContainerBySourceIP(sourceIP string) (*ContainerRecord, error) {
	for _, key := range sourceIPKeys(sourceIP) {
		if rec := m.getContainerBySourceIPCached(key); rec != nil {
			return copyRecord(rec), nil
		}
	}

	m.mu.Lock()
	names := make([]string, 0, len(m.containers))
	for name := range m.containers {
		names = append(names, name)
	}
	m.mu.Unlock()

	for _, name := range names {
		latestIP, _ := m.getContainerIP(name)
		if latestIP == "" {
			continue
		}

		m.mu.Lock()
		rec := m.containers[name]
		if rec == nil {
			m.mu.Unlock()
			continue
		}
		if rec.ContainerIP != "" && rec.ContainerIP != latestIP {
			m.unindexContainerIPLocked(rec.ContainerIP)
		}
		rec.ContainerIP = latestIP
		m.indexContainerIPLocked(latestIP, name)
		m.mu.Unlock()

		latestKeys := make(map[string]struct{})
		for _, key := range sourceIPKeys(latestIP) {
			latestKeys[key] = struct{}{}
		}
		for _, sourceKey := range sourceIPKeys(sourceIP) {
			if _, ok := latestKeys[sourceKey]; ok {
				m.mu.Lock()
				out := copyRecord(m.containers[name])
				m.mu.Unlock()
				if out != nil {
					return out, nil
				}
			}
		}
	}

	return nil, nil
}

func (m *Manager) EnsureContainer(name string, opts EnsureContainerOptions) (*ContainerRecord, error) {
	m.trace("ensure_container_request", map[string]any{"name": name, "opts": opts})

	for {
		m.mu.Lock()
		if inflight := m.ensureInFlight[name]; inflight != nil {
			m.mu.Unlock()
			<-inflight.done
			if inflight.err != nil {
				return nil, inflight.err
			}
			return copyRecord(inflight.rec), nil
		}

		wait := &ensureWait{done: make(chan struct{})}
		m.ensureInFlight[name] = wait
		m.pendingWorkspaces[name] = m.pendingWorkspaces[name] + 1
		m.mu.Unlock()

		rec, err := m.ensureContainerUnlocked(name, opts)

		m.mu.Lock()
		wait.rec = rec
		wait.err = err
		close(wait.done)
		delete(m.ensureInFlight, name)
		if m.pendingWorkspaces[name] <= 1 {
			delete(m.pendingWorkspaces, name)
		} else {
			m.pendingWorkspaces[name]--
		}
		m.mu.Unlock()

		if err != nil {
			return nil, err
		}
		return copyRecord(rec), nil
	}
}

func (m *Manager) ensureContainerUnlocked(name string, opts EnsureContainerOptions) (*ContainerRecord, error) {
	dockerName := m.dockerName(name)

	m.mu.Lock()
	cached := m.containers[name]
	m.mu.Unlock()

	if cached != nil {
		inspect, inspectErr := m.inspectContainer(dockerName, 30*time.Second)
		if inspectErr == nil && inspect.State != nil && inspect.State.Running {
			if !m.matchesConfiguredImage(inspect) {
				m.trace("ensure_container_image_mismatch_cached", map[string]any{
					"name":            name,
					"configuredImage": m.sandboxImage,
					"containerImage":  configuredImageFromInspect(inspect),
				})
				log.Printf("[ContainerManager] container %s image mismatch (have=%s want=%s); recreating", name, configuredImageFromInspect(inspect), m.sandboxImage)
				_ = m.removeContainerIfExists(dockerName, true)
				m.mu.Lock()
				m.removeContainerRecordLocked(name)
				m.mu.Unlock()
			} else {
				m.mu.Lock()
				if current := m.containers[name]; current != nil {
					current.LastAccessedAt = nowMillis()
					m.resetIdleTimerLocked(name, current)
					m.mu.Unlock()
					m.trace("ensure_container_cache_hit", map[string]any{"name": name, "container": current})
					return copyRecord(current), nil
				}
				m.mu.Unlock()
			}
		} else {
			m.mu.Lock()
			m.removeContainerRecordLocked(name)
			m.mu.Unlock()
		}
	}

	if inspect, err := m.inspectContainer(dockerName, 30*time.Second); err == nil && inspect.State != nil && inspect.State.Running {
		if !m.matchesConfiguredImage(inspect) {
			m.trace("ensure_container_image_mismatch_existing", map[string]any{
				"name":            name,
				"configuredImage": m.sandboxImage,
				"containerImage":  configuredImageFromInspect(inspect),
			})
			log.Printf("[ContainerManager] container %s image mismatch (have=%s want=%s); recreating", name, configuredImageFromInspect(inspect), m.sandboxImage)
			_ = m.removeContainerIfExists(dockerName, true)
		} else {
			port := hostPortFromInspect(inspect, m.containerHTTPPort)
			containerIP := containerIPFromInspect(inspect)
			if port > 0 {
				orgID := opts.OrgID
				workspaceID := opts.WorkspaceID
				if orgID == "" {
					orgID = labelFromInspect(inspect, labelOrgID)
				}
				if workspaceID == "" {
					workspaceID = labelFromInspect(inspect, labelWorkspaceID)
				}
				rec := &ContainerRecord{
					Name:             name,
					ContainerID:      dockerName,
					HostPort:         port,
					ContainerIP:      containerIP,
					Status:           "running",
					CreatedAt:        nowMillis(),
					LastAccessedAt:   nowMillis(),
					InFlightRequests: 0,
					OrgID:            orgID,
					WorkspaceID:      workspaceID,
				}
				m.mu.Lock()
				m.containers[name] = rec
				if containerIP != "" {
					m.indexContainerIPLocked(containerIP, name)
				}
				m.resetIdleTimerLocked(name, rec)
				m.mu.Unlock()
				log.Printf("[ContainerManager] reconnected to existing container %s (port=%d)", name, port)
				m.trace("ensure_container_reconnected_existing", map[string]any{"name": name, "container": rec})
				return copyRecord(rec), nil
			}
		}
	}

	_ = m.removeContainerIfExists(dockerName, true)

	if _, err := m.workspaces.Ensure(name); err != nil {
		return nil, err
	}
	wsPath := m.workspacePath(name)

	log.Printf("[ContainerManager] creating container %s", name)
	m.trace("ensure_container_create_begin", map[string]any{"name": name, "workspacePath": wsPath, "image": m.sandboxImage, "runtime": m.containerRuntime, "opts": opts})

	env := []string{
		"HOME=/home/claude",
		"USER=claude",
	}
	binds := []string{
		wsPath + ":/home/claude",
	}
	var capAdd []string
	var devices []dockercontainer.DeviceMapping

	var r2Config *containerR2Config
	if opts.OrgID != "" && opts.WorkspaceID != "" {
		cfg, err := m.prepareContainerR2Config(dockerName, opts.OrgID, opts.WorkspaceID)
		if err != nil {
			return nil, fmt.Errorf("prepare R2 config for %s: %w", name, err)
		}
		r2Config = cfg
		if r2Config != nil {
			binds = append(binds,
				r2Config.uploadsCredentialsFile+":/run/chiridion-r2/uploads.credentials:ro",
				r2Config.outputsCredentialsFile+":/run/chiridion-r2/outputs.credentials:ro",
			)
			capAdd = append(capAdd, "SYS_ADMIN")
			devices = append(devices, dockercontainer.DeviceMapping{
				PathOnHost:        "/dev/fuse",
				PathInContainer:   "/dev/fuse",
				CgroupPermissions: "rwm",
			})
			env = append(env,
				"R2_MOUNT_ENABLED=1",
				"R2_BUCKET_NAME="+m.r2BucketName,
				"R2_ACCOUNT_ID="+m.r2AccountID,
				"R2_UPLOADS_PREFIX="+r2Config.uploadsPrefix,
				"R2_OUTPUTS_PREFIX="+r2Config.outputsPrefix,
				"R2_UPLOADS_CREDENTIALS_FILE=/run/chiridion-r2/uploads.credentials",
				"R2_OUTPUTS_CREDENTIALS_FILE=/run/chiridion-r2/outputs.credentials",
			)
			log.Printf("[ContainerManager] in-container R2 mounts configured for %s/%s", opts.OrgID, opts.WorkspaceID)
		}
	}

	memoryBytes := int64(0)
	if parsed, parseErr := dockerunits.RAMInBytes(m.containerMemory); parseErr == nil {
		memoryBytes = parsed
	}
	cpuShares := int64(0)
	if parsed, parseErr := strconv.ParseInt(m.containerCPUShares, 10, 64); parseErr == nil {
		cpuShares = parsed
	}

	containerHTTPPort := nat.Port(strconv.Itoa(m.containerHTTPPort) + "/tcp")
	createConfig := &dockercontainer.Config{
		Image: m.sandboxImage,
		Env:   env,
		ExposedPorts: nat.PortSet{
			containerHTTPPort: {},
		},
		Labels: map[string]string{
			labelOrgID:            opts.OrgID,
			labelWorkspaceID:      opts.WorkspaceID,
			labelRuntimeScope:     m.containerScope,
			labelRuntimeProjectID: name,
		},
	}
	hostConfig := &dockercontainer.HostConfig{
		Runtime:     m.containerRuntime,
		Binds:       binds,
		CapAdd:      capAdd,
		NetworkMode: dockercontainer.NetworkMode("bridge"),
		ExtraHosts:  []string{"host.docker.internal:host-gateway"},
		Resources: dockercontainer.Resources{
			Memory:    memoryBytes,
			CPUShares: cpuShares,
			Devices:   devices,
		},
		PortBindings: nat.PortMap{
			containerHTTPPort: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: ""}},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	createResponse, err := m.docker.ContainerCreate(
		ctx,
		createConfig,
		hostConfig,
		&dockernetwork.NetworkingConfig{},
		nil,
		dockerName,
	)
	cancel()
	if err != nil {
		if r2Config != nil {
			_ = m.cleanupContainerR2Config(dockerName)
		}
		m.trace("ensure_container_create_failed", map[string]any{"name": name, "error": err.Error()})
		return nil, fmt.Errorf("failed to create container %s: %w", name, err)
	}

	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	startErr := m.docker.ContainerStart(startCtx, createResponse.ID, dockercontainer.StartOptions{})
	startCancel()
	if startErr != nil {
		m.trace("ensure_container_create_failed", map[string]any{"name": name, "error": startErr.Error(), "containerId": createResponse.ID})
		return nil, fmt.Errorf("failed to start container %s: %w", name, startErr)
	}

	containerID := strings.TrimSpace(createResponse.ID)
	if len(containerID) > 12 {
		containerID = containerID[:12]
	}

	port, containerIP := m.getHostPortAndIP(name)
	if port == 0 {
		return nil, fmt.Errorf("container %s created but no port mapping found", name)
	}

	if !m.waitForHealth(port, 30*time.Second) {
		log.Printf("[ContainerManager] container %s health check timed out, proceeding anyway", name)
		m.trace("ensure_container_health_timeout", map[string]any{"name": name, "hostPort": port})
	}

	rec := &ContainerRecord{
		Name:             name,
		ContainerID:      containerID,
		HostPort:         port,
		ContainerIP:      containerIP,
		Status:           "running",
		CreatedAt:        nowMillis(),
		LastAccessedAt:   nowMillis(),
		InFlightRequests: 0,
		OrgID:            opts.OrgID,
		WorkspaceID:      opts.WorkspaceID,
	}

	m.mu.Lock()
	m.containers[name] = rec
	if containerIP != "" {
		m.indexContainerIPLocked(containerIP, name)
	}
	m.resetIdleTimerLocked(name, rec)
	m.mu.Unlock()

	log.Printf("[ContainerManager] created container %s (id=%s, port=%d)", name, containerID, port)
	m.trace("ensure_container_create_success", map[string]any{"name": name, "container": rec})
	return copyRecord(rec), nil
}

func (m *Manager) GetContainer(name string) (*ContainerRecord, error) {
	dockerName := m.dockerName(name)

	m.mu.Lock()
	cached := m.containers[name]
	m.mu.Unlock()

	if cached != nil {
		if running, _ := m.isRunning(name); running {
			m.mu.Lock()
			if current := m.containers[name]; current != nil {
				current.LastAccessedAt = nowMillis()
				m.resetIdleTimerLocked(name, current)
				out := copyRecord(current)
				m.mu.Unlock()
				return out, nil
			}
			m.mu.Unlock()
		} else {
			m.mu.Lock()
			m.removeContainerRecordLocked(name)
			m.mu.Unlock()
		}
	}

	if inspect, err := m.inspectContainer(dockerName, 30*time.Second); err == nil && inspect.State != nil && inspect.State.Running {
		if !m.matchesConfiguredImage(inspect) {
			m.trace("get_container_image_mismatch_existing", map[string]any{
				"name":            name,
				"configuredImage": m.sandboxImage,
				"containerImage":  configuredImageFromInspect(inspect),
			})
			log.Printf("[ContainerManager] container %s image mismatch (have=%s want=%s); recreating", name, configuredImageFromInspect(inspect), m.sandboxImage)
			_ = m.removeContainerIfExists(dockerName, true)
			return nil, nil
		}
		port := hostPortFromInspect(inspect, m.containerHTTPPort)
		containerIP := containerIPFromInspect(inspect)
		if port > 0 {
			rec := &ContainerRecord{
				Name:             name,
				ContainerID:      dockerName,
				HostPort:         port,
				ContainerIP:      containerIP,
				Status:           "running",
				CreatedAt:        nowMillis(),
				LastAccessedAt:   nowMillis(),
				InFlightRequests: 0,
				OrgID:            labelFromInspect(inspect, labelOrgID),
				WorkspaceID:      labelFromInspect(inspect, labelWorkspaceID),
			}
			m.mu.Lock()
			m.containers[name] = rec
			if containerIP != "" {
				m.indexContainerIPLocked(containerIP, name)
			}
			m.resetIdleTimerLocked(name, rec)
			m.mu.Unlock()
			m.trace("get_container_reconnected", map[string]any{"name": name, "container": rec})
			return copyRecord(rec), nil
		}
	}

	return nil, nil
}

func (m *Manager) Exec(ctx context.Context, name string, opts EnsureContainerOptions, req ExecRequest) (ExecResponse, error) {
	if len(req.Cmd) == 0 {
		return ExecResponse{}, fmt.Errorf("cmd is required")
	}
	if _, err := m.EnsureContainer(name, opts); err != nil {
		return ExecResponse{}, err
	}
	dockerName := m.dockerName(name)

	workingDir := strings.TrimSpace(req.Cwd)
	if workingDir == "" {
		workingDir = "/home/claude"
	}

	execEnv := []string{
		"HOME=/home/claude",
		"USER=claude",
	}
	for key, value := range req.Env {
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.Contains(value, "\x00") {
			continue
		}
		execEnv = append(execEnv, key+"="+value)
	}

	createResp, err := m.docker.ContainerExecCreate(ctx, dockerName, dockercontainer.ExecOptions{
		User:         "claude",
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   workingDir,
		Cmd:          req.Cmd,
		Env:          execEnv,
	})
	if err != nil {
		return ExecResponse{}, err
	}

	hijacked, err := m.docker.ContainerExecAttach(ctx, createResp.ID, dockercontainer.ExecAttachOptions{Tty: false})
	if err != nil {
		return ExecResponse{}, err
	}
	defer hijacked.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, hijacked.Reader); err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		if err != io.EOF {
			return ExecResponse{}, err
		}
	}

	inspect, err := m.docker.ContainerExecInspect(context.Background(), createResp.ID)
	if err != nil {
		return ExecResponse{}, err
	}
	return ExecResponse{
		Success:  inspect.ExitCode == 0,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: inspect.ExitCode,
	}, nil
}

func (m *Manager) TerminateContainer(name, reason string) (bool, error) {
	dockerName := m.dockerName(name)

	m.mu.Lock()
	existing := copyRecord(m.containers[name])
	m.mu.Unlock()
	m.trace("terminate_container_request", map[string]any{"name": name, "reason": reason, "container": existing})

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	stopTimeoutSecs := 5
	stopErr := m.docker.ContainerStop(stopCtx, dockerName, dockercontainer.StopOptions{Timeout: &stopTimeoutSecs})
	stopCancel()
	noSuchContainer := stopErr != nil && dockererrdefs.IsNotFound(stopErr)
	if stopErr != nil && !noSuchContainer {
		m.trace("terminate_container_stop_failed", map[string]any{"name": name, "reason": reason, "error": stopErr.Error()})
	}

	removeErr := m.removeContainerIfExists(dockerName, true)
	terminated := removeErr == nil || noSuchContainer
	if !terminated {
		stillRunning, _ := m.isRunning(name)
		terminated = !stillRunning
		m.trace("terminate_container_postcheck", map[string]any{"name": name, "reason": reason, "stillRunning": stillRunning, "error": removeErr.Error()})
	}

	if terminated {
		m.mu.Lock()
		m.removeContainerRecordLocked(name)
		m.mu.Unlock()

		log.Printf("[ContainerManager] terminated container %s", name)
		m.trace("terminate_container_success", map[string]any{"name": name, "reason": reason})
		return true, nil
	}

	m.trace("terminate_container_failed", map[string]any{"name": name, "reason": reason})
	if removeErr != nil {
		return false, removeErr
	}
	if stopErr != nil {
		return false, stopErr
	}
	return false, nil
}

func (m *Manager) ListContainers() []ContainerRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ContainerRecord, 0, len(m.containers))
	for _, rec := range m.containers {
		out = append(out, *copyRecord(rec))
	}
	return out
}

func (m *Manager) getContainerBySourceIPCached(sourceIP string) *ContainerRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	if indexedName, ok := m.containerIPIndex[sourceIP]; ok {
		if rec := m.containers[indexedName]; rec != nil {
			return rec
		}
		delete(m.containerIPIndex, sourceIP)
	}

	for name, rec := range m.containers {
		if rec.ContainerIP == "" {
			continue
		}
		for _, key := range sourceIPKeys(rec.ContainerIP) {
			if key == sourceIP {
				m.containerIPIndex[sourceIP] = name
				return rec
			}
		}
	}

	return nil
}

func (m *Manager) indexContainerIPLocked(ip, name string) {
	for _, key := range sourceIPKeys(ip) {
		m.containerIPIndex[key] = name
	}
}

func (m *Manager) unindexContainerIPLocked(ip string) {
	for _, key := range sourceIPKeys(ip) {
		delete(m.containerIPIndex, key)
	}
}

func sourceIPKeys(ip string) []string {
	normalized := normalizeSourceIP(ip)
	if normalized == "" {
		return nil
	}
	keys := map[string]struct{}{normalized: {}}
	if ipv4Regex.MatchString(normalized) {
		keys["::ffff:"+normalized] = struct{}{}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	return out
}

var ipv4Regex = regexp.MustCompile(`^\d{1,3}(?:\.\d{1,3}){3}$`)

func normalizeSourceIP(ip string) string {
	trimmed := strings.TrimSpace(ip)
	return strings.TrimPrefix(trimmed, "::ffff:")
}

func (m *Manager) workspacePath(name string) string {
	return filepath.Join(m.workspacesRoot, name)
}

func (m *Manager) dockerName(name string) string {
	return m.containerNamePrefix + name
}

func (m *Manager) logicalName(dockerName string) string {
	if m.containerNamePrefix == "" {
		return dockerName
	}
	return strings.TrimPrefix(dockerName, m.containerNamePrefix)
}

func (m *Manager) getHostPort(name string) (int, error) {
	inspect, err := m.inspectContainer(m.dockerName(name), 30*time.Second)
	if err != nil {
		return 0, err
	}
	return hostPortFromInspect(inspect, m.containerHTTPPort), nil
}

func (m *Manager) getContainerIP(name string) (string, error) {
	inspect, err := m.inspectContainer(m.dockerName(name), 30*time.Second)
	if err != nil {
		return "", err
	}
	return containerIPFromInspect(inspect), nil
}

func (m *Manager) getHostPortAndIP(name string) (int, string) {
	inspect, err := m.inspectContainer(m.dockerName(name), 30*time.Second)
	if err != nil {
		return 0, ""
	}
	return hostPortFromInspect(inspect, m.containerHTTPPort), containerIPFromInspect(inspect)
}

func (m *Manager) isRunning(name string) (bool, error) {
	inspect, err := m.inspectContainer(m.dockerName(name), 30*time.Second)
	if err != nil {
		if dockererrdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return inspect.State != nil && inspect.State.Running, nil
}

func (m *Manager) waitForHealth(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{}

	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
		resp, err := client.Do(req)
		cancel()
		if err == nil && resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return true
			}
		}
		time.Sleep(m.healthPollInterval)
	}
	return false
}

func (m *Manager) inspectContainer(name string, timeout time.Duration) (dockercontainer.InspectResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return m.docker.ContainerInspect(ctx, name)
}

func (m *Manager) removeContainerIfExists(name string, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := m.docker.ContainerRemove(ctx, name, dockercontainer.RemoveOptions{
		Force: force,
	})
	if err != nil && dockererrdefs.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return err
	}
	if cleanupErr := m.cleanupContainerR2Config(name); cleanupErr != nil {
		log.Printf("[ContainerManager] failed to cleanup R2 config for %s: %v", name, cleanupErr)
		return cleanupErr
	}
	return nil
}

func (m *Manager) discoverRunningContainers() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	list, err := m.docker.ContainerList(ctx, dockercontainer.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", labelRuntimeScope+"="+m.containerScope)),
	})
	if err != nil {
		log.Printf("[ContainerManager] failed to discover running containers: %v", err)
		return
	}

	discovered := 0
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if name == "" {
			continue
		}

		inspect, inspectErr := m.inspectContainer(name, 30*time.Second)
		if inspectErr != nil {
			log.Printf("[ContainerManager] failed to inspect discovered container %s: %v", name, inspectErr)
			continue
		}
		if inspect.State == nil || !inspect.State.Running {
			continue
		}

		port := hostPortFromInspect(inspect, m.containerHTTPPort)
		containerIP := containerIPFromInspect(inspect)
		if port == 0 {
			continue
		}

		logicalName := labelFromInspect(inspect, labelRuntimeProjectID)
		if logicalName == "" {
			logicalName = m.logicalName(name)
		}

		rec := &ContainerRecord{
			Name:             logicalName,
			ContainerID:      name,
			HostPort:         port,
			ContainerIP:      containerIP,
			Status:           "running",
			CreatedAt:        nowMillis(),
			LastAccessedAt:   nowMillis(),
			InFlightRequests: 0,
			OrgID:            labelFromInspect(inspect, labelOrgID),
			WorkspaceID:      labelFromInspect(inspect, labelWorkspaceID),
		}

		m.mu.Lock()
		m.containers[logicalName] = rec
		if containerIP != "" {
			m.indexContainerIPLocked(containerIP, logicalName)
		}
		m.resetIdleTimerLocked(logicalName, rec)
		m.mu.Unlock()
		discovered++
	}

	if discovered > 0 {
		log.Printf("[ContainerManager] discovered %d running container(s) from Docker", discovered)
	}
}

func labelFromInspect(inspect dockercontainer.InspectResponse, key string) string {
	if inspect.Config == nil || inspect.Config.Labels == nil {
		return ""
	}
	return inspect.Config.Labels[key]
}

func configuredImageFromInspect(inspect dockercontainer.InspectResponse) string {
	if inspect.Config == nil {
		return ""
	}
	return strings.TrimSpace(inspect.Config.Image)
}

func (m *Manager) matchesConfiguredImage(inspect dockercontainer.InspectResponse) bool {
	return configuredImageFromInspect(inspect) == strings.TrimSpace(m.sandboxImage)
}

func hostPortFromInspect(inspect dockercontainer.InspectResponse, containerHTTPPort int) int {
	portKey := nat.Port(strconv.Itoa(containerHTTPPort) + "/tcp")
	if inspect.NetworkSettings == nil || inspect.NetworkSettings.Ports == nil {
		return 0
	}
	bindings, ok := inspect.NetworkSettings.Ports[portKey]
	if !ok || len(bindings) == 0 {
		return 0
	}
	hostPort := strings.TrimSpace(bindings[0].HostPort)
	if hostPort == "" {
		return 0
	}
	parsed, err := strconv.Atoi(hostPort)
	if err != nil {
		return 0
	}
	return parsed
}

func containerIPFromInspect(inspect dockercontainer.InspectResponse) string {
	if inspect.NetworkSettings == nil || inspect.NetworkSettings.Networks == nil {
		return ""
	}
	for _, endpoint := range inspect.NetworkSettings.Networks {
		if endpoint == nil {
			continue
		}
		ip := strings.TrimSpace(endpoint.IPAddress)
		if ip != "" {
			return ip
		}
	}
	return ""
}

func copyRecord(rec *ContainerRecord) *ContainerRecord {
	if rec == nil {
		return nil
	}
	copied := *rec
	return &copied
}

func nowMillis() int64 {
	return time.Now().UTC().UnixMilli()
}

func (m *Manager) trace(event string, details map[string]any) {
	if !m.traceLifecycle {
		return
	}
	log.Printf("[ContainerManager][trace] %s %+v", event, details)
}

func envString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := envString(key, "")
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func containerIdleTimeoutFromEnv() time.Duration {
	key := "CONTAINER_IDLE_TIMEOUT_MS"
	if strings.TrimSpace(os.Getenv(key)) == "" {
		key = "IDLE_TIMEOUT_MS"
	}
	return maxDuration(
		minContainerIdleTimeout,
		time.Duration(envInt(key, defaultContainerIdleTimeoutMS))*time.Millisecond,
	)
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func defaultLocalRoot() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".sandbox-host"
	}
	return filepath.Join(wd, ".sandbox-host")
}

func defaultWorkspaceRoot() string {
	if runtime.GOOS == "linux" {
		return "/srv/sandboxes"
	}
	return filepath.Join(defaultLocalRoot(), "workspaces")
}

func defaultContainerRuntime() string {
	if runtime.GOOS == "linux" {
		return "runsc"
	}
	return "runc"
}
