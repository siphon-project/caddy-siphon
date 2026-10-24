// Package postgresstorage is a Caddy storage module (caddy.storage.postgresql) that keeps certmagic's
// state (certificates, keys, ACME accounts, HTTP-01 challenge tokens and locks) in PostgreSQL, so
// several Caddy instances can share one ACME state.
package postgresstorage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/certmagic"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(new(Storage))
}

const (
	defaultTablePrefix = "caddy_"
	defaultLockTTL     = 30 * time.Second
	minimumLockTTL     = time.Second
	provisionTimeout   = 30 * time.Second
)

// A prefix is used inside a quoted identifier, but is held to a plain lower-case name anyway: it is
// configuration, not data, and anything else is far more likely a mistake than a wish.
var tablePrefixPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Storage keeps certmagic's state in PostgreSQL.
//
// Two tables, created on provision unless auto_create is false:
//
//	<prefix>objects (key text PRIMARY KEY, value bytea, modified timestamptz)
//	<prefix>locks   (name text PRIMARY KEY, owner text, expires_at timestamptz)
//
// Every time comparison uses the database clock, never the host's, so nodes with skewed clocks agree.
type Storage struct {
	// The PostgreSQL connection string, as a URL or key=value pairs. Placeholders are resolved at
	// provision time, so `{env.CADDY_STORAGE_DSN}` keeps the secret out of the config (a Caddyfile
	// `{$CADDY_STORAGE_DSN}` is substituted when the Caddyfile is adapted, and would write the
	// password into Caddy's autosave.json). TLS follows pgx's `sslmode`, whose default is `prefer`
	// (tries TLS, verifies nothing, falls back to plaintext); use `sslmode=verify-full`.
	DSN string `json:"dsn,omitempty"`

	// Prefix for the two table names. Default `caddy_`. To keep the tables in a schema of their own,
	// set the search path in the DSN instead: pgx sends an unknown DSN parameter to the server as a
	// run-time setting, so `...?sslmode=verify-full&search_path=caddy` puts both tables in schema caddy.
	TablePrefix string `json:"table_prefix,omitempty"`

	// How long a lock lives without a heartbeat. A holder renews it every third of this, so it
	// expires only when its holder has died or lost the database. Default 30s, minimum 1s.
	LockTTL caddy.Duration `json:"lock_ttl,omitempty"`

	// Whether to create the tables on provision (default true). Set false when the role has no
	// CREATE privilege; the tables must then exist, and provisioning checks that they do.
	AutoCreate *bool `json:"auto_create,omitempty"`

	logger      *zap.Logger
	shared      *sharedPool
	objects     string // quoted table names
	locks       string
	ttl         time.Duration
	description string

	mu        sync.Mutex
	held      map[string]*heldLock
	cleanedUp bool
}

// CaddyModule returns the Caddy module information.
func (*Storage) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.storage.postgresql",
		New: func() caddy.Module { return new(Storage) },
	}
}

// Provision resolves the DSN, opens (or joins) the connection pool and makes sure the tables exist.
func (s *Storage) Provision(ctx caddy.Context) error {
	s.logger = ctx.Logger()
	repl := caddy.NewReplacer()
	dsn := repl.ReplaceKnown(s.DSN, "")
	ctxTimeout, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	return s.open(ctxTimeout, dsn)
}

// open does the work of Provision once the DSN is resolved; tests call it directly.
func (s *Storage) open(ctx context.Context, dsn string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if dsn == "" {
		return errors.New("postgresql storage: the dsn resolved to an empty string")
	}
	prefix := s.TablePrefix
	if prefix == "" {
		prefix = defaultTablePrefix
	}
	s.ttl = time.Duration(s.LockTTL)
	if s.ttl == 0 {
		s.ttl = defaultLockTTL
	}
	s.objects = pgx.Identifier{prefix + "objects"}.Sanitize()
	s.locks = pgx.Identifier{prefix + "locks"}.Sanitize()
	s.description = fmt.Sprintf("postgresql storage (tables %sobjects, %slocks)", prefix, prefix)
	s.held = make(map[string]*heldLock)
	if s.logger == nil {
		s.logger = zap.NewNop()
	}

	shared, err := acquirePool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("postgresql storage: %w", err)
	}
	if s.AutoCreate == nil || *s.AutoCreate {
		err = s.createTables(ctx, shared, prefix)
	} else {
		err = s.checkTables(ctx, shared)
	}
	if err != nil {
		shared.release()
		return fmt.Errorf("postgresql storage: %w", err)
	}
	s.shared = shared
	return nil
}

