package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/sanskar/syncprimitives/web"

	"os/signal"
	"syscall"
)

func main() {
	addr := flag.String("addr", ":8085", "HTTP server address")
	allowedOriginsFlag := flag.String("allowed-origins", "", "Comma-separated allowed WebSocket origins (empty = localhost only, * = all)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file path")
	tlsKey := flag.String("tls-key", "", "TLS key file path")
	apiKey := flag.String("api-key", "", "WebSocket API key (empty = no authentication required)")
	flag.Parse()

	// O1: Configure structured logging based on LOG_FORMAT env var.
	logFormat := os.Getenv("LOG_FORMAT")
	var handler slog.Handler
	if logFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(handler))

	slog.Info("Advanced Synchronization Primitives Server")
	slog.Info("Starting server", "addr", *addr)

	var allowedOrigins []string
	if *allowedOriginsFlag != "" {
		for _, o := range strings.Split(*allowedOriginsFlag, ",") {
			if trimmed := strings.TrimSpace(o); trimmed != "" {
				allowedOrigins = append(allowedOrigins, trimmed)
			}
		}
	}

	cfg := web.Config{
		AllowedOrigins: allowedOrigins,
		TLSCertFile:    *tlsCert,
		TLSKeyFile:     *tlsKey,
		APIKey:         *apiKey,
	}

	server := web.NewServerWithConfig(cfg)

	// Run the HTTP server in a separate goroutine so the main goroutine
	// can block on the OS signal channel.
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start(*addr)
	}()

	// Wait for an interrupt or termination signal.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			slog.Error("Server failed", "err", err)
			os.Exit(1)
		}
	case sig := <-quit:
		slog.Info("Received signal — shutting down", "signal", sig)
	}

	// Give active connections up to 10 seconds to finish.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		slog.Error("Shutdown error", "err", err)
	} else {
		slog.Info("Server stopped cleanly")
	}
}
