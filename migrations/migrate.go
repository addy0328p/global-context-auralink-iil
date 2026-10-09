// Package migrations applies the embedded, ordered SQL changes explicitly.
package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var files embed.FS

// Apply runs under a transaction and a shared lock. Concurrent runners cannot
// apply the same change twice, and a SQL failure rolls back this entire batch.
func Apply(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, errors.New("could not begin migration transaction")
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(724611923)"); err != nil {
		return nil, errors.New("could not acquire migration lock")
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
	)`); err != nil {
		return nil, errors.New("could not initialize migration history")
	}
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, errors.New("could not list embedded migrations")
	}
	var applied []string
	for _, name := range names {
		body, err := files.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("could not read migration %s", name)
		}
		// Git may check SQL files out with CRLF on Windows. Ignore that
		// platform difference when comparing an already-applied migration.
		body = []byte(strings.ReplaceAll(string(body), "\r\n", "\n"))
		hash := sha256.Sum256(body)
		checksum := hex.EncodeToString(hash[:])
		var recorded string
		err = tx.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE name = $1", name).Scan(&recorded)
		if err == nil {
			if recorded != checksum {
				return nil, fmt.Errorf("applied migration %s was changed; add a new migration instead", name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("could not read migration history")
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return nil, fmt.Errorf("migration %s failed; no changes committed", name)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(name, checksum) VALUES ($1, $2)", name, checksum); err != nil {
			return nil, errors.New("could not record migration")
		}
		applied = append(applied, name)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New("could not commit migrations")
	}
	return applied, nil
}
