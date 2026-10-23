package postgresstorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"time"

	"go.uber.org/zap"
)

// Locks
//
// A lock is a row in <prefix>locks: its name, the random id of the holder, and when it expires by
// the database clock. Acquiring is one atomic statement:
//
//	INSERT ... ON CONFLICT (name) DO UPDATE SET owner = ..., expires_at = ...
//	WHERE locks.expires_at <= now() RETURNING owner
//
// It inserts a free lock, takes over an expired one, and leaves a live one alone (the WHERE fails, so
// no row comes back). PostgreSQL serialises the conflicting statements on the row, so two nodes can
// never both get a row back. While the lock is held a heartbeat pushes expires_at forward every third
// of the TTL, so a live holder keeps it however long issuance takes, and a holder that died loses it
// one TTL later. Unlock deletes the row only if the owner is still ours, so a holder whose lock
// expired and was taken over cannot release the new holder's lock.
//
// There are no fencing tokens: a holder cut off from the database for longer than the TTL keeps
// running while another node takes over. certmagic tolerates that ("code relying upon Locker must be
// tolerant of some mis-synchronizations") and re-checks storage after acquiring a lock.

// pollInterval is the mean wait between attempts while Lock blocks; each wait is jittered by ±50% so
// waiters on several nodes do not retry in step.
const pollInterval = 500 * time.Millisecond

type heldLock struct {
	owner  string
	cancel context.CancelFunc
	done   chan struct{}
}

func newOwner() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("postgresql storage: generating a lock owner id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func jitter(mean time.Duration) time.Duration {
	return mean/2 + time.Duration(mathrand.Int64N(int64(mean)))
}

// tryAcquire makes one attempt and reports whether the lock is now owner's.
func (s *Storage) tryAcquire(ctx context.Context, name, owner string) (bool, error) {
	rows, err := s.shared.pool.Query(ctx,
		`INSERT INTO `+s.locks+` AS l (name, owner, expires_at)
		 VALUES ($1, $2, now() + $3 * interval '1 millisecond')
		 ON CONFLICT (name) DO UPDATE SET owner = EXCLUDED.owner, expires_at = EXCLUDED.expires_at
		 WHERE l.expires_at <= now()
		 RETURNING owner`,
		name, owner, s.ttl.Milliseconds())
	if err != nil {
		return false, fmt.Errorf("postgresql storage: acquiring lock %s: %w", name, err)
	}
	defer rows.Close()
	acquired := false
	for rows.Next() {
		var got string
		if err := rows.Scan(&got); err != nil {
			return false, fmt.Errorf("postgresql storage: acquiring lock %s: %w", name, err)
		}
		acquired = got == owner
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("postgresql storage: acquiring lock %s: %w", name, err)
	}
	return acquired, nil
}

