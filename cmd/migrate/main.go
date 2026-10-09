package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/database"
	"github.com/rohankamewar2/global-context-auralink-iil/migrations"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx)
	if err != nil {
		slog.Error("migration connection failed", "reason", err.Error())
		os.Exit(1)
	}
	defer pool.Close()
	applied, err := migrations.Apply(ctx, pool)
	if err != nil {
		slog.Error("migration failed", "reason", err.Error())
		os.Exit(1)
	}
	if len(applied) == 0 {
		slog.Info("database is already up to date")
		return
	}
	for _, name := range applied {
		slog.Info("migration applied", "name", name)
	}
}
