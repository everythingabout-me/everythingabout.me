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
	"everythingabout.me/backend/internal/cos"
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
	if err := cfg.ValidateCOS(); err != nil {
		return err
	}

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	objectStorage, err := cos.New(cfg.COS)
	if err != nil {
		return fmt.Errorf("failed to initialize COS: %w", err)
	}

	apiServer := api.New(cfg, db, objectStorage, webui.New(cfg.WebDir))
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	shutdownSignal, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	apiServer.StartTimeline(shutdownSignal)

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
