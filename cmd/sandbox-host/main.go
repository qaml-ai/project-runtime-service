package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/qaml-ai/project-runtime-service/internal/app"
	"github.com/qaml-ai/project-runtime-service/internal/container"
	"github.com/qaml-ai/project-runtime-service/internal/fsops"
	"github.com/qaml-ai/project-runtime-service/internal/state"
	"github.com/qaml-ai/project-runtime-service/internal/workspace"
)

func main() {
	cfg := app.LoadConfig()

	usageStore, err := state.NewUsageStore(cfg.UsageDBRoot)
	if err != nil {
		log.Printf("[SandboxHost] usage store unavailable (%s): %v; running without spend tracking", cfg.UsageDBRoot, err)
	}
	if usageStore != nil {
		defer func() {
			if closeErr := usageStore.Close(); closeErr != nil {
				log.Printf("[SandboxHost] failed to close usage store: %v", closeErr)
			}
		}()
	}

	workspaces := workspace.NewManagerFromEnv()
	containers := container.NewManager(workspaces)
	fsManager := fsops.NewManager(os.Getenv("WORKSPACES_ROOT"))
	server := app.NewServer(cfg, containers, workspaces, fsManager, usageStore)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		WriteTimeout:      cfg.WriteTimeout,
	}
	if tlsConfig, err := controlTLSConfig(cfg); err != nil {
		log.Fatalf("control TLS configuration failed: %v", err)
	} else if tlsConfig != nil {
		httpServer.TLSConfig = tlsConfig
	}
	dockerProxyServer := &http.Server{
		Addr:              cfg.DockerProxyListenAddr,
		Handler:           server.DockerProxyHandler(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		WriteTimeout:      cfg.WriteTimeout,
	}

	errCh := make(chan error, 1)
	shutdownDone := make(chan struct{})

	log.Printf("[SandboxHost] control listener on %s", cfg.ListenAddr)
	go func() {
		var err error
		if httpServer.TLSConfig != nil {
			err = httpServer.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = httpServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("control listener failed: %w", err)
		}
	}()
	log.Printf("[SandboxHost] docker proxy listener on %s", cfg.DockerProxyListenAddr)
	go func() {
		if err := dockerProxyServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("docker proxy listener failed: %w", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("[SandboxHost] received %s; shutting down", sig)

		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("[SandboxHost] control listener shutdown failed: %v", err)
		}
		if err := dockerProxyServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("[SandboxHost] docker proxy listener shutdown failed: %v", err)
		}
		cancelShutdown()
		close(shutdownDone)
	}()

	select {
	case err := <-errCh:
		log.Fatalf("sandbox-host stopped: %v", err)
	case <-shutdownDone:
		log.Printf("[SandboxHost] shutdown complete")
	}
}

func controlTLSConfig(cfg app.Config) (*tls.Config, error) {
	if cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" && cfg.TLSClientCAFile == "" {
		return nil, nil
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return nil, fmt.Errorf("CONTROL_PLANE_TLS_CERT_FILE and CONTROL_PLANE_TLS_KEY_FILE are both required when TLS is enabled")
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if cfg.TLSClientCAFile != "" {
		pemBytes, err := os.ReadFile(cfg.TLSClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read client CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("client CA file did not contain any PEM certificates")
		}
		tlsConfig.ClientCAs = pool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsConfig, nil
}
