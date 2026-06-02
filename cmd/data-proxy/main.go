package main

import (
	"log"
	"net/http"

	"github.com/qaml-ai/project-runtime-service/internal/app"
)

func main() {
	cfg := app.LoadDataProxyServiceConfig()
	handler := app.NewDataProxyHandler(cfg.HandlerConfig)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		WriteTimeout:      cfg.WriteTimeout,
	}

	log.Printf("[DataProxy] listener on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("data-proxy stopped: %v", err)
	}
}
