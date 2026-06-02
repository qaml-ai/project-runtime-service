package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

type Config struct {
	Port                  int
	ListenAddr            string
	DockerProxyPort       int
	DockerProxyListenAddr string
	ControlAuthType       string
	ControlBearerToken    string
	TLSCertFile           string
	TLSKeyFile            string
	TLSClientCAFile       string
	HostPiSessionRoot     string
	UsageDBRoot           string
	BackupRoot            string
	BackupRetention       int
	DiskReserveBytes      int64
	ProxyCapabilitiesJSON string
	ProxyCapabilitiesFile string
	DataProxyUpstreamURL  string
	WorkerBaseURL         string
	SandboxProxySecret    string
	IdleTimeout           time.Duration
	ReadHeaderTimeout     time.Duration
	WriteTimeout          time.Duration
	TraceSandboxHost      bool
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
	dockerProxyPort := envInt("SANDBOX_DOCKER_PROXY_PORT", 8081)
	dataProxyPort := envInt("DATA_PROXY_PORT", defaultByPlatform(8090, 8090))
	idleSecs := maxInt(10, envInt("SANDBOX_HOST_IDLE_TIMEOUT_SECS", 120))

	return Config{
		Port:                  controlPort,
		ListenAddr:            ":" + strconv.Itoa(controlPort),
		DockerProxyPort:       dockerProxyPort,
		DockerProxyListenAddr: ":" + strconv.Itoa(dockerProxyPort),
		ControlAuthType:       envString("CONTROL_PLANE_AUTH_TYPE", "none"),
		ControlBearerToken:    envString("CONTROL_PLANE_BEARER_TOKEN", ""),
		TLSCertFile:           envString("CONTROL_PLANE_TLS_CERT_FILE", ""),
		TLSKeyFile:            envString("CONTROL_PLANE_TLS_KEY_FILE", ""),
		TLSClientCAFile:       envString("CONTROL_PLANE_TLS_CLIENT_CA_FILE", ""),
		HostPiSessionRoot:     envString("HOST_PI_SESSION_ROOT", defaultHostPiSessionRoot()),
		UsageDBRoot:           envString("SANDBOX_HOST_USAGE_DB_DIR", defaultUsageDBRoot()),
		BackupRoot:            envString("PROJECT_RUNTIME_BACKUP_ROOT", defaultBackupRoot()),
		BackupRetention:       maxInt(1, envInt("PROJECT_RUNTIME_BACKUP_RETENTION", 5)),
		DiskReserveBytes:      envInt64("PROJECT_RUNTIME_DISK_RESERVE_BYTES", 20*1024*1024*1024),
		ProxyCapabilitiesJSON: envString("PROJECT_RUNTIME_PROXY_CAPABILITIES_JSON", ""),
		ProxyCapabilitiesFile: envString("PROJECT_RUNTIME_PROXY_CAPABILITIES_FILE", ""),
		DataProxyUpstreamURL:  envString("DATA_PROXY_UPSTREAM_URL", "http://127.0.0.1:"+strconv.Itoa(dataProxyPort)),
		WorkerBaseURL:         envString("WORKER_BASE_URL", ""),
		SandboxProxySecret:    envString("SANDBOX_PROXY_SECRET", ""),
		IdleTimeout:           time.Duration(idleSecs) * time.Second,
		ReadHeaderTimeout:     15 * time.Second,
		WriteTimeout:          0,
		TraceSandboxHost:      envString("TRACE_SANDBOX_HOST", "") == "1",
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
		return "/srv/sandboxes/.sandbox-host/usage"
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".sandbox-host/usage"
	}
	return filepath.Join(wd, ".sandbox-host", "usage")
}

func defaultBackupRoot() string {
	if runtime.GOOS == "linux" {
		return "/srv/sandboxes/.project-runtime/backups"
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".project-runtime/backups"
	}
	return filepath.Join(wd, ".project-runtime", "backups")
}

func defaultHostPiSessionRoot() string {
	if runtime.GOOS == "linux" {
		return "/srv/sandboxes/.sandbox-host/pi-sessions"
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ".sandbox-host/pi-sessions"
	}
	return filepath.Join(wd, ".sandbox-host", "pi-sessions")
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
