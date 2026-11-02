package browserupload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"everythingabout.me/backend/internal/paths"
)

const (
	leaseTTL      = 90 * time.Second
	renewInterval = 15 * time.Second
	retryDelay    = 30 * time.Second
	finishTimeout = 60 * time.Second
)

type Service struct {
	binding string
	journal Journal
	driver  Driver
	index   Index
}

func New(binding string, journal Journal, driver Driver, index Index) *Service {
	return &Service{binding: binding, journal: journal, driver: driver, index: index}
}
func newID() (string, error) {
	var b [20]byte
	_, err := rand.Read(b[:])
	return "bu_" + hex.EncodeToString(b[:]), err
}

func (s *Service) Begin(ctx context.Context, owner, path string, size int64, mime string, modifiedAt int64) (Session, error) {
	normalized, err := paths.Normalize(path)
	if err != nil || normalized == "" || size < 0 || size > 9007199254740991 || owner == "" || s.binding == "" || !safeMime(mime) {
		return Session{}, fail(400, "invalid upload target or size")
	}
	if s.journal == nil || s.driver == nil || s.index == nil {
		return Session{}, fail(503, "upload service unavailable")
	}
	id, err := newID()
	if err != nil {
		return Session{}, err
	}
	session := Session{ID: id, Binding: s.binding, OwnerID: owner, Path: normalized, Size: size, Mime: mime, ModifiedAt: modifiedAt, State: "open", Checkpoint: json.RawMessage(`{}`), ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.journal.Create(ctx, session); err != nil {
		return Session{}, err
	}
	return session, nil
}
func (s *Service) CheckConflict(ctx context.Context, owner, path string, subtree bool) error {
	return s.journal.CheckConflict(ctx, s.binding, owner, path, subtree)
}

// Receive consumes a body once. Retries query Confirm and obtain a fresh upload
// after a definitive 410; they never stream different bytes into the same intent.
func (s *Service) Receive(ctx context.Context, owner, id string, body io.Reader, contentLength int64) (json.RawMessage, error) {
	session, err := s.journal.Get(ctx, s.binding, owner, id)
	if err != nil {
		return nil, err
	}
	if session.State == "completed" {
		return session.Result, nil
	}
	if session.State != "open" {
		return nil, lifecycleError(session.State)
	}
	if time.Now().After(session.ExpiresAt) {
		return nil, fail(410, "upload expired; start a new upload")
	}
	if contentLength != session.Size || body == nil {
		return nil, fail(400, "upload body length does not match the declared size")
	}
	session, err = s.journal.Claim(ctx, s.binding, owner, id, leaseTTL)
	if err != nil {
		return nil, err
	}
	if session.State != "open" {
		s.release(session)
		return nil, lifecycleError(session.State)
	}
	work, stop := s.keepLease(ctx, session)
	defer stop()
	session.State = "receiving"
	if err = s.save(work, &session, time.Now().Add(leaseTTL)); err != nil {
		return nil, err
	}
	checkpoint := func(checkCtx context.Context, value json.RawMessage) error {
		if !validJSON(value) {
			return fail(500, "invalid upload checkpoint")
		}
		next := session
		next.Checkpoint = append(json.RawMessage(nil), value...)
		if err := s.journal.Save(checkCtx, next, time.Now().Add(leaseTTL)); err != nil {
			return err
		}
		session = next
		return nil
	}
	stage, stageErr := s.driver.Stage(work, session, body, checkpoint)
	if stageErr == nil {
		stageErr = work.Err()
	}
	if stageErr != nil {
		stop()
		durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancel()
		if err := s.abort(durable, &session); err != nil {
			return nil, err
		}
		return nil, fail(410, "upload body was interrupted; start a new upload")
	}
	if !validJSON(stage) {
		return nil, fail(500, "invalid staged upload checkpoint")
	}
	// Reception is complete. A closed browser must not cancel final publication
	// or the immediately following database write. The independent lease heartbeat
	// continues for this bounded detached phase.
	stop()
	durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	finish, finishStop := s.keepLease(durable, session)
	defer finishStop()
	session.State = "publishing"
	session.Checkpoint = append(json.RawMessage(nil), stage...)
	if err = s.save(finish, &session, time.Now().Add(leaseTTL)); err != nil {
		return nil, err
	}
	applied, publishErr := s.driver.Publish(finish, session, stage)
	if applied {
		session.State = "applied"
		if err = s.save(finish, &session, time.Now().Add(retryDelay)); err != nil {
			return nil, err
		}
		return s.indexApplied(finish, &session, true)
	}
	// Even a timeout after native publication starts is not an abort. Persist the
	// uncertainty before inspection, and never call Publish again during recovery.
	session.State = "unknown"
	if err = s.save(finish, &session, time.Now().Add(retryDelay)); err != nil {
		return nil, err
	}
	result, recoverErr := s.inspect(finish, &session)
	if recoverErr != nil {
		return nil, recoverErr
	}
	if result != nil {
		return result, nil
	}
	if publishErr != nil {
		return nil, fail(409, "upload publication outcome is unknown; retry confirmation")
	}
	return nil, lifecycleError(session.State)
}

func (s *Service) Confirm(ctx context.Context, owner, id string) (json.RawMessage, error) {
	session, err := s.journal.Get(ctx, s.binding, owner, id)
	if err != nil {
		return nil, err
	}
	if session.State == "completed" {
		return session.Result, nil
	}
	if session.State == "aborted" {
		return nil, lifecycleError(session.State)
	}
	if session.State == "open" {
		return nil, fail(409, "upload body has not been received")
	}
	session, err = s.journal.Claim(ctx, s.binding, owner, id, leaseTTL)
	if err != nil {
		return nil, err
	}
	work, stop := s.keepLease(ctx, session)
	defer stop()
	switch session.State {
	case "completed":
		s.release(session)
		return session.Result, nil
	case "applied":
		return s.indexApplied(work, &session, true)
	case "publishing", "unknown":
		return s.inspect(work, &session)
	case "receiving", "aborting":
		if err = s.abort(work, &session); err != nil {
			return nil, err
		}
		return nil, lifecycleError("aborted")
	default:
		s.release(session)
		return nil, lifecycleError(session.State)
	}
}

func (s *Service) inspect(ctx context.Context, session *Session) (json.RawMessage, error) {
	inspection, err := s.driver.Inspect(ctx, *session)
	if err != nil {
		s.reschedule(*session)
		return nil, err
	}
	if inspection.Applied {
		if inspection.Terminal {
			session.State = "applied"
			if err = s.save(ctx, session, time.Now().Add(retryDelay)); err != nil {
				return nil, err
			}
		}
		return s.indexApplied(ctx, session, inspection.Terminal)
	}
	if inspection.Terminal {
		if err = s.abort(ctx, session); err != nil {
			return nil, err
		}
		return nil, lifecycleError("aborted")
	}
	session.State = "unknown"
	if err = s.save(ctx, session, time.Now().Add(retryDelay)); err != nil {
		return nil, err
	}
	s.release(*session)
	return nil, lifecycleError("unknown")
}
func (s *Service) indexApplied(ctx context.Context, session *Session, terminal bool) (json.RawMessage, error) {
	result := session.Result
	var err error
	if !validJSON(result) {
		result, err = s.index(ctx, *session)
	}
	if err != nil {
		s.reschedule(*session)
		return nil, err
	}
	if !validJSON(result) {
		s.reschedule(*session)
		return nil, fail(500, "invalid upload index result")
	}
	session.Result = append(json.RawMessage(nil), result...)
	if terminal {
		session.State = "completed"
	} else {
		session.State = "unknown"
	}
	next := time.Now().Add(retryDelay)
	if !terminal {
		next = time.Now().Add(5 * time.Minute)
	}
	if err = s.save(ctx, session, next); err != nil {
		return nil, err
	}
	s.release(*session)
	if !terminal {
		return nil, lifecycleError("unknown")
	}
	return result, nil
}
func (s *Service) abort(ctx context.Context, session *Session) error {
	session.State = "aborting"
	if err := s.save(ctx, session, time.Now().Add(retryDelay)); err != nil {
		return err
	}
	if err := s.driver.Discard(ctx, *session); err != nil {
		s.reschedule(*session)
		return err
	}
	session.State = "aborted"
	if err := s.save(ctx, session, time.Now().Add(retryDelay)); err != nil {
		return err
	}
	s.release(*session)
	return nil
}
func (s *Service) save(ctx context.Context, session *Session, next time.Time) error {
	return s.journal.Save(ctx, *session, next)
}
func (s *Service) reschedule(session Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.journal.Save(ctx, session, time.Now().Add(retryDelay))
	s.release(session)
}
func (s *Service) release(session Session) {
	// A zero-duration renewal releases only our own token. It does not release
	// the active path guard or change lifecycle state.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.journal.Renew(ctx, session, 0)
}
func (s *Service) keepLease(ctx context.Context, session Session) (context.Context, func()) {
	work, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(work, 5*time.Second)
				err := s.journal.Renew(renewCtx, session, leaseTTL)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return work, func() { once.Do(func() { cancel(); <-done }) }
}

func (s *Service) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("browser upload recovery", "error", err)
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (s *Service) RunOnce(ctx context.Context) error {
	candidates, err := s.journal.Due(ctx, s.binding, 32)
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		session, err := s.journal.Claim(ctx, s.binding, candidate.OwnerID, candidate.ID, leaseTTL)
		if err != nil {
			var typed *Error
			if !errors.As(err, &typed) || typed.Status != 409 {
				failures = append(failures, err)
			}
			continue
		}
		work, cancel := context.WithTimeout(ctx, finishTimeout)
		active, stop := s.keepLease(work, session)
		switch session.State {
		case "open":
			if time.Now().Before(session.ExpiresAt) {
				session.State = "open"
				err = s.save(active, &session, session.ExpiresAt)
				s.release(session)
			} else {
				err = s.abort(active, &session)
			}
		case "receiving", "aborting":
			err = s.abort(active, &session)
		case "publishing", "unknown":
			_, err = s.inspect(active, &session)
		case "applied":
			_, err = s.indexApplied(active, &session, true)
		default:
			s.release(session)
		}
		stop()
		cancel()
		if err != nil {
			var typed *Error
			if !errors.As(err, &typed) || typed.Status >= 500 {
				failures = append(failures, err)
			}
		}
	}
	if err := s.journal.Cleanup(ctx, s.binding, 128); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// Validate a media type without interpreting it as a path or HTTP header.
func safeMime(value string) bool { return !strings.ContainsAny(value, "\r\n\x00") }
