package migrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/database"
)

// Database tests are opt-in. They use a new randomly named schema, not the
// project's tables. Cleanup removes only the schema created by this test.
func TestDatabaseFoundation(t *testing.T) {
	if os.Getenv("AURALINK_RUN_DB_TESTS") != "1" {
		t.Skip("set AURALINK_RUN_DB_TESTS=1 to run isolated PostgreSQL integration tests")
	}
	if err := godotenv.Load("../.env"); err != nil && !os.IsNotExist(err) {
		t.Fatal("could not load local test configuration")
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
		t.Fatal("could not create test schema name")
	}
	schema := "auralink_test_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("could not create isolated test schema")
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("could not clean up test-created schema")
		}
	}()
	config := admin.Config().Copy()
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("could not create test connection pool")
	}
	defer pool.Close()

	t.Run("migration_is_repeatable", func(t *testing.T) {
		first, err := Apply(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		names, err := fs.Glob(files, "*.sql")
		if err != nil {
			t.Fatal(err)
		}
		if len(first) != len(names) {
			t.Fatalf("expected %d initial migrations, got %d", len(names), len(first))
		}
		second, err := Apply(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		if len(second) != 0 {
			t.Fatal("repeat migration applied changes again")
		}
	})
	if t.Failed() {
		return
	}

	t.Run("concurrent_seed_does_not_duplicate_events", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan database.DemoReport, 8)
		failures := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				report, err := database.SeedDemo(ctx, pool)
				if err != nil {
					failures <- err
					return
				}
				results <- report
			}()
		}
		wg.Wait()
		close(failures)
		close(results)
		for err := range failures {
			t.Error(err)
		}
		insertions := 0
		for report := range results {
			if !report.IsSynthetic || report.Role != "buyer" || report.Namespace != "synthetic_demo" || report.EventCount != 1 || report.AcceptedRevision != 1 {
				t.Error("seed report contains incorrect identity or duplicate revisions")
			}
			if report.EventInserted {
				insertions++
			}
		}
		if insertions != 1 {
			t.Fatalf("expected exactly one insertion, got %d", insertions)
		}
	})

	t.Run("invalid_role_is_rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO subject_scopes(user_id, role) SELECT id, 'invalid' FROM users LIMIT 1`)
		expectSQLState(t, err, "23514")
	})
	t.Run("unknown_source_time_cannot_be_fabricated", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO raw_events(tenant_id, namespace, source, source_entity_id, source_revision,
			event_kind, channel, payload, content_sha256, occurred_at, time_quality, is_synthetic)
			VALUES ('local_demo', 'synthetic_demo', 'test', 'time-001', '1', 'call.metadata', 'import', '{}',
			repeat('a',64), now(), 'unresolved', true)`)
		expectSQLState(t, err, "23514")
	})
	t.Run("thread_cannot_cross_buyer_seller_roles", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal("could not begin constraint test")
		}
		defer tx.Rollback(context.Background())
		var userID, buyerID, threadID, eventID int64
		err = tx.QueryRow(ctx, `SELECT u.id, s.id, c.id, e.id FROM users u
			JOIN subject_scopes s ON s.user_id=u.id AND s.role='buyer'
			JOIN conversation_threads c ON c.scope_id=s.id
			JOIN event_projections p ON p.scope_id=s.id JOIN raw_events e ON e.id=p.event_id LIMIT 1`).Scan(&userID, &buyerID, &threadID, &eventID)
		if err != nil {
			t.Fatal("could not load test fixture")
		}
		var sellerID int64
		err = tx.QueryRow(ctx, "INSERT INTO subject_scopes(user_id,role) VALUES ($1,'seller') RETURNING id", userID).Scan(&sellerID)
		if err != nil {
			t.Fatal("could not create test seller role")
		}
		_, err = tx.Exec(ctx, `INSERT INTO event_projections(event_id,scope_id,user_id,tenant_id,namespace,scope_revision,thread_id)
			VALUES ($1,$2,$3,'local_demo','synthetic_demo',1,$4)`, eventID, sellerID, userID, threadID)
		expectSQLState(t, err, "23503")
	})
	t.Run("event_cannot_be_attributed_to_another_user", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal("could not begin identity test")
		}
		defer tx.Rollback(context.Background())
		var otherUser, eventID, scopeID int64
		err = tx.QueryRow(ctx, `INSERT INTO users(tenant_id,namespace,glid,is_synthetic)
			VALUES ('local_demo','synthetic_demo','900000002',true) RETURNING id`).Scan(&otherUser)
		if err != nil {
			t.Fatal("could not create alternate test user")
		}
		err = tx.QueryRow(ctx, "SELECT event_id, scope_id FROM event_projections LIMIT 1").Scan(&eventID, &scopeID)
		if err != nil {
			t.Fatal("could not retrieve test event")
		}
		err = tx.QueryRow(ctx, `INSERT INTO raw_events(tenant_id,namespace,source,source_entity_id,source_revision,
			event_kind,channel,payload,content_sha256,occurred_at,time_quality,is_synthetic)
			SELECT tenant_id,namespace,source,source_entity_id,'identity-test',event_kind,channel,payload,
			content_sha256,occurred_at,time_quality,is_synthetic FROM raw_events WHERE id=$1 RETURNING id`, eventID).Scan(&eventID)
		if err != nil {
			t.Fatal("could not prepare isolated identity test event")
		}
		_, err = tx.Exec(ctx, `INSERT INTO event_projections(event_id,scope_id,user_id,tenant_id,namespace,scope_revision)
			VALUES ($1,$2,$3,'local_demo','synthetic_demo',2)`, eventID, scopeID, otherUser)
		expectSQLState(t, err, "23503")
	})
	t.Run("published_revision_cannot_exceed_received_revision", func(t *testing.T) {
		_, err := pool.Exec(ctx, "UPDATE publication_state SET published_revision=accepted_revision+1")
		expectSQLState(t, err, "23514")
	})
	t.Run("changed_migration_is_rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx, "UPDATE schema_migrations SET checksum='changed'")
		if err != nil {
			t.Fatal("could not prepare checksum test")
		}
		if _, err := Apply(ctx, pool); err == nil {
			t.Fatal("changed migration checksum was accepted")
		}
	})
}

func expectSQLState(t *testing.T, err error, state string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != state {
		t.Fatalf("expected PostgreSQL rejection %s", state)
	}
}
