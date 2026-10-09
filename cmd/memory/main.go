package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/database"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/ingestion"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
)

func main() {
	if err := run(); err != nil {
		slog.Error("memory generation failed", "reason", err.Error())
		os.Exit(1)
	}
}

func run() error {
	samples := flag.Bool("samples", false, "select and export three dataset buyers and three sellers")
	glid := flag.String("glid", "", "explicit imported subject GLID")
	role := flag.String("role", "", "buyer or seller for an explicit GLID")
	namespace := flag.String("namespace", ingestion.Namespace, "historical_dataset or synthetic_demo")
	asOf := flag.String("as-of", "", "source calendar date YYYY-MM-DD; default is latest actual activity date")
	activity := flag.Int("activity-days", 90, "activity calendar-day lookback")
	conversation := flag.Int("conversation-days", 30, "conversation calendar-day lookback")
	budget := flag.Int("max-bytes", 6000, "maximum UTF-8 Markdown size")
	thread := flag.String("thread", "", "explicit requirement thread for a single subject")
	flag.Parse()
	if !*samples && (*glid == "" || *role == "") {
		return fmt.Errorf("use -samples or provide -glid and -role")
	}
	if *samples && (*glid != "" || *role != "" || *thread != "") {
		return fmt.Errorf("sample selection cannot be combined with explicit scope or thread")
	}
	if *namespace != ingestion.Namespace && *namespace != "synthetic_demo" {
		return fmt.Errorf("unsupported local memory namespace")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, err := database.Open(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if *asOf == "" {
		*asOf, err = memory.ReferenceDate(ctx, pool, ingestion.Tenant, *namespace)
		if err != nil {
			return err
		}
	}
	policy := memory.Policy{AsOfDate: *asOf, ActivityDays: *activity, ConversationDays: *conversation, MaxBytes: *budget, ThreadRef: *thread}
	if err := policy.Validate(); err != nil {
		return err
	}
	var subjects []memory.Candidate
	if *samples {
		subjects, err = memory.SelectSamples(ctx, pool, ingestion.Tenant, *namespace, 3, policy)
		if err != nil {
			return err
		}
	} else {
		subjects = []memory.Candidate{{Scope: memory.Scope{Tenant: ingestion.Tenant, Namespace: *namespace, GLID: *glid, Role: *role}}}
	}
	root := filepath.Join("artifacts", "exports")
	exports := []memory.Export{}
	var reused, totalBytes, maxBytes int
	for _, candidate := range subjects {
		snapshot, err := memory.Generate(ctx, pool, candidate.Scope, policy)
		if err != nil {
			return err
		}
		export, err := memory.ExportSnapshot(ctx, pool, snapshot, root)
		if err != nil {
			return err
		}
		exports = append(exports, export)
		totalBytes += export.Bytes
		if export.Bytes > maxBytes {
			maxBytes = export.Bytes
		}
		if export.Reused {
			reused++
		}
	}
	manifest := struct {
		RendererVersion string             `json:"renderer_version"`
		Policy          memory.Policy      `json:"selection_policy"`
		GeneratedAt     time.Time          `json:"exported_at"`
		Subjects        []memory.Candidate `json:"subjects"`
		Exports         []memory.Export    `json:"exports"`
	}{memory.RendererVersion, policy, time.Now().UTC(), subjects, exports}
	// Keep the six-subject manifest when generating a one-off thread inspection.
	if *samples {
		if err := memory.SaveManifest(root, *namespace, manifest); err != nil {
			return err
		}
	}
	slog.Info("memory files ready", "files", len(exports), "reused_snapshots", reused, "as_of_source_date", *asOf, "total_bytes", totalBytes, "largest_file_bytes", maxBytes, "directory", filepath.Join(root, *namespace))
	return nil
}
