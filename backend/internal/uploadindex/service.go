// Package uploadindex repairs the index of paths used by legacy direct COS uploads.
// A watch observes native facts; it is not an upload receipt or publication fence.
package uploadindex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"everythingabout.me/backend/internal/paths"
)

const (
	pollInterval       = 5 * time.Second
	hotInterval        = 15 * time.Second
	coldInterval       = 24 * time.Hour
	retryInterval      = 30 * time.Second
	hotGrace           = time.Hour
	claimLease         = 3 * time.Minute
	observationTimeout = 10 * time.Second
	journalTimeout     = 5 * time.Second
	batchLimit         = 32
	parallelLimit      = 4
)

// Key binds a normalized path to the exact storage configuration that issued it.
// Binding must change when the backend, owner root mapping or native bucket changes.
type Key struct {
	Binding string
	OwnerID string
	Path    string
	IsDir   bool
}

// Watch includes two independent fences: Generation changes on Register/Settle;
// LeaseVersion changes on every claim, including reclaim after a crashed worker.
type Watch struct {
	Key
	IssuedUntil  time.Time
	Settled      bool
	Generation   int64
	LeaseVersion int64
}

// Result describes an observation, never the success of an upload attempt.
type Result struct {
	NextCheck   time.Time
	CheckedAt   time.Time
	Fingerprint string
	Error       string
}

// Journal durably schedules observations. Finish must condition its update on both
// watch fences, so a stale worker cannot override renewed or explicitly settled work.
type Journal interface {
	Register(context.Context, Key, time.Time, time.Time) error
	Settle(context.Context, Key, time.Time, time.Time) error
	Claim(context.Context, string, time.Time, time.Time, int) ([]Watch, error)
	Finish(context.Context, Watch, Result) error
}

// Observer checks storage and updates its index from the actual native object.
// An empty fingerprint with no error means no complete object is currently visible.
// Implementations must honor cancellation and coordinate their index writes with
// application mutations; the scheduling lease does not guard native publication.
type Observer interface {
	Observe(context.Context, string, string, bool) (string, error)
}

type ObserverFunc func(context.Context, string, string, bool) (string, error)

func (f ObserverFunc) Observe(ctx context.Context, ownerID, path string, isDir bool) (string, error) {
	return f(ctx, ownerID, path, isDir)
}

type Service struct {
	binding  string
	journal  Journal
	observer Observer
	now      func() time.Time
}

func New(binding string, journal Journal, observer Observer) *Service {
	return &Service{binding: binding, journal: journal, observer: observer, now: time.Now}
}

func (s *Service) key(ownerID, path string, isDir bool) (Key, error) {
	if strings.TrimSpace(s.binding) == "" || strings.TrimSpace(ownerID) == "" {
		return Key{}, errors.New("upload index observation requires storage binding and owner")
	}
	normalized, err := paths.Normalize(path)
	if err != nil {
		return Key{}, err
	}
	if normalized == "" {
		return Key{}, errors.New("upload index observation requires a non-root path")
	}
	return Key{Binding: s.binding, OwnerID: ownerID, Path: normalized, IsDir: isDir}, nil
}

// Register must succeed before credentials or a native folder write are exposed.
// Re-registering a path renews hot observation and fences older acknowledgments.
func (s *Service) Register(ctx context.Context, ownerID, path string, isDir bool, issuedUntil time.Time) error {
	key, err := s.key(ownerID, path, isDir)
	if err != nil {
		return err
	}
	return s.journal.Register(ctx, key, issuedUntil, s.now())
}

// Settle is only an explicit client-confirmation scheduling hint. It does not
// delete the watch: already authenticated provider requests can still finish late.
func (s *Service) Settle(ctx context.Context, ownerID, path string, isDir bool) error {
	key, err := s.key(ownerID, path, isDir)
	if err != nil {
		return err
	}
	now := s.now()
	return s.journal.Settle(ctx, key, now, now.Add(coldInterval))
}

// Run resumes due work immediately and remains independent of request contexts.
// The caller supplies the server shutdown context and starts it once per process.
func (s *Service) Run(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("upload index observation failed", "error", err)
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// RunOnce claims a bounded batch. Four concurrent, individually bounded checks
// keep the 32-record batch within the claim lease under cooperative cancellation.
func (s *Service) RunOnce(ctx context.Context) error {
	now := s.now()
	watches, err := s.journal.Claim(ctx, s.binding, now, now.Add(claimLease), batchLimit)
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		return nil
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	pending := make(chan Watch)
	for range parallelLimit {
		wg.Go(func() {
			for watch := range pending {
				if ctx.Err() != nil {
					continue // The lease permits replay after shutdown or a crash.
				}
				if err := s.observe(ctx, watch); err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
				}
			}
		})
	}
	for _, watch := range watches {
		select {
		case pending <- watch:
		case <-ctx.Done():
			close(pending)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(pending)
	wg.Wait()
	return errors.Join(failures...)
}

func (s *Service) observe(ctx context.Context, watch Watch) error {
	checkCtx, cancel := context.WithTimeout(ctx, observationTimeout)
	fingerprint, observeErr := s.observer.Observe(checkCtx, watch.OwnerID, watch.Path, watch.IsDir)
	if observeErr == nil {
		observeErr = checkCtx.Err()
	}
	cancel()
	now := s.now()
	delay := coldInterval
	if !watch.Settled && now.Before(watch.IssuedUntil.Add(hotGrace)) {
		delay = hotInterval
	}
	result := Result{CheckedAt: now, NextCheck: now.Add(delay), Fingerprint: fingerprint}
	if observeErr != nil {
		result.Error = strings.ReplaceAll(observeErr.Error(), "\x00", "\uFFFD")
		if len(result.Error) > 2048 {
			result.Error = strings.ToValidUTF8(result.Error[:2048], "\uFFFD")
		}
		result.NextCheck = now.Add(retryInterval)
	}
	// Server cancellation must leave the durable claim replayable, rather than
	// marking an uncommitted/unknown observation successfully checked.
	if err := ctx.Err(); err != nil {
		return err
	}
	finishCtx, finishCancel := context.WithTimeout(ctx, journalTimeout)
	defer finishCancel()
	if err := s.journal.Finish(finishCtx, watch, result); err != nil {
		return fmt.Errorf("save upload index observation: %w", err)
	}
	return observeErr
}
