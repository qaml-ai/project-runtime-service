package main

import (
	"context"
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
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
