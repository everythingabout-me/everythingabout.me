package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"log/slog"

	"everythingabout.me/backend/internal/api"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/webui"
)

func runServer(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("server takes no arguments: %v", args)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateServer(); err != nil {
		return err
	}

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	// Refresh measured write capability for this exact connection. Evidence cannot
	// be carried across a bucket or region change, and re-running the probe here
	// keeps configuration in charge instead of making an operator redo it by hand.
	if err := storagebackend.EnsureAdmission(ctx, cfg); err != nil {
		slog.Warn("object admission probe failed; coordinated object writes stay disabled", "error", err)
	}
	backend, err := storagebackend.NewForServing(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize storage backend: %w", err)
	}
	defer backend.Close()
	if problem := backend.ObjectWritesProblem(); problem != nil {
		slog.Warn("coordinated object writes are disabled", "reason", problem)
	}

	// The configured tree wins: when it changed, adopt it and invalidate the derived
	// view instead of refusing to start. This is a constant number of statements per
	// changed owner, with no native enumeration.
	if err := adoptStorageRoots(ctx, db, backend); err != nil {
		return err
	}

	apiServer := api.New(cfg, db, backend.Legacy(), webui.New(cfg.WebDir))
	apiServer.SetStorageFactory(backend)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	shutdownSignal, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	apiServer.StartTimeline(shutdownSignal)
	apiServer.StartUploadIndex(shutdownSignal)
	apiServer.StartFileOperations(shutdownSignal)

	go func() {
		<-shutdownSignal.Done()
		slog.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	slog.Info("server started", "addr", cfg.Addr, "env", cfg.AppEnv, "web", cfg.WebDir)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
