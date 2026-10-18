package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"edition/internal/app"
	"edition/internal/auth"
	"edition/internal/config"
	"edition/internal/httpapi"
	"edition/internal/news"
	"edition/internal/postgres"
)

var version = "dev"
var commit = "unknown"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	tokens, err := auth.New(cfg.JWTSecret, cfg.SessionTTL)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	repo, err := postgres.New(initCtx, cfg.DatabaseURL, tokens)
	if err == nil {
		err = repo.EnsureClient(initCtx, news.Client{ID: "editor", Name: "Редактор", Role: "editor", Trust: 0, Enabled: true}, cfg.EditorAPIKey)
	}
	if err == nil {
		_, err = repo.Policy(initCtx)
	}
	cancel()
	if err != nil {
		if repo != nil {
			repo.Pool.Close()
		}
		return errors.New("database initialization failed; check connection and run migrate up")
	}
	defer repo.Pool.Close()
	adapter := &httpapi.Server{Service: app.Service{Repo: repo, Now: time.Now}, Logger: logger, Version: version, Commit: commit}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: adapter.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	logger.Info("newsroom started", "address", cfg.HTTPAddr, "version", version, "commit", commit)
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

func healthcheck() error {
	client := &http.Client{Timeout: 3 * time.Second}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	response, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/health/ready")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness status: %d", response.StatusCode)
	}
	return nil
}
