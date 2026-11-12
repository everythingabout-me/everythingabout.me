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
	"everythingabout.me/backend/internal/connections"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/webui"
)

// shutdownBudget is the single deadline for the drain that starts on the first
// SIGTERM or SIGINT. One context bounds both phases — the HTTP handlers and then
// the background workers — so a slow phase eats into the other instead of adding a
// second timer that would hide which one ran out.
//
// What it must cover: the one request-scoped step that cannot simply be retried is
// an upload commit's settlement. Once the native publication exists, fileops
// detaches SaveApplied and Settle from the request context (durableContext) so a
// disconnected client cannot orphan bytes that are already visible; that tail is
// one index transaction in nodes.CommitStagedFile, not a transfer. A part receiver,
// a move/copy, a timeline job and a backup are all resumable from the journal, so
// cutting them costs a retry rather than a fact. The budget therefore has to cover
// a settlement queued behind the per-owner advisory lock and the 40-connection
// pool, not a whole transfer.
//
// The 10 s this replaces was a round number with no stated coverage. 30 s leaves
// room for several queued settlements while staying well below systemd's default
// 90 s stop timeout, so the service manager still gets to escalate when the drain is
// genuinely stuck. Expiry is logged as work that was cut; it is never reported as a
// successful drain.
const shutdownBudget = 30 * time.Second

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

	// Connections are data. The environment seeds the first authority row of a fresh
	// database and is not consulted again for an owner that already has a binding, so
	// adding a provider is an INSERT instead of a deployment edit.
	connStore := connections.NewPostgresStore(db)
	resolver := connections.NewResolver(*cfg, connStore)
	defer resolver.Close()
	if err := resolver.Seed(ctx); err != nil {
		return fmt.Errorf("seed storage connections: %w", err)
	}
	// The row a binding names is authoritative: when it changed, adopt it and
	// invalidate the derived view instead of refusing to start. This is a constant
	// number of statements per changed owner, with no native enumeration.
	if err := adoptStorageRoots(ctx, db, resolver); err != nil {
		return err
	}

	apiServer := api.New(cfg, db, backend.Legacy(), webui.New(cfg.WebDir))
	apiServer.SetStorageFactory(resolver)
	// A single instance cannot have a backup in flight before it has served a
	// request, so a row still marked 'running' here belongs to a previous process
	// that crashed or was killed before it could record the interruption.
	if err := apiServer.MarkInterruptedBackups(ctx); err != nil {
		slog.Warn("could not record backups interrupted by the previous process", "error", err)
	}
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

	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ListenAndServe() }()

	slog.Info("server started", "addr", cfg.Addr, "env", cfg.AppEnv, "web", cfg.WebDir)
	select {
	case err := <-serveDone:
		// The listener stopped before any signal arrived (a bind failure, or an
		// unexpected close). No handlers were accepted, so there is nothing to
		// drain; the deferred closes run as the process exits.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-shutdownSignal.Done():
	}

	slog.Info("shutting down", "budget", shutdownBudget)
	// The first signal has already done its job by cancelling shutdownSignal, which
	// is the context every background worker received. Unregistering the handler now
	// restores the default disposition, so a second SIGINT or SIGTERM kills the
	// process immediately instead of being swallowed: that is the operator's escape
	// hatch when a handler outlives shutdownBudget, and it is why the budget can be
	// longer than the old one without making a deploy unkillable.
	stop()

	// The ordering below is the fix, so it is stated rather than left to defer
	// intuition. http.Server.Shutdown returns when handlers have finished, but
	// ListenAndServe returns as soon as Shutdown *begins*; closing the pool on that
	// error path (the previous shape, with Shutdown in an unobserved goroutine) ran
	// the deferred resolver/backend/db closes while handlers were still committing.
	// Both drains therefore complete on this goroutine before the function returns:
	//
	//	signal -> Shutdown (handlers) -> WaitBackground (workers) -> return
	//	       -> deferred resolver.Close -> backend.Close -> db.Close
	//
	// so a resource is only closed after the last user of it has stopped.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http drain did not finish within the shutdown budget; some requests were cut", "budget", shutdownBudget, "error", err)
	}
	// Collect the listener result now that Shutdown has closed it; this also keeps
	// the goroutine from outliving the function.
	if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("http server stopped with an error", "error", err)
	}
	if err := apiServer.WaitBackground(shutdownCtx); err != nil {
		slog.Warn("background work did not stop within the shutdown budget; some work was cut", "budget", shutdownBudget, "error", err)
	}
	// A backup started through the web is deliberately not waited for: copying a
	// whole tree can outlive any drain budget, and the process exit is what stops
	// it. Record that interruption while the pool is still open, so a deploy leaves
	// a failed run instead of a row that claims "running" forever. This shares the
	// one shutdown budget; it adds no timer of its own.
	if err := apiServer.MarkInterruptedBackups(shutdownCtx); err != nil {
		slog.Warn("could not record backups interrupted by this shutdown", "error", err)
	}
	return nil
}
