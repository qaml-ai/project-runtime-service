package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

type Config struct {
	Port                      int
	ListenAddr                string
	DockerProxyPort           int
	DockerProxyListenAddr     string
	ControlAuthType           string
	ControlBearerToken        string
	TLSCertFile               string
	TLSKeyFile                string
	TLSClientCAFile           string
	HostPiSessionRoot         string
	UsageDBRoot               string
	ProjectStateRoot          string
	BackupRoot                string
	BackupRetention           int
	DiskReserveBytes          int64
	ArchiveRetention          int
	ArchiveAfter              time.Duration
	ArchiveSweepInterval      time.Duration
	ObjectStoreBucket         string
	ObjectStorePrefix         string
	ObjectStoreEndpoint       string
	ObjectStoreRegion         string
	ObjectStoreAccessKey      string
	ObjectStoreSecretKey      string
	ObjectStorePathStyle      bool
	ProxyCapabilitiesFile     string
	DataProxyUpstreamURL      string
	WorkerBaseURL             string
	ProjectRuntimeProxySecret string
	IdleTimeout               time.Duration
	ReadHeaderTimeout         time.Duration
	WriteTimeout              time.Duration
	TraceProjectRuntime       bool
}

type DataProxyServiceConfig struct {
	Port              int
	ListenAddr        string
	IdleTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	HandlerConfig     DataProxyHandlerConfig
}

func LoadConfig() Config {
	controlPort := envInt("PORT", defaultByPlatform(80, 4400))
	dockerProxyPort := envInt("PROJECT_RUNTIME_DOCKER_PROXY_PORT", 8081)
	dataProxyPort := envInt("DATA_PROXY_PORT", defaultByPlatform(8090, 8090))
	idleSecs := maxInt(10, envInt("PROJECT_RUNTIME_IDLE_TIMEOUT_SECS", 120))

	return Config{
		Port:                      controlPort,
		ListenAddr:                ":" + strconv.Itoa(controlPort),
		DockerProxyPort:           dockerProxyPort,
		DockerProxyListenAddr:     ":" + strconv.Itoa(dockerProxyPort),
		ControlAuthType:           envString("CONTROL_PLANE_AUTH_TYPE", "none"),
		ControlBearerToken:        envString("CONTROL_PLANE_BEARER_TOKEN", ""),
		TLSCertFile:               envString("CONTROL_PLANE_TLS_CERT_FILE", ""),
		TLSKeyFile:                envString("CONTROL_PLANE_TLS_KEY_FILE", ""),
		TLSClientCAFile:           envString("CONTROL_PLANE_TLS_CLIENT_CA_FILE", ""),
		HostPiSessionRoot:         envString("PROJECT_RUNTIME_PI_SESSION_ROOT", defaultHostPiSessionRoot()),
		UsageDBRoot:               envString("PROJECT_RUNTIME_USAGE_DB_DIR", defaultUsageDBRoot()),
		ProjectStateRoot:          envString("PROJECT_RUNTIME_STATE_ROOT", defaultProjectStateRoot()),
		BackupRoot:                envString("PROJECT_RUNTIME_BACKUP_ROOT", defaultBackupRoot()),
		BackupRetention:           maxInt(1, envInt("PROJECT_RUNTIME_BACKUP_RETENTION", 5)),
		DiskReserveBytes:          envInt64("PROJECT_RUNTIME_DISK_RESERVE_BYTES", 20*1024*1024*1024),
		ArchiveRetention:          maxInt(1, envInt("PROJECT_RUNTIME_ARCHIVE_RETENTION", 2)),
		ArchiveAfter:              time.Duration(envInt("PROJECT_RUNTIME_ARCHIVE_AFTER_SECS", 0)) * time.Second,
		ArchiveSweepInterval:      time.Duration(maxInt(60, envInt("PROJECT_RUNTIME_ARCHIVE_SWEEP_SECS", 300))) * time.Second,
		ObjectStoreBucket:         envString("PROJECT_RUNTIME_OBJECT_BUCKET", ""),
		ObjectStorePrefix:         envString("PROJECT_RUNTIME_OBJECT_PREFIX", "project-runtime"),
		ObjectStoreEndpoint:       envString("PROJECT_RUNTIME_OBJECT_ENDPOINT", ""),
		ObjectStoreRegion:         envString("PROJECT_RUNTIME_OBJECT_REGION", "auto"),
		ObjectStoreAccessKey:      envString("PROJECT_RUNTIME_OBJECT_ACCESS_KEY_ID", ""),
		ObjectStoreSecretKey:      envString("PROJECT_RUNTIME_OBJECT_SECRET_ACCESS_KEY", ""),
		ObjectStorePathStyle:      envBool("PROJECT_RUNTIME_OBJECT_PATH_STYLE", true),
		ProxyCapabilitiesFile:     envString("PROJECT_RUNTIME_PROXY_CAPABILITIES_FILE", defaultProxyCapabilitiesFile()),
		DataProxyUpstreamURL:      envString("DATA_PROXY_UPSTREAM_URL", "http://127.0.0.1:"+strconv.Itoa(dataProxyPort)),
		WorkerBaseURL:             envString("WORKER_BASE_URL", ""),
		ProjectRuntimeProxySecret: envString("PROJECT_RUNTIME_PROXY_SECRET", ""),
		IdleTimeout:               time.Duration(idleSecs) * time.Second,
		ReadHeaderTimeout:         15 * time.Second,
		WriteTimeout:              0,
		TraceProjectRuntime:       envString("TRACE_PROJECT_RUNTIME", "") == "1",
	}
}

