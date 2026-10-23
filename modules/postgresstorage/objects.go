package postgresstorage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/jackc/pgx/v5"
)

func notExist(key string) error {
	return fmt.Errorf("%w: %s", fs.ErrNotExist, key)
}

func errEmptyKey(operation string) error {
	return fmt.Errorf("postgresql storage: %s with an empty key", operation)
}

// Store puts value at key, creating or overwriting it.
func (s *Storage) Store(ctx context.Context, key string, value []byte) error {
	if key == "" {
		return errEmptyKey("store")
	}
	if value == nil {
		value = []byte{} // the column is NOT NULL; an empty file is still a file
	}
	_, err := s.shared.pool.Exec(ctx,
		`INSERT INTO `+s.objects+` (key, value, modified) VALUES ($1, $2, now())
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, modified = EXCLUDED.modified`,
		key, value)
	if err != nil {
		return fmt.Errorf("postgresql storage: storing %s: %w", key, err)
	}
	return nil
}

// Load returns the value at key, or an error wrapping fs.ErrNotExist.
func (s *Storage) Load(ctx context.Context, key string) ([]byte, error) {
	var value []byte
	err := s.shared.pool.QueryRow(ctx, `SELECT value FROM `+s.objects+` WHERE key = $1`, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notExist(key)
	}
	if err != nil {
		return nil, fmt.Errorf("postgresql storage: loading %s: %w", key, err)
	}
	return value, nil
}

// Delete removes key and, if key is a folder, everything under it.
//
// Deleting a key that does not exist succeeds, as FileStorage's os.RemoveAll does. certmagic relies on
// that: after deleting a site's certificate, key and metadata it deletes the site's folder
// (certmagic v0.25.3 config.go, deleteSiteAssets), and with no files left that folder no longer
// exists here. Returning fs.ErrNotExist would fail that call. The interface asks for an error "only
// if the key still exists when the method returns".
func (s *Storage) Delete(ctx context.Context, key string) error {
	if folderPrefix(key) == "" {
		return errEmptyKey("delete") // never empty the whole table by accident
	}
	_, err := s.shared.pool.Exec(ctx,
		`DELETE FROM `+s.objects+` WHERE key = $1 OR starts_with(key, $2)`,
		key, folderPrefix(key))
	if err != nil {
		return fmt.Errorf("postgresql storage: deleting %s: %w", key, err)
	}
	return nil
}

// Exists reports whether key is a file or a folder (the prefix of at least one file).
func (s *Storage) Exists(ctx context.Context, key string) bool {
	var exists bool
	err := s.shared.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+s.objects+` WHERE key = $1 OR starts_with(key, $2))`,
		key, folderPrefix(key)).Scan(&exists)
	return err == nil && exists
}

// List returns the keys under path: its direct children (files and folders) or, if recursive, every
// descendant including the folders on the way. A path with nothing under it gives fs.ErrNotExist, as
// FileStorage does for a missing directory; the root gives an empty list.
func (s *Storage) List(ctx context.Context, path string, recursive bool) ([]string, error) {
	prefix := folderPrefix(path)
	rows, err := s.shared.pool.Query(ctx,
		`SELECT key FROM `+s.objects+` WHERE starts_with(key, $1) ORDER BY key`, prefix)
	if err != nil {
		return nil, fmt.Errorf("postgresql storage: listing %s: %w", path, err)
	}
	descendants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("postgresql storage: listing %s: %w", path, err)
	}
	if len(descendants) == 0 && prefix != "" {
		if s.Exists(ctx, path) {
			return []string{}, nil // a file has no children
		}
		return nil, notExist(path)
	}
	return listChildren(prefix, descendants, recursive), nil
}

// Stat describes key: a file (IsTerminal, with its size and modification time) or a folder (not
// terminal, modified when the newest file under it was).
func (s *Storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	var size int64
	var modified time.Time
	err := s.shared.pool.QueryRow(ctx,
		`SELECT octet_length(value), modified FROM `+s.objects+` WHERE key = $1`, key).Scan(&size, &modified)
	if err == nil {
		return certmagic.KeyInfo{Key: key, Modified: modified, Size: size, IsTerminal: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return certmagic.KeyInfo{}, fmt.Errorf("postgresql storage: stat %s: %w", key, err)
	}
	prefix := folderPrefix(key)
	if prefix == "" {
		return certmagic.KeyInfo{Key: key, IsTerminal: false}, nil
	}
	var newest *time.Time
	err = s.shared.pool.QueryRow(ctx,
		`SELECT max(modified) FROM `+s.objects+` WHERE starts_with(key, $1)`, prefix).Scan(&newest)
	if err != nil {
		return certmagic.KeyInfo{}, fmt.Errorf("postgresql storage: stat %s: %w", key, err)
	}
	if newest == nil {
		return certmagic.KeyInfo{}, notExist(key)
	}
	return certmagic.KeyInfo{Key: key, Modified: *newest, IsTerminal: false}, nil
}
