package contextapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/database"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
	"github.com/rohankamewar2/global-context-auralink-iil/migrations"
)

func TestPostgresSessionBinding(t *testing.T) {
	if os.Getenv("AURALINK_RUN_DB_TESTS") != "1" {
		t.Skip("opt-in isolated PostgreSQL session test")
	}
	_ = godotenv.Load("../../.env")
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
	schema := "auralink_api_test_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("could not create test schema")
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := admin.Exec(clean, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("could not remove test-created schema")
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
	// Change only this isolated synthetic fixture into a structured requirement.
	if _, err := pool.Exec(ctx, `UPDATE raw_events SET event_kind='buyer.requirement_snapshot',payload='{"PRODUCT_NAME":"Generated steel fixture"}'`); err != nil {
		t.Fatal("could not prepare synthetic requirement")
	}
	scope := memory.Scope{Tenant: "local_demo", Namespace: "synthetic_demo", GLID: "900000001", Role: "buyer"}
	snapshot, err := memory.Generate(ctx, pool, scope, memory.Policy{AsOfDate: "2026-10-09", ActivityDays: 90, ConversationDays: 30, MaxBytes: 6000})
	if err != nil {
		t.Fatal(err)
	}
	_, _, c := fixtureAPI(t)
	c.Subjects = []memory.Scope{scope}
	mux := http.NewServeMux()
	if err := Register(mux, c, PGStore{pool}); err != nil {
		t.Fatal(err)
	}
	voice := create(t, mux, c, `{"glid":"900000001","role":"buyer","channel":"voice","active_thread_ref":"demo:req:steel-001"}`)
	chat := create(t, mux, c, `{"channel":"chat","resumed_from_id":"`+voice.Session.ID+`"}`)
	if chat.Session.Scope != scope || chat.Session.Thread != voice.Session.Thread || chat.Session.ResumedFrom != voice.Session.ID {
		t.Fatal("persisted cross-channel binding incorrect")
	}
	if voice.Token == chat.Token {
		t.Fatal("distinct durable sessions reused a capability")
	}
	wrong := request(mux, "POST", "/v1/sarvam/on-start", `{"session_id":"`+chat.Session.ID+`"}`, c.ToolToken, voice.Token)
	if wrong.Code != 404 {
		t.Fatal("database-backed session accepted another session's capability")
	}
	stored, err := (PGStore{pool}).Session(ctx, voice.Session.ID)
	if err != nil || stored.CapabilityHash != digest(voice.Token) || stored.CapabilityHash == voice.Token {
		t.Fatal("session capability not stored as digest")
	}
	w := request(mux, "POST", "/v1/sarvam/on-start", `{"session_id":"`+chat.Session.ID+`"}`, c.ToolToken, chat.Token)
	var e Envelope
	if json.Unmarshal(w.Body.Bytes(), &e) != nil || w.Code != 200 || e.Variables.MemoryMD != snapshot.MD || e.Hash != snapshot.Hash {
		t.Fatal("durable session failed canonical context read")
	}
	// The database itself also rejects cross-role resume and thread substitution.
	var userID, scopeID int64
	if err := pool.QueryRow(ctx, `SELECT user_id,id FROM subject_scopes WHERE role='buyer'`).Scan(&userID, &scopeID); err != nil {
		t.Fatal(err)
	}
	var otherID int64
	if err := pool.QueryRow(ctx, `INSERT INTO subject_scopes(user_id,role) VALUES ($1,'seller') RETURNING id`, userID).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_sessions SET scope_id=$2 WHERE id=$1`, chat.Session.ID, otherID); err == nil {
		t.Fatal("database accepted cross-role session substitution")
	}
	if _, err := pool.Exec(ctx, `UPDATE publication_state SET accepted_revision=accepted_revision+1 WHERE scope_id=$1`, scopeID); err != nil {
		t.Fatal(err)
	}
	w = request(mux, "POST", "/v1/sarvam/on-start", `{"session_id":"`+chat.Session.ID+`"}`, c.ToolToken, chat.Token)
	if w.Code != 503 {
		t.Fatal("agent did not block pending revision")
	}
	if _, err := pool.Exec(ctx, `UPDATE context_sessions SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, chat.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := (PGStore{pool}).Session(ctx, chat.Session.ID); err != ErrNotFound {
		t.Fatal("database did not enforce session expiry")
	}
}

// Optional read-only verification of the real imported cohort through the HTTP
// handlers. No customer calls, provider requests or new dataset events are made.
func TestImportedCohortContextAPI(t *testing.T) {
	if os.Getenv("AURALINK_VERIFY_IMPORTED_API") != "1" {
		t.Skip("opt-in real local cohort API read")
	}
	_ = godotenv.Load("../../.env")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	subjects, err := ReadSubjects("../../artifacts/exports/historical_dataset/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _, c := fixtureAPI(t)
	c.Subjects = subjects
	mux := http.NewServeMux()
	if err := Register(mux, c, PGStore{pool}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range subjects {
		w := request(mux, "GET", "/v1/users/"+scope.GLID+"/memory?role="+scope.Role, "", c.DemoToken, "")
		var e Envelope
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &e) != nil {
			t.Fatal("cohort API read failed")
		}
		s, n, err := memory.ReadPublished(ctx, pool, scope)
		if err != nil || e.Hash != s.Hash || e.Variables.MemoryMD != s.MD || e.Variables.MemoryVersion != s.Version || e.AcceptedRevision != n || e.Processing.Pending {
			t.Fatal("cohort API differs from canonical publication")
		}
		if len(e.Variables.MemoryMD) > 6000 || !e.Historical || e.Synthetic {
			t.Fatal("cohort API mislabeled dataset or exceeded memory size")
		}
	}
	t.Logf("verified %d real subject roles through private API handlers", len(subjects))
}
