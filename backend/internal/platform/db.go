package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"sort"

	"agentowl/backend/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Open(ctx context.Context, c Config) (*pgxpool.Pool, error) {
	p, e := pgxpool.New(ctx, c.DatabaseURL)
	if e != nil {
		return nil, e
	}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, e
	}
	return p, nil
}
func Migrate(ctx context.Context, p *pgxpool.Pool) error {
	conn, e := p.Acquire(ctx)
	if e != nil {
		return e
	}
	defer conn.Release()
	if _, e = conn.Exec(ctx, "SELECT pg_advisory_lock(728690103)"); e != nil {
		return e
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(728690103)")
	if _, e = conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())"); e != nil {
		return e
	}
	entries, e := migrations.Files.ReadDir(".")
	if e != nil {
		return e
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		b, e := migrations.Files.ReadFile(entry.Name())
		if e != nil {
			return e
		}
		sum := sha256.Sum256(b)
		digest := hex.EncodeToString(sum[:])
		var old string
		e = conn.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE name=$1", entry.Name()).Scan(&old)
		if e == nil {
			if old != digest {
				return fmt.Errorf("migration checksum changed: %s", entry.Name())
			}
			continue
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, string(b)); e != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", entry.Name(), e)
		}
		if _, e = tx.Exec(ctx, "INSERT INTO schema_migrations(name,checksum) VALUES($1,$2)", entry.Name(), digest); e != nil {
			tx.Rollback(ctx)
			return e
		}
		if e = tx.Commit(ctx); e != nil {
			return e
		}
	}
	return nil
}
