package postgresstorage

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// One connection pool per DSN, shared by every storage instance and every held lock that uses it,
// and closed when the last of them lets go.
//
// Why reference counting: on a config reload Caddy provisions the new storage, starts the new
// config, then cleans up the old one. An issuance the old config started can still be running at
// that point: it holds a lock (kept alive by a heartbeat) and is about to Store the certificate it
// paid an ACME order for, through the old storage instance, and it Unlocks through that instance
// too. Closing the pool in Cleanup would lose that certificate and leave the lock to expire, so
// another node would order the same certificate again. So Cleanup gives up only the instance's own
// reference; each held lock owns a reference of its own until Unlock. With the DSN unchanged, a
// reload simply goes from two references back to one and the pool is never closed.
type sharedPool struct {
	key  string
	pool *pgxpool.Pool
	refs int
}

var registry = struct {
	mu    sync.Mutex
	pools map[string]*sharedPool
}{pools: make(map[string]*sharedPool)}

// acquirePool returns the pool for dsn with one reference taken, opening and pinging it if this is
// the first user.
func acquirePool(ctx context.Context, dsn string) (*sharedPool, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if shared, ok := registry.pools[dsn]; ok {
		shared.refs++
		return shared, nil
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx's parse error can quote the DSN, password included; say only that it is malformed.
		return nil, fmt.Errorf("the dsn is not a valid PostgreSQL connection string")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("opening the connection pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	shared := &sharedPool{key: dsn, pool: pool, refs: 1}
	registry.pools[dsn] = shared
	return shared, nil
}

// retain takes one more reference.
func (shared *sharedPool) retain() {
	registry.mu.Lock()
	shared.refs++
	registry.mu.Unlock()
}

// release gives up one reference and closes the pool when it was the last.
func (shared *sharedPool) release() {
	registry.mu.Lock()
	shared.refs--
	last := shared.refs == 0
	if last {
		delete(registry.pools, shared.key)
	}
	registry.mu.Unlock()
	if last {
		shared.pool.Close()
	}
}

// poolReferences reports the reference count for dsn (0 when no pool is open). For tests.
func poolReferences(dsn string) int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if shared, ok := registry.pools[dsn]; ok {
		return shared.refs
	}
	return 0
}
