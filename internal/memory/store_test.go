package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/database"
	"github.com/rohankamewar2/global-context-auralink-iil/migrations"
)

func TestMemoryPublicationAndExports(t *testing.T) {
	if os.Getenv("AURALINK_RUN_DB_TESTS") != "1" {
		t.Skip("opt-in isolated PostgreSQL memory test")
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
	schema := "auralink_memory_test_" + hex.EncodeToString(random[:])
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
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := migrations.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SeedDemo(ctx, pool); err != nil {
		t.Fatal(err)
	}
	buyer := Scope{Tenant: "local_demo", Namespace: "synthetic_demo", GLID: "900000001", Role: "buyer"}
	seller := buyer
	seller.Role = "seller"
	var userID, scopeID int64
	if err := pool.QueryRow(ctx, `SELECT u.id,s.id FROM users u JOIN subject_scopes s ON s.user_id=u.id WHERE u.tenant_id=$1 AND u.namespace=$2 AND u.glid=$3 AND s.role='buyer'`, buyer.Tenant, buyer.Namespace, buyer.GLID).Scan(&userID, &scopeID); err != nil {
		t.Fatal(err)
	}
	profile, _ := marshal(map[string]any{"buyer": map[string]any{"company_name": "Generated buyer fixture", "city": "Noida"}, "seller": map[string]any{"top_category_1": "SELLER_ONLY_FIXTURE", "do_not_call_requested": "1"}})
	if _, err := pool.Exec(ctx, `UPDATE users SET profile=$2 WHERE id=$1`, userID, profile); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO subject_scopes (user_id,role) VALUES ($1,'seller')`, userID); err != nil {
		t.Fatal(err)
	}
	addFixtureEvent(t, ctx, pool, buyer, scopeID, userID, "future-profile", "profile.snapshot", "2050-01-01 10:00:00", "", map[string]any{"fixture": true})
	date, err := ReferenceDate(ctx, pool, buyer.Tenant, buyer.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	if date != "2026-10-09" {
		t.Fatal("profile observation time became the activity reference date")
	}
	policy := testPolicy()
	policy.AsOfDate = date
	var wg sync.WaitGroup
	results := make(chan Snapshot, 8)
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Generate(ctx, pool, buyer, policy)
			if err != nil {
				failures <- err
			} else {
				results <- s
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var first Snapshot
	var reused int
	for result := range results {
		if result.Version != 1 {
			t.Fatal("concurrent generation multiplied snapshot versions")
		}
		if first.Version == 0 {
			first = result
		} else if first.MD != result.MD || first.Hash != result.Hash {
			t.Fatal("same revision returned different canonical bytes")
		}
		if result.Reused {
			reused++
		}
	}
	if reused != 7 {
		t.Fatal("concurrent repeat generation did not reuse the canonical snapshot")
	}
	current, err := ReadCurrent(ctx, pool, buyer)
	if err != nil {
		t.Fatal(err)
	}
	if current.MD != first.MD || current.Context.MemoryMD != first.MD || current.Hash != first.Hash {
		t.Fatal("executive/agent read differs from canonical memory")
	}
	root := t.TempDir()
	forged := first
	forged.MD = "noncanonical text"
	forged.Hash = hash([]byte(forged.MD))
	if _, err := ExportSnapshot(ctx, pool, forged, root); err == nil {
		t.Fatal("a self-consistent forged snapshot was exported instead of canonical bytes")
	}
	export1, err := ExportSnapshot(ctx, pool, first, root)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := os.ReadFile(export1.Path)
	if err != nil {
		t.Fatal(err)
	}
	if hash(exported) != first.Hash {
		t.Fatal("exported bytes differ from PostgreSQL")
	}
	addFixtureEvent(t, ctx, pool, buyer, scopeID, userID, "new-enquiry", "buyer.requirement_snapshot", "2026-10-09 10:00:00", "enquiry:fixture", map[string]any{"PRODUCT_NAME": "BUYER_PRODUCT_FIXTURE"})
	second, err := Generate(ctx, pool, buyer, policy)
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 || second.IncludedRevision != 3 || !strings.Contains(second.MD, "BUYER\\_PRODUCT\\_FIXTURE") {
		t.Fatal("new event was not published in the next version")
	}
	if _, err := ExportSnapshot(ctx, pool, first, root); err == nil {
		t.Fatal("stale snapshot replaced a newer export")
	}
	export2, err := ExportSnapshot(ctx, pool, second, root)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := os.ReadFile(export2.Path)
	if err != nil {
		t.Fatal(err)
	}
	if hash(latest) != second.Hash {
		t.Fatal("latest export has incorrect bytes")
	}
	old, err := os.ReadFile(filepath.Join(root, buyer.Namespace, buyer.GLID, "versions", "v000001", "buyer.md"))
	if err != nil {
		t.Fatal(err)
	}
	if hash(old) != first.Hash {
		t.Fatal("immutable earlier version was overwritten")
	}
	otherRole, err := Generate(ctx, pool, seller, policy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(otherRole.MD, "BUYER\\_PRODUCT") || !otherRole.Context.ColdStart {
		t.Fatal("buyer event leaked into seller context")
	}
	// A late old event advances the receipt revision, but does not become current work.
	addFixtureEvent(t, ctx, pool, buyer, scopeID, userID, "late-enquiry", "buyer.requirement_snapshot", "2026-01-01 10:00:00", "enquiry:fixture", map[string]any{"PRODUCT_NAME": "LATE_OLD_PRODUCT_FIXTURE"})
	third, err := Generate(ctx, pool, buyer, policy)
	if err != nil {
		t.Fatal(err)
	}
	if third.Version != 3 || third.IncludedRevision != 4 || strings.Contains(third.MD, "LATE\\_OLD") {
		t.Fatal("late historical data became current requirement evidence")
	}
	wrong := buyer
	wrong.Namespace = "historical_dataset"
	if _, err := Generate(ctx, pool, wrong, policy); err == nil {
		t.Fatal("namespace boundary ignored")
	}
}

func addFixtureEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scope Scope, scopeID, userID int64, id, kind, raw, thread string, payload map[string]any) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT accepted_revision+1 FROM publication_state WHERE scope_id=$1 FOR UPDATE`, scopeID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	var threadID *int64
	if thread != "" {
		var value int64
		if err := tx.QueryRow(ctx, `INSERT INTO conversation_threads (scope_id,thread_ref) VALUES ($1,$2) ON CONFLICT (scope_id,thread_ref) DO UPDATE SET thread_ref=EXCLUDED.thread_ref RETURNING id`, scopeID, thread).Scan(&value); err != nil {
			t.Fatal(err)
		}
		threadID = &value
	}
	b, _ := marshal(payload)
	var eventID int64
	if err := tx.QueryRow(ctx, `INSERT INTO raw_events (tenant_id,namespace,source,source_entity_id,source_revision,event_kind,channel,payload,content_sha256,source_time_raw,time_quality,is_synthetic)
VALUES ($1,$2,'buyer.LAST_ENQUIRY',$3,$4,$5,'app',$6,$4,$7,'unresolved',true) RETURNING id`, scope.Tenant, scope.Namespace, id, hash(b), kind, b, raw).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO event_projections (event_id,scope_id,user_id,tenant_id,namespace,scope_revision,thread_id) VALUES ($1,$2,$3,$4,$5,$6,$7)`, eventID, scopeID, userID, scope.Tenant, scope.Namespace, revision, threadID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE publication_state SET accepted_revision=$2 WHERE scope_id=$1`, scopeID, revision); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