func LoadDataProxyServiceConfig() DataProxyServiceConfig {
	dataProxyPort := envInt("DATA_PROXY_PORT", defaultByPlatform(8090, 8090))
	idleSecs := maxInt(10, envInt("DATA_PROXY_IDLE_TIMEOUT_SECS", 120))
	requestLimit := envInt64("DATA_PROXY_MAX_REQUEST_BYTES", defaultDataProxyRequestLimitBytes)
	tunnelIdleSecs := envInt("DATA_PROXY_SSH_TUNNEL_IDLE_SECS", int(defaultSSHTunnelIdleTimeout.Seconds()))
	tunnelConnectSecs := envInt("DATA_PROXY_SSH_TUNNEL_CONNECT_TIMEOUT_SECS", int(defaultSSHTunnelConnectTimeout.Seconds()))

	return DataProxyServiceConfig{
		Port:              dataProxyPort,
		ListenAddr:        ":" + strconv.Itoa(dataProxyPort),
		IdleTimeout:       time.Duration(idleSecs) * time.Second,
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      0,
		HandlerConfig: DataProxyHandlerConfig{
			RequestBodyLimitBytes: requestLimit,
			TunnelManager: NewSSHTunnelManager(SSHTunnelConfig{
				Host:                  envString("DATA_PROXY_SSH_TUNNEL_HOST", ""),
				Port:                  envInt("DATA_PROXY_SSH_TUNNEL_PORT", defaultSSHTunnelPort),
				User:                  envString("DATA_PROXY_SSH_TUNNEL_USER", defaultSSHTunnelUser),
				IdentityFile:          envString("DATA_PROXY_SSH_TUNNEL_KEY_PATH", ""),
				KnownHostsFile:        envString("DATA_PROXY_SSH_TUNNEL_KNOWN_HOSTS_PATH", ""),
				ProxyCommand:          envString("DATA_PROXY_SSH_PROXY_COMMAND", ""),
				StrictHostKeyChecking: envString("DATA_PROXY_SSH_TUNNEL_STRICT_HOST_KEY_CHECKING", defaultSSHTunnelStrictHostKeyMode),
				IdleTimeout:           time.Duration(tunnelIdleSecs) * time.Second,
				ConnectTimeout:        time.Duration(tunnelConnectSecs) * time.Second,
			}),
		},
	}
}

func defaultByPlatform(linuxValue, otherValue int) int {
	if runtime.GOOS == "linux" {
		return linuxValue
	}
	return otherValue
}

func defaultUsageDBRoot() string {
	if runtime.GOOS == "linux" {
		return "/srv/project-runtime/.project-runtime/usage"
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".project-runtime/usage"
	}
	return filepath.Join(wd, ".project-runtime", "usage")
}

func defaultBackupRoot() string {
	if runtime.GOOS == "linux" {
		return "/srv/project-runtime/.project-runtime/backups"
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".project-runtime/backups"
	}
	return filepath.Join(wd, ".project-runtime", "backups")
}

func defaultProjectStateRoot() string {
	if runtime.GOOS == "linux" {
		return "/srv/project-runtime/.project-runtime/state"
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".project-runtime/state"
	}
	return filepath.Join(wd, ".project-runtime", "state")
}

func defaultHostPiSessionRoot() string {
	if runtime.GOOS == "linux" {
		return "/srv/project-runtime/.project-runtime/pi-sessions"
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".project-runtime/pi-sessions"
	}
	return filepath.Join(wd, ".project-runtime", "pi-sessions")
}

func defaultProxyCapabilitiesFile() string {
	if runtime.GOOS == "linux" {
		return "/etc/project-runtime-service/proxies.json"
	}
	return ""
}

func envString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := envString(key, "")
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(key string, fallback int64) int64 {
	raw := envString(key, "")
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	raw := envString(key, "")
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return fallback
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