// Lock blocks until the named lock is acquired or ctx ends.
func (s *Storage) Lock(ctx context.Context, name string) error {
	owner, err := newOwner()
	if err != nil {
		return err
	}
	for {
		acquired, err := s.tryAcquire(ctx, name, owner)
		if err != nil {
			return err
		}
		if acquired {
			s.hold(name, owner)
			return nil
		}
		timer := time.NewTimer(jitter(pollInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// TryLock makes one attempt to acquire the named lock.
func (s *Storage) TryLock(ctx context.Context, name string) (bool, error) {
	owner, err := newOwner()
	if err != nil {
		return false, err
	}
	acquired, err := s.tryAcquire(ctx, name, owner)
	if err != nil || !acquired {
		return false, err
	}
	s.hold(name, owner)
	return true, nil
}

// hold records an acquired lock and starts its heartbeat. The lock takes its own reference to the
// pool, so Cleanup on a reload cannot close the pool under it.
func (s *Storage) hold(name, owner string) {
	s.shared.retain()
	heartbeatContext, cancel := context.WithCancel(context.Background())
	lock := &heldLock{owner: owner, cancel: cancel, done: make(chan struct{})}

	s.mu.Lock()
	previous := s.held[name]
	s.held[name] = lock
	s.mu.Unlock()
	if previous != nil {
		// The database only hands us this lock if the previous holder's row expired, so that holder
		// had already lost it; stop its heartbeat rather than leave it running.
		s.logger.Warn("a lock this instance held had expired and was acquired again",
			zap.String("lock", name))
		s.stopHeartbeat(previous)
	}
	go s.heartbeat(heartbeatContext, name, lock)
}

func (s *Storage) stopHeartbeat(lock *heldLock) {
	lock.cancel()
	<-lock.done
	s.shared.release()
}

// heartbeat renews the lock every third of the TTL until cancelled, or until it finds the lock is no
// longer ours.
func (s *Storage) heartbeat(ctx context.Context, name string, lock *heldLock) {
	defer close(lock.done)
	ticker := time.NewTicker(s.ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		renewContext, cancel := context.WithTimeout(ctx, s.ttl/3)
		owned, err := s.extend(renewContext, name, lock.owner, s.ttl)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			// Keep trying: the lock is still ours until it expires, and the database may come back.
			s.logger.Error("renewing lock", zap.String("lock", name), zap.Error(err))
		case !owned:
			s.logger.Error("lock was lost: it expired and another holder may have it",
				zap.String("lock", name))
			return
		}
	}
}

// extend pushes the lock's expiry to at least now()+duration, if owner still holds it.
func (s *Storage) extend(ctx context.Context, name, owner string, duration time.Duration) (bool, error) {
	tag, err := s.shared.pool.Exec(ctx,
		`UPDATE `+s.locks+`
		 SET expires_at = GREATEST(expires_at, now() + $3 * interval '1 millisecond')
		 WHERE name = $1 AND owner = $2`,
		name, owner, duration.Milliseconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Unlock releases the named lock, if this instance holds it. The row is deleted only while it is
// still ours: if the lock expired and another node took it, that node keeps it.
func (s *Storage) Unlock(ctx context.Context, name string) error {
	s.mu.Lock()
	lock := s.held[name]
	delete(s.held, name)
	s.mu.Unlock()
	if lock == nil {
		return fmt.Errorf("postgresql storage: lock %s is not held by this instance", name)
	}
	lock.cancel()
	<-lock.done
	defer s.shared.release()
	tag, err := s.shared.pool.Exec(ctx,
		`DELETE FROM `+s.locks+` WHERE name = $1 AND owner = $2`, name, lock.owner)
	if err != nil {
		// The heartbeat is stopped, so the row expires within one TTL anyway.
		return fmt.Errorf("postgresql storage: releasing lock %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		s.logger.Warn("releasing a lock that had already expired and may be held by another node",
			zap.String("lock", name))
	}
	return nil
}

// RenewLockLease confirms the lock is still held and extends it.
//
// certmagic asks for leases up to hours long (its retry back-off plus the obtain timeout). The lease
// is capped at the TTL on purpose: the heartbeat already keeps a live holder's lock for as long as it
// needs, so a longer lease would only ever matter for a holder that died, and would then keep every
// other node from taking over that certificate for hours. What the call does buy is the check: it
// fails if the lock has been lost.
func (s *Storage) RenewLockLease(ctx context.Context, name string, _ time.Duration) error {
	s.mu.Lock()
	lock := s.held[name]
	s.mu.Unlock()
	if lock == nil {
		return fmt.Errorf("postgresql storage: lock %s is not held by this instance", name)
	}
	owned, err := s.extend(ctx, name, lock.owner, s.ttl)
	if err != nil {
		return fmt.Errorf("postgresql storage: renewing lock %s: %w", name, err)
	}
	if !owned {
		return errLockLost(name)
	}
	return nil
}

var errLost = errors.New("lock was lost")

func errLockLost(name string) error {
	return fmt.Errorf("postgresql storage: %w: %s expired and may be held by another node", errLost, name)
}
