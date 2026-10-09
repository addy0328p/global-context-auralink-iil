package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DemoReport is limited to synthetic data and contains no customer records.
type DemoReport struct {
	GLID             string `json:"glid"`
	Role             string `json:"role"`
	Namespace        string `json:"namespace"`
	IsSynthetic      bool   `json:"is_synthetic"`
	AcceptedRevision int64  `json:"accepted_revision"`
	EventCount       int64  `json:"event_count"`
	EventInserted    bool   `json:"event_inserted"`
}

// SeedDemo creates one fixture without overwriting existing records. The fixed
// identity and source reference make repeats stable, while the row lock prevents
// concurrent seed runs from allocating duplicate revisions.
func SeedDemo(ctx context.Context, pool *pgxpool.Pool) (DemoReport, error) {
	var report DemoReport
	tx, err := pool.Begin(ctx)
	if err != nil {
		return report, errors.New("could not begin demo transaction")
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(ctx, `INSERT INTO users (tenant_id, namespace, glid, is_synthetic, profile, provenance)
		VALUES ('local_demo', 'synthetic_demo', '900000001', true,
		'{"display_name":"Synthetic Buyer 1","fixture_id":"buyer-steel-001"}',
		'{"source":"local_fixture","synthetic":true}')
		ON CONFLICT (tenant_id, namespace, glid) DO NOTHING`)
	if err != nil {
		return report, errors.New("could not insert synthetic profile")
	}
	var userID int64
	var valid bool
	err = tx.QueryRow(ctx, `SELECT id, is_synthetic AND profile->>'fixture_id' = 'buyer-steel-001'
		FROM users WHERE tenant_id = 'local_demo' AND namespace = 'synthetic_demo' AND glid = '900000001'`).Scan(&userID, &valid)
	if err != nil || !valid {
		return report, errors.New("demo identity conflicts with an existing record; nothing overwritten")
	}
	_, err = tx.Exec(ctx, `INSERT INTO subject_scopes(user_id, role) VALUES ($1, 'buyer') ON CONFLICT (user_id, role) DO NOTHING`, userID)
	if err != nil {
		return report, errors.New("could not create buyer scope")
	}
	var scopeID int64
	err = tx.QueryRow(ctx, "SELECT id FROM subject_scopes WHERE user_id = $1 AND role = 'buyer'", userID).Scan(&scopeID)
	if err != nil {
		return report, errors.New("could not retrieve buyer scope")
	}
	_, err = tx.Exec(ctx, "INSERT INTO publication_state(scope_id) VALUES ($1) ON CONFLICT DO NOTHING", scopeID)
	if err != nil {
		return report, errors.New("could not initialize publication state")
	}
	var accepted int64
	if err := tx.QueryRow(ctx, "SELECT accepted_revision FROM publication_state WHERE scope_id = $1 FOR UPDATE", scopeID).Scan(&accepted); err != nil {
		return report, errors.New("could not lock demo scope")
	}
	_, err = tx.Exec(ctx, `INSERT INTO conversation_threads(scope_id, thread_ref)
		VALUES ($1, 'demo:req:steel-001') ON CONFLICT (scope_id, thread_ref) DO NOTHING`, scopeID)
	if err != nil {
		return report, errors.New("could not create demo thread")
	}
	var threadID int64
	err = tx.QueryRow(ctx, "SELECT id FROM conversation_threads WHERE scope_id = $1 AND thread_ref = 'demo:req:steel-001'", scopeID).Scan(&threadID)
	if err != nil {
		return report, errors.New("could not retrieve demo thread")
	}

	const payload = `{"text":"I need 200 kilograms of steel rods delivered to Noida.","synthetic":true}`
	// Include immutable metadata in the event identity checksum, not just text.
	const eventContent = "demo.seed|buyer-steel-001:turn-001|fixture-v1|conversation.user_turn|chat|2026-10-09T06:30:00Z|synthetic|" + payload
	hash := sha256.Sum256([]byte(eventContent))
	checksum := hex.EncodeToString(hash[:])
	var eventID int64
	err = tx.QueryRow(ctx, `INSERT INTO raw_events (
		tenant_id, namespace, source, source_entity_id, source_revision, event_kind,
		channel, payload, content_sha256, occurred_at, time_quality, is_synthetic)
		VALUES ('local_demo', 'synthetic_demo', 'demo.seed', 'buyer-steel-001:turn-001',
		'fixture-v1', 'conversation.user_turn', 'chat', $1::jsonb, $2, $3, 'synthetic', true)
		ON CONFLICT (tenant_id, namespace, source, source_entity_id, source_revision)
		DO NOTHING RETURNING id`, payload, checksum, time.Date(2026, 10, 9, 6, 30, 0, 0, time.UTC)).Scan(&eventID)
	if err == nil {
		report.EventInserted = true
	} else if errors.Is(err, pgx.ErrNoRows) {
		var stored string
		err = tx.QueryRow(ctx, `SELECT id, content_sha256 FROM raw_events
			WHERE tenant_id = 'local_demo' AND namespace = 'synthetic_demo' AND source = 'demo.seed'
			AND source_entity_id = 'buyer-steel-001:turn-001' AND source_revision = 'fixture-v1'`).Scan(&eventID, &stored)
		if err != nil || stored != checksum {
			return report, errors.New("demo event conflicts with existing content; nothing overwritten")
		}
	} else {
		return report, errors.New("could not insert synthetic event")
	}
	var revision int64
	err = tx.QueryRow(ctx, "SELECT scope_revision FROM event_projections WHERE event_id = $1 AND scope_id = $2", eventID, scopeID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		revision = accepted + 1
		_, err = tx.Exec(ctx, `INSERT INTO event_projections(event_id, scope_id, user_id, tenant_id, namespace, scope_revision, thread_id)
			VALUES ($1, $2, $3, 'local_demo', 'synthetic_demo', $4, $5)`, eventID, scopeID, userID, revision, threadID)
		if err != nil {
			return report, errors.New("could not attach event to its buyer/thread")
		}
		_, err = tx.Exec(ctx, "UPDATE publication_state SET accepted_revision = $2, updated_at = clock_timestamp() WHERE scope_id = $1", scopeID, revision)
		if err != nil {
			return report, errors.New("could not advance scope revision")
		}
	} else if err != nil {
		return report, errors.New("could not check duplicate event projection")
	}
	if err := tx.Commit(ctx); err != nil {
		return report, errors.New("could not commit synthetic fixture")
	}

	// Retrieve after commit to check that the data is really stored.
	err = pool.QueryRow(ctx, `SELECT u.glid, s.role, u.namespace, u.is_synthetic, p.accepted_revision,
		(SELECT count(*) FROM event_projections e WHERE e.scope_id = s.id)
		FROM users u JOIN subject_scopes s ON s.user_id = u.id JOIN publication_state p ON p.scope_id = s.id
		WHERE s.id = $1`, scopeID).Scan(&report.GLID, &report.Role, &report.Namespace, &report.IsSynthetic, &report.AcceptedRevision, &report.EventCount)
	if err != nil {
		return report, errors.New("fixture saved, but verification read failed")
	}
	return report, nil
}
