package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// Open loads local configuration without printing credentials. Process settings
// take precedence over .env, so deployed services need not have that file.
func Open(ctx context.Context) (*pgxpool.Pool, error) {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return nil, errors.New("could not load .env; check its formatting")
	}
	for _, key := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSSLMODE"} {
		value := os.Getenv(key)
		if value == "" || value == "REPLACE_ME" {
			return nil, fmt.Errorf("set %s in .env or the process environment", key)
		}
	}
	config, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, errors.New("invalid PostgreSQL configuration")
	}
	config.MaxConns = 5
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("could not initialize PostgreSQL connection pool")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(checkCtx); err != nil {
		pool.Close()
		return nil, errors.New("PostgreSQL connection failed; check credentials, host, port, and server status")
	}
	return pool, nil
}
