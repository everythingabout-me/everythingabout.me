package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"everythingabout.me/backend/internal/api"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
)

// runUploadGateway starts a dedicated byte receiver. It deliberately starts no
// application workers: all native commit/recovery work belongs to the app.
func runUploadGateway(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("upload-gateway takes no arguments: %v", args)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err = cfg.ValidateUploadGateway(); err != nil {
		return err
	}
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to upload database: %w", err)
	}
	defer db.Close()
	backend, err := storagebackend.New(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize upload storage: %w", err)
	}
	defer backend.Close()
	receiver := api.New(cfg, db, backend.Legacy(), nil)
	receiver.SetStorageFactory(backend)
	server := &http.Server{Addr: cfg.Addr, Handler: receiver.UploadGatewayHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	shutdown, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdown.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	slog.Info("upload gateway started", "addr", cfg.Addr, "env", cfg.AppEnv)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