// Validate checks the configuration without touching the database.
func (s *Storage) Validate() error {
	if s.DSN == "" {
		return errors.New("postgresql storage: dsn is required")
	}
	if s.TablePrefix != "" {
		if !tablePrefixPattern.MatchString(s.TablePrefix) {
			return fmt.Errorf("postgresql storage: table_prefix %q must be lower-case letters, digits and underscores, not starting with a digit", s.TablePrefix)
		}
		// PostgreSQL truncates identifiers past 63 bytes; "objects" is the longer suffix.
		if len(s.TablePrefix)+len("objects") > 63 {
			return fmt.Errorf("postgresql storage: table_prefix %q is too long", s.TablePrefix)
		}
	}
	if ttl := time.Duration(s.LockTTL); ttl != 0 && ttl < minimumLockTTL {
		return fmt.Errorf("postgresql storage: lock_ttl %s is below the minimum of %s", ttl, minimumLockTTL)
	}
	return nil
}

// createTables creates both tables if they are missing. Two nodes starting at once would race on
// CREATE TABLE IF NOT EXISTS (it is not atomic against itself), so the creation runs under a
// transaction-scoped advisory lock keyed on the table names.
func (s *Storage) createTables(ctx context.Context, shared *sharedPool, prefix string) error {
	transaction, err := shared.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("creating tables: %w", err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()
	statements := []string{
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		`CREATE TABLE IF NOT EXISTS ` + s.objects + ` (
			key      text COLLATE "C" PRIMARY KEY,
			value    bytea NOT NULL,
			modified timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS ` + s.locks + ` (
			name       text COLLATE "C" PRIMARY KEY,
			owner      text NOT NULL,
			expires_at timestamptz NOT NULL
		)`,
	}
	if _, err := transaction.Exec(ctx, statements[0], "caddy-storage-schema:"+prefix); err != nil {
		return fmt.Errorf("creating tables: %w", err)
	}
	for _, statement := range statements[1:] {
		if _, err := transaction.Exec(ctx, statement); err != nil {
			return fmt.Errorf("creating tables (set auto_create false if the role may not create them): %w", err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("creating tables: %w", err)
	}
	return nil
}

// checkTables fails provisioning early when auto_create is off and a table is missing or unreadable.
func (s *Storage) checkTables(ctx context.Context, shared *sharedPool) error {
	for _, table := range []string{s.objects, s.locks} {
		rows, err := shared.pool.Query(ctx, `SELECT 1 FROM `+table+` LIMIT 0`)
		if err != nil {
			return fmt.Errorf("auto_create is off and table %s is not usable: %w", table, err)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("auto_create is off and table %s is not usable: %w", table, err)
		}
	}
	return nil
}

// CertMagicStorage returns the storage itself.
func (s *Storage) CertMagicStorage() (certmagic.Storage, error) {
	return s, nil
}

// Cleanup gives up this instance's reference to the connection pool. It does not release held
// locks, stop their heartbeats or close the pool under them: see sharedPool for why.
func (s *Storage) Cleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleanedUp || s.shared == nil {
		return nil
	}
	s.cleanedUp = true
	s.shared.release()
	return nil
}

// String describes the storage without the DSN: certmagic logs storage values (zap.Any), and the
// struct holds the password.
func (s *Storage) String() string {
	if s.description == "" {
		return "postgresql storage"
	}
	return s.description
}

// UnmarshalCaddyfile sets up the storage from Caddyfile tokens:
//
//	storage postgresql {
//		dsn          "{env.CADDY_STORAGE_DSN}"
//		table_prefix caddy_
//		lock_ttl     30s
//		auto_create  true|false
//	}
func (s *Storage) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the module name
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		directive := d.Val()
		if !d.NextArg() {
			return d.ArgErr()
		}
		value := d.Val()
		if d.NextArg() {
			return d.ArgErr()
		}
		switch directive {
		case "dsn":
			s.DSN = value
		case "table_prefix":
			s.TablePrefix = value
		case "lock_ttl":
			ttl, err := caddy.ParseDuration(value)
			if err != nil {
				return d.Errf("lock_ttl: %v", err)
			}
			s.LockTTL = caddy.Duration(ttl)
		case "auto_create":
			switch value {
			case "true":
				s.AutoCreate = nil
			case "false":
				off := false
				s.AutoCreate = &off
			default:
				return d.Errf("auto_create must be true or false, not %q", value)
			}
		default:
			return d.Errf("unrecognized parameter %q", directive)
		}
	}
	if s.DSN == "" {
		return d.Err("dsn is required")
	}
	return nil
}

// Interface guards
var (
	_ caddy.Module               = (*Storage)(nil)
	_ caddy.Provisioner          = (*Storage)(nil)
	_ caddy.Validator            = (*Storage)(nil)
	_ caddy.CleanerUpper         = (*Storage)(nil)
	_ caddy.StorageConverter     = (*Storage)(nil)
	_ caddyfile.Unmarshaler      = (*Storage)(nil)
	_ certmagic.Storage          = (*Storage)(nil)
	_ certmagic.TryLocker        = (*Storage)(nil)
	_ certmagic.LockLeaseRenewer = (*Storage)(nil)
	_ fmt.Stringer               = (*Storage)(nil)
)
