package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/database"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx)
	if err != nil {
		slog.Error("seed connection failed", "reason", err.Error())
		os.Exit(1)
	}
	defer pool.Close()
	report, err := database.SeedDemo(ctx, pool)
	if err != nil {
		slog.Error("synthetic seed failed", "reason", err.Error())
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		os.Exit(1)
	}
}
