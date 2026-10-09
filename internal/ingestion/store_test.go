package ingestion

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/database"
	"github.com/rohankamewar2/global-context-auralink-iil/migrations"
)

func TestImportDatabase(t *testing.T) {
	if os.Getenv("AURALINK_RUN_DB_TESTS") != "1" {
		t.Skip("opt-in isolated PostgreSQL integration test")
	}
	if err := godotenv.Load("../../.env"); err != nil && !os.IsNotExist(err) {
		t.Fatal("could not load test configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := database.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "auralink_import_test_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("could not create test schema")
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("could not clean up test-created schema")
		}
	}()
	cfg := admin.Config().Copy()
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("could not open isolated pool")
	}
	defer pool.Close()
	if _, err := migrations.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	file := newFile("generated-test-fixture.csv", digest([]byte("fixture-v1")))
	file.row(1, map[string]any{"fixture": true}, map[string]any{"fixture": true}, "")
	file.row(2, map[string]any{"fixture": true}, map[string]any{"fixture": true}, "test_quarantine")
	file.Profiles = []Profile{{GLID: "101", Role: "seller", Payload: encode(map[string]any{"fixture": true}), Provenance: encode(map[string]any{"fixture": true})}}
	file.event("101", "seller", "test.generated", "one", "conversation.turn", "voice", "test-thread", "", "", map[string]any{"fixture": true})
	file.event("101", "seller", "test.generated", "one", "conversation.turn", "voice", "test-thread", "", "", map[string]any{"fixture": true})
	file.event("101", "buyer", "test.generated", "two", "conversation.turn", "chat", "test-thread", "", "", map[string]any{"fixture": true})
	dataset := &Dataset{Files: []*File{file}}
	first, err := Store(ctx, pool, dataset, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Files[0].NewProfiles != 1 || first.Files[0].NewEvents != 2 {
		t.Fatal("duplicate source events were inserted")
	}
	second, err := Store(ctx, pool, dataset, "")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Files[0].Skipped || second.Files[0].NewEvents != 0 {
		t.Fatal("repeat file was imported again")
	}
	var projections, threads, quarantined, revision int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event_projections").Scan(&projections); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM conversation_threads").Scan(&threads); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM import_rows WHERE quarantine_reason IS NOT NULL").Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT sum(accepted_revision) FROM publication_state").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if projections != 2 || threads != 2 || quarantined != 1 || revision != 2 {
		t.Fatal("audit, role separation, or revision tracking is inconsistent")
	}
	if _, err := Store(ctx, pool, dataset, "Asia/Kolkata"); err == nil {
		t.Fatal("inconsistent historical timezone assumption accepted")
	}
	// A later copy of an event must not advance revisions again, even in another file.
	file2 := *file
	file2.Report = file.Report
	file2.Report.Name = "generated-repeat-fixture.csv"
	file2.Report.SHA256 = digest([]byte("fixture-v2"))
	third, err := Store(ctx, pool, &Dataset{Files: []*File{&file2}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if third.Files[0].NewEvents != 0 {
		t.Fatal("same events in another export were duplicated")
	}
	if err := pool.QueryRow(ctx, "SELECT sum(accepted_revision) FROM publication_state").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 2 {
		t.Fatal("revisions advanced without new events")
	}
	// A failed transaction must not leave source rows or history behind.
	invalid := newFile("generated-invalid-fixture.csv", digest([]byte("invalid")))
	invalid.event("101", "invalid-role", "test.generated", "bad", "conversation.turn", "chat", "", "", "", map[string]any{"fixture": true})
	if _, err := Store(ctx, pool, &Dataset{Files: []*File{invalid}}, ""); err == nil {
		t.Fatal("invalid role was accepted")
	}
	var registered bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM import_files WHERE source_name='generated-invalid-fixture.csv')").Scan(&registered); err != nil {
		t.Fatal(err)
	}
	if registered {
		t.Fatal("failed import left partial audit history")
	}
}
