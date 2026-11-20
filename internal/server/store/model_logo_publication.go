package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

// WithModelLogoPublication reserves a prospective reference and holds local
// business admission until publication, started renewals and any failed-ticket
// release finish. Renewal keeps the original ticket; it cannot revive expiry.
// publish must check before making each new file name visible. It must return
// only when its actual I/O has finished; a canceled wait is not a drained writer.
// This local barrier does not certify cross-member file ownership or cleanup.
func (s *Store) WithModelLogoPublication(ctx context.Context, name, owner string, ttl time.Duration, publish func(check func() error) error) (LogoReservation, error) {
	if publish == nil {
		return LogoReservation{}, ErrLogoGCInvalid
	}
	check := func() error { return ctx.Err() }
	if b := s.businessWriteBarrier; b != nil {
		release, err := b.acquire()
		if err != nil {
			return LogoReservation{}, err
		}
		defer release()
		check = func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return b.check(ctx)
		}
	}
	if err := check(); err != nil {
		return LogoReservation{}, err
	}
	r, err := s.reserveModelLogoAttempt(ctx, name, owner, ttl)
	if err != nil {
		// Commit may have succeeded without delivering acknowledgement.
		// Only the original tuple may be used, and only to cancel it; never
		// read it back as publication authority. Retain admission until this
		// actual cancellation returns, including closed-state cleanup.
		if r.Generation > 0 {
			err = errors.Join(err, s.CancelModelLogoReservation(context.Background(), r))
		}
		return LogoReservation{}, err
	}
	admitted := check
	var mu sync.Mutex
	var failed error
	latch := func(err error) error {
		mu.Lock()
		defer mu.Unlock()
		if failed == nil {
			failed = err
		}
		return failed
	}
	check = func() error {
		if err := latch(admitted()); err != nil {
			return err
		}
		// A renewal may still be blocked in durable I/O. Check the actual
		// original ticket rather than borrowing its unconfirmed extension.
		started := time.Now().UTC()
		var row struct {
			ExpiresAt int64
			Now       float64
		}
		err := s.reader().WithContext(ctx).Raw(`SELECT r.expires_at, (julianday('now') - 2440587.5) * 86400.0 AS now FROM model_logo_reservations AS r JOIN model_logo_gc AS g ON g.digest = r.digest WHERE r.token = ? AND r.name = ? AND r.owner = ? AND r.generation = ? AND g.generation = r.generation AND g.deleting_name = '' AND r.expires_at > CAST(strftime('%s','now') AS integer)`, r.Token, r.Name, r.Owner, r.Generation).Scan(&row).Error
		// Result delivery can itself be slow. Anchor the DB's remaining time
		// before the call, conservatively including query delay, and allow for
		// fractional clock precision. A late live row is no longer authority.
		remaining := time.Duration((float64(row.ExpiresAt) - row.Now) * float64(time.Second))
		deadline := started.Add(remaining - time.Millisecond)
		if err == nil {
			err = admitted()
		}
		if err == nil && (row.ExpiresAt == 0 || !time.Now().UTC().Before(deadline)) {
			err = ErrLogoGCStale
		}
		return latch(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		interval := ttl / 3
		if interval <= 0 {
			interval = time.Nanosecond
		}
		timer := time.NewTicker(interval)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				_ = latch(ctx.Err())
				return
			case <-timer.C:
			}
			// No lifecycle lock is held over the real transaction. Stop does
			// not cancel a started call or substitute for its actual return.
			select {
			case <-stop:
				return
			default:
			}
			if latch(admitted()) != nil {
				return
			}
			if err := s.RenewModelLogoReservation(ctx, r, ttl); err != nil {
				_ = latch(err)
				return
			}
			if latch(admitted()) != nil {
				return
			}
		}
	}()
	// The reservation transaction can commit just before admission is revoked.
	// Recheck before entering the publisher rather than treating a live ticket
	// as service eligibility.
	if err = check(); err == nil {
		err = publish(check)
	}
	close(stop)
	<-done
	// Refresh once at handoff through the existing write admission. A local
	// live read alone does not prove a fresh successful renewal transaction.
	if err == nil && latch(admitted()) == nil {
		_ = latch(s.RenewModelLogoReservation(ctx, r, ttl))
	}
	err = errors.Join(err, check())
	if err != nil {
		// Releasing this exact ticket is allowed while the service is closed.
		// Keep admission active through release, including slow replicated I/O.
		return LogoReservation{}, errors.Join(err, s.CancelModelLogoReservation(context.Background(), r))
	}
	return r, nil
}
