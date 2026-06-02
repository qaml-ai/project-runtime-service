package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultSSHTunnelUser              = "tunnel"
	defaultSSHTunnelPort              = 22
	defaultSSHTunnelIdleTimeout       = 10 * time.Minute
	defaultSSHTunnelConnectTimeout    = 10 * time.Second
	defaultSSHTunnelStrictHostKeyMode = "accept-new"
)

type SSHTunnelConfig struct {
	Host                  string
	Port                  int
	User                  string
	IdentityFile          string
	KnownHostsFile        string
	ProxyCommand          string
	StrictHostKeyChecking string
	IdleTimeout           time.Duration
	ConnectTimeout        time.Duration
}

type SSHTunnelManager struct {
	cfg     SSHTunnelConfig
	mu      sync.Mutex
	tunnels map[string]*sshTunnel
}

type sshTunnel struct {
	destinationHost string
	destinationPort int
	localHost       string
	localPort       int
	cmd             *exec.Cmd
	lastUsed        time.Time
	done            chan error
}

func NewSSHTunnelManager(cfg SSHTunnelConfig) *SSHTunnelManager {
	cfg = normalizeSSHTunnelConfig(cfg)
	if strings.TrimSpace(cfg.Host) == "" {
		return nil
	}
	return &SSHTunnelManager{
		cfg:     cfg,
		tunnels: make(map[string]*sshTunnel),
	}
}

func normalizeSSHTunnelConfig(cfg SSHTunnelConfig) SSHTunnelConfig {
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.User = strings.TrimSpace(cfg.User)
	if cfg.User == "" {
		cfg.User = defaultSSHTunnelUser
	}
	if cfg.Port <= 0 {
		cfg.Port = defaultSSHTunnelPort
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultSSHTunnelIdleTimeout
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = defaultSSHTunnelConnectTimeout
	}
	cfg.IdentityFile = strings.TrimSpace(cfg.IdentityFile)
	cfg.KnownHostsFile = strings.TrimSpace(cfg.KnownHostsFile)
	cfg.ProxyCommand = strings.TrimSpace(cfg.ProxyCommand)
	cfg.StrictHostKeyChecking = strings.TrimSpace(cfg.StrictHostKeyChecking)
	if cfg.StrictHostKeyChecking == "" {
		cfg.StrictHostKeyChecking = defaultSSHTunnelStrictHostKeyMode
	}
	return cfg
}

func (m *SSHTunnelManager) EnsureTunnel(ctx context.Context, destinationHost string, destinationPort int) (sqlEndpoint, error) {
	if m == nil {
		return sqlEndpoint{Host: destinationHost, Port: destinationPort}, nil
	}
	destinationHost = strings.TrimSpace(destinationHost)
	if destinationHost == "" || destinationPort <= 0 {
		return sqlEndpoint{}, errors.New("invalid tunnel destination")
	}

	key := tunnelKey(destinationHost, destinationPort)
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.cleanupLocked(now)

	if existing := m.tunnels[key]; existing != nil && existing.running() {
		existing.lastUsed = now
		return sqlEndpoint{Host: existing.localHost, Port: existing.localPort}, nil
	}

	tunnel, err := m.startTunnelLocked(ctx, destinationHost, destinationPort)
	if err != nil {
		return sqlEndpoint{}, err
	}
	m.tunnels[key] = tunnel
	return sqlEndpoint{Host: tunnel.localHost, Port: tunnel.localPort}, nil
}

func (m *SSHTunnelManager) startTunnelLocked(ctx context.Context, destinationHost string, destinationPort int) (*sshTunnel, error) {
	localPort, err := freeLocalPort()
	if err != nil {
		return nil, fmt.Errorf("allocate tunnel port: %w", err)
	}

	localHost := "127.0.0.1"
	localAddr := net.JoinHostPort(localHost, strconv.Itoa(localPort))
	forwardSpec := fmt.Sprintf("%s:%d:%s:%d", localHost, localPort, destinationHost, destinationPort)
	args := []string{
		"-N",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=" + strconv.Itoa(maxInt(1, int(m.cfg.ConnectTimeout.Seconds()))),
		"-o", "StrictHostKeyChecking=" + m.cfg.StrictHostKeyChecking,
	}
	if m.cfg.IdentityFile != "" {
		args = append(args, "-i", m.cfg.IdentityFile)
	}
	if m.cfg.KnownHostsFile != "" {
		args = append(args, "-o", "UserKnownHostsFile="+m.cfg.KnownHostsFile)
	}
	if m.cfg.ProxyCommand != "" {
		args = append(args, "-o", "ProxyCommand="+expandSSHProxyCommand(
			m.cfg.ProxyCommand,
			m.cfg.Host,
			m.cfg.Port,
			m.cfg.User,
		))
	}
	args = append(
		args,
		"-L", forwardSpec,
		"-p", strconv.Itoa(m.cfg.Port),
		fmt.Sprintf("%s@%s", m.cfg.User, m.cfg.Host),
	)

	cmd := exec.CommandContext(context.Background(), "ssh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh tunnel: %w", err)
	}

	tunnel := &sshTunnel{
		destinationHost: destinationHost,
		destinationPort: destinationPort,
		localHost:       localHost,
		localPort:       localPort,
		cmd:             cmd,
		lastUsed:        time.Now(),
		done:            make(chan error, 1),
	}
	go func() {
		tunnel.done <- cmd.Wait()
		close(tunnel.done)
	}()

	if err := waitForTunnelReady(ctx, localAddr, m.cfg.ConnectTimeout, tunnel.done, &stderr); err != nil {
		tunnel.stop()
		return nil, err
	}
	return tunnel, nil
}

func (m *SSHTunnelManager) cleanupLocked(now time.Time) {
	for key, tunnel := range m.tunnels {
		if !tunnel.running() || now.Sub(tunnel.lastUsed) > m.cfg.IdleTimeout {
			tunnel.stop()
			delete(m.tunnels, key)
		}
	}
}

func (t *sshTunnel) running() bool {
	select {
	case <-t.done:
		return false
	default:
		return t.cmd != nil && t.cmd.Process != nil
	}
}

func (t *sshTunnel) stop() {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func waitForTunnelReady(
	ctx context.Context,
	localAddr string,
	timeout time.Duration,
	done <-chan error,
	stderr *bytes.Buffer,
) error {
	if timeout <= 0 {
		timeout = defaultSSHTunnelConnectTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		conn, err := net.DialTimeout("tcp", localAddr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("ssh tunnel canceled: %w", ctx.Err())
		case err := <-done:
			return fmt.Errorf("ssh tunnel exited before ready: %v%s", err, stderrSuffix(stderr.String()))
		case <-deadline.C:
			return fmt.Errorf("ssh tunnel did not become ready after %s%s", timeout, stderrSuffix(stderr.String()))
		case <-tick.C:
		}
	}
}

func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr.Port <= 0 {
		return 0, errors.New("listener did not return a TCP port")
	}
	return addr.Port, nil
}

func tunnelKey(host string, port int) string {
	return strings.ToLower(strings.TrimSpace(host)) + ":" + strconv.Itoa(port)
}

func expandSSHProxyCommand(command string, host string, port int, user string) string {
	replacer := strings.NewReplacer(
		"%%", "%",
		"%h", host,
		"%p", strconv.Itoa(port),
		"%r", user,
	)
	return replacer.Replace(command)
}

func stderrSuffix(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	if len(stderr) > 1000 {
		stderr = stderr[len(stderr)-1000:]
	}
	return ": " + stderr
}
