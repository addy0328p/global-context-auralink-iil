package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/database"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/ingestion"
)

func main() {
	if err := run(); err != nil {
		slog.Error("dataset import failed", "reason", err.Error())
		os.Exit(1)
	}
}

func run() error {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not load local configuration")
	}
	seller := flag.String("seller-zip", os.Getenv("SELLER_DATASET_ZIP"), "local seller ZIP path")
	buyer := flag.String("buyer-csv", os.Getenv("BUYER_DATASET_CSV"), "local buyer CSV path")
	zone := flag.String("source-timezone", os.Getenv("SOURCE_TIMEZONE"), "confirmed source timezone; blank preserves unresolved timestamps")
	dry := flag.Bool("dry-run", false, "validate files and report counts without database changes")
	status := flag.Bool("status", false, "show aggregate database counts without reading or importing files")
	flag.Parse()
	if *status {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pool, err := database.Open(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		stats, err := ingestion.Verify(ctx, pool)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(stats, "", "  ")
		if err != nil {
			return fmt.Errorf("could not encode dataset status")
		}
		fmt.Println(string(b))
		if !stats.Consistent {
			return fmt.Errorf("dataset revision verification failed")
		}
		return nil
	}
	if *seller == "" || *buyer == "" {
		return fmt.Errorf("set SELLER_DATASET_ZIP and BUYER_DATASET_CSV in .env or supply path flags")
	}
	slog.Info("reading local source files", "dry_run", *dry)
	dataset, err := ingestion.Parse(ingestion.Options{SellerZIP: *seller, BuyerCSV: *buyer, SourceTimezone: *zone})
	if err != nil {
		return err
	}
	report := ingestion.Report{MappingVersion: ingestion.MappingVersion, SourceTimezone: *zone}
	if *dry {
		for _, file := range dataset.Files {
			report.Files = append(report.Files, file.Report)
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		pool, err := database.Open(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		report, err = ingestion.Store(ctx, pool, dataset, *zone)
		if err != nil {
			return err
		}
	}
	dir := filepath.Join("artifacts", "imports")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("could not create local report folder")
	}
	mode := "import"
	if *dry {
		mode = "audit"
	}
	filename := filepath.Join(dir, time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+mode+".json")
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("could not encode import report")
	}
	if err := os.WriteFile(filename, append(b, '\n'), 0600); err != nil {
		return fmt.Errorf("could not save local import report")
	}
	var records, accepted, quarantined, events, newEvents, newProfiles, skipped int
	for _, f := range report.Files {
		records += f.Records
		accepted += f.Accepted
		quarantined += f.Quarantined
		events += f.Events
		newEvents += f.NewEvents
		newProfiles += f.NewProfiles
		if f.Skipped {
			skipped++
		}
	}
	slog.Info("dataset validation complete", "source_records", records, "accepted", accepted, "quarantined", quarantined, "normalized_events", events)
	if !*dry {
		slog.Info("database import complete", "new_profiles", newProfiles, "new_events", newEvents, "files_already_imported", skipped)
	}
	slog.Info("local audit report saved", "path", filename)
	return nil
}
