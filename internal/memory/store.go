package memory

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func ReferenceDate(ctx context.Context, pool *pgxpool.Pool, tenant, namespace string) (string, error) {
	var value *string
	err := pool.QueryRow(ctx, `SELECT max(CASE
 WHEN source_time_raw ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}' THEN left(source_time_raw,10)
 WHEN source_time_raw ~ '^[0-9]{14}$' THEN left(source_time_raw,4)||'-'||substr(source_time_raw,5,2)||'-'||substr(source_time_raw,7,2)
	 WHEN time_quality IN ('verified','synthetic') AND occurred_at IS NOT NULL AND source_time_raw IS NULL THEN to_char(occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD')
 END) FROM raw_events WHERE tenant_id=$1 AND namespace=$2 AND event_kind<>'profile.snapshot'`, tenant, namespace).Scan(&value)
	if err != nil {
		return "", errors.New("could not determine historical reference date")
	}
	if value == nil || SourceDay(*value) == "" {
		return "", errors.New("no supported activity dates; supply an explicit -as-of date")
	}
	return *value, nil
}

// SelectSamples ranks real profile subjects using textual history, requirement
// evidence and channel diversity. Delivery receipts do not count as text.
func SelectSamples(ctx context.Context, pool *pgxpool.Pool, tenant, namespace string, perRole int, policy Policy) ([]Candidate, error) {
	if perRole < 1 || perRole > 10 {
		return nil, errors.New("sample count must be between 1 and 10 per role")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	ref, _ := time.Parse("2006-01-02", policy.AsOfDate)
	start := ref.AddDate(0, 0, -policy.ConversationDays+1).Format("2006-01-02")
	var result []Candidate
	for _, role := range []string{"buyer", "seller"} {
		rows, err := pool.Query(ctx, `WITH base AS (
 SELECT r.*,p.scope_id,p.thread_id,
 CASE WHEN r.source_time_raw ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}' THEN left(r.source_time_raw,10)
 WHEN r.source_time_raw ~ '^[0-9]{14}$' THEN left(r.source_time_raw,4)||'-'||substr(r.source_time_raw,5,2)||'-'||substr(r.source_time_raw,7,2)
 END source_day,
 CASE WHEN r.source IN ('buyer.LAST_WA9696_CHAT','buyer.LAST_WA8181_CHAT') THEN r.payload->>'MESSAGE_SENDER'='USER'
 WHEN r.source='seller.gc_whatsapp_chatbot_conversations.csv' THEN
 (jsonb_typeof(r.payload->'user_message')='string' AND length(r.payload->>'user_message')>3) OR length(r.payload->'user_message'->>'text_query')>3
 WHEN r.source='seller.gc_bot_call_turns.csv' THEN r.payload->>'speaker'='seller'
 ELSE r.event_kind IN ('enquiry.message','conversation.completed') END has_text
 FROM raw_events r JOIN event_projections p ON p.event_id=r.id
 WHERE r.tenant_id=$1 AND r.namespace=$2
), counts AS (
 SELECT u.glid,s.role,array_agg(DISTINCT r.channel ORDER BY r.channel) FILTER (WHERE r.channel<>'import') channels,
 count(*) event_count,
 count(*) FILTER (WHERE r.has_text AND (r.source_day BETWEEN $5 AND $6 OR r.source_day IS NULL)) text_count,
 count(DISTINCT r.thread_id) FILTER (WHERE r.event_kind IN ('buyer.requirement_snapshot','enquiry.created')) requirements,
 count(*) FILTER (WHERE r.channel='voice' AND r.event_kind='conversation.turn' AND r.has_text AND r.source_day BETWEEN $5 AND $6) voice_turns,
 count(*) FILTER (WHERE r.channel='whatsapp' AND r.has_text AND (r.source_day BETWEEN $5 AND $6 OR r.source_day IS NULL)) wa_turns
 FROM users u JOIN subject_scopes s ON s.user_id=u.id JOIN base r ON r.scope_id=s.id
 WHERE u.tenant_id=$1 AND u.namespace=$2 AND s.role=$3 AND u.profile ? $3 AND NOT u.is_synthetic
 GROUP BY u.glid,s.role
)
SELECT glid,role,channels,event_count,text_count,requirements FROM counts
WHERE text_count>0 AND requirements>0 AND wa_turns>0
ORDER BY (voice_turns>0) DESC,cardinality(channels) DESC,text_count DESC,requirements DESC,glid LIMIT $4`, tenant, namespace, role, perRole, start, policy.AsOfDate)
		if err != nil {
			return nil, errors.New("could not shortlist dataset subjects")
		}
		start := len(result)
		for rows.Next() {
			c := Candidate{Scope: Scope{Tenant: tenant, Namespace: namespace}}
			if err := rows.Scan(&c.Scope.GLID, &c.Scope.Role, &c.Channels, &c.Events, &c.TextEvents, &c.Requirements); err != nil {
				rows.Close()
				return nil, errors.New("could not read subject shortlist")
			}
			result = append(result, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, errors.New("could not complete subject shortlist")
		}
		if len(result)-start < perRole {
			return nil, errors.New("not enough profile subjects with requirement and WhatsApp history; select explicit GLIDs")
		}
	}
	return result, nil
}

// Generate locks one publication state while reading its bounded revision and
// publishing the exact Markdown and companion context in the same transaction.
// The CLI renderer makes no network/model calls while holding this lock.
func Generate(ctx context.Context, pool *pgxpool.Pool, scope Scope, policy Policy) (Snapshot, error) {
	var snapshot Snapshot
	if err := validateScope(scope); err != nil {
		return snapshot, err
	}
	if err := policy.Validate(); err != nil {
		return snapshot, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return snapshot, errors.New("could not begin memory publication")
	}
	defer tx.Rollback(context.Background())
	var scopeID int64
	var profile, provenance []byte
	var synthetic bool
	err = tx.QueryRow(ctx, `SELECT s.id,u.profile,u.provenance,u.is_synthetic FROM users u JOIN subject_scopes s ON s.user_id=u.id
WHERE u.tenant_id=$1 AND u.namespace=$2 AND u.glid=$3 AND s.role=$4`, scope.Tenant, scope.Namespace, scope.GLID, scope.Role).Scan(&scopeID, &profile, &provenance, &synthetic)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, errors.New("no imported user scope matches this GLID and role")
	}
	if err != nil {
		return snapshot, errors.New("could not load memory subject")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO publication_state (scope_id) VALUES ($1) ON CONFLICT DO NOTHING`, scopeID); err != nil {
		return snapshot, errors.New("could not initialise publication tracking")
	}
	var accepted, published int64
	var current *int64
	err = tx.QueryRow(ctx, `SELECT accepted_revision,published_revision,current_memory_version FROM publication_state WHERE scope_id=$1 FOR UPDATE`, scopeID).Scan(&accepted, &published, &current)
	if err != nil {
		return snapshot, errors.New("could not lock memory revision")
	}
	if current != nil && accepted == published {
		previous, err := loadSnapshot(ctx, tx, scope, scopeID, *current)
		if err != nil {
			return snapshot, err
		}
		if previous.Context.PolicyHash == policyHash(policy) {
			previous.Reused = true
			return previous, nil
		}
	}
	// Reload after taking the revision lock: an importer may have committed a
	// newer profile while this generator was waiting for that lock.
	err = tx.QueryRow(ctx, `SELECT u.profile,u.provenance FROM users u JOIN subject_scopes s ON s.user_id=u.id WHERE s.id=$1`, scopeID).Scan(&profile, &provenance)
	if err != nil {
		return snapshot, errors.New("could not read the profile at the locked revision")
	}
	input := Input{Scope: scope, Synthetic: synthetic, Revision: accepted}
	if err := decode(profile, &input.Profile); err != nil {
		return snapshot, errors.New("could not decode source profile")
	}
	if err := decode(provenance, &input.Provenance); err != nil {
		return snapshot, errors.New("could not decode profile provenance")
	}
	rows, err := tx.Query(ctx, `SELECT r.id,p.scope_revision,r.source,r.source_entity_id,r.event_kind,r.channel,COALESCE(t.thread_ref,''),COALESCE(r.source_time_raw,''),r.occurred_at,r.time_quality,r.payload
FROM event_projections p JOIN raw_events r ON r.id=p.event_id LEFT JOIN conversation_threads t ON t.id=p.thread_id
WHERE p.scope_id=$1 AND p.scope_revision<=$2 ORDER BY p.scope_revision LIMIT 20001`, scopeID, accepted)
	if err != nil {
		return snapshot, errors.New("could not load scoped evidence")
	}
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.Revision, &e.Source, &e.EntityID, &e.Kind, &e.Channel, &e.ThreadRef, &e.RawTime, &e.OccurredAt, &e.TimeQuality, &payload); err != nil {
			rows.Close()
			return snapshot, errors.New("could not read scoped evidence")
		}
		if err := decode(payload, &e.Payload); err != nil {
			rows.Close()
			return snapshot, errors.New("could not decode event evidence")
		}
		input.Events = append(input.Events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return snapshot, errors.New("could not complete evidence read")
	}
	if len(input.Events) > 20000 {
		return snapshot, errors.New("scope history exceeds the baseline compiler limit; use a bounded enrichment pipeline")
	}
	if int64(len(input.Events)) != accepted {
		return snapshot, errors.New("scope evidence does not match its accepted revision count")
	}
	version := int64(1)
	if current != nil {
		version = *current + 1
	}
	compiled, err := Build(input, policy, version)
	if err != nil {
		return snapshot, err
	}
	b, err := marshal(compiled)
	if err != nil {
		return snapshot, errors.New("could not encode companion context")
	}
	snapshot = Snapshot{Scope: scope, Version: version, IncludedRevision: accepted, MD: compiled.MemoryMD, Hash: hash([]byte(compiled.MemoryMD)), Context: compiled}
	err = tx.QueryRow(ctx, `INSERT INTO memory_snapshots (scope_id,memory_version,included_revision,memory_md,content_sha256,derived_context)
VALUES ($1,$2,$3,$4,$5,$6) RETURNING published_at`, scopeID, version, accepted, snapshot.MD, snapshot.Hash, b).Scan(&snapshot.PublishedAt)
	if err != nil {
		return Snapshot{}, errors.New("could not store canonical memory")
	}
	_, err = tx.Exec(ctx, `UPDATE publication_state SET published_revision=$2,current_memory_version=$3,updated_at=clock_timestamp() WHERE scope_id=$1`, scopeID, accepted, version)
	if err != nil {
		return Snapshot{}, errors.New("could not advance published memory revision")
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, errors.New("could not commit canonical memory")
	}
	return snapshot, nil
}

func loadSnapshot(ctx context.Context, tx pgx.Tx, scope Scope, scopeID, version int64) (Snapshot, error) {
	s := Snapshot{Scope: scope, Version: version}
	var contextJSON []byte
	err := tx.QueryRow(ctx, `SELECT included_revision,memory_md,content_sha256,derived_context,published_at FROM memory_snapshots WHERE scope_id=$1 AND memory_version=$2`, scopeID, version).Scan(&s.IncludedRevision, &s.MD, &s.Hash, &contextJSON, &s.PublishedAt)
	if err != nil {
		return s, errors.New("could not read canonical snapshot")
	}
	if err := decode(contextJSON, &s.Context); err != nil {
		return s, errors.New("could not decode canonical context")
	}
	if s.Hash != hash([]byte(s.MD)) || s.Context.MemoryMD != s.MD || s.Context.MemoryVersion != s.Version {
		return s, errors.New("canonical memory integrity check failed")
	}
	return s, nil
}

// ReadCurrent provides the canonical bytes for future agent and executive APIs.
// Caller identity/allowlist validation belongs to the API, not this SQL helper.
func ReadCurrent(ctx context.Context, pool *pgxpool.Pool, scope Scope) (Snapshot, error) {
	s, _, err := ReadPublished(ctx, pool, scope)
	return s, err
}

// ReadPublished reads canonical memory and its accepted watermark in one
// consistent transaction, so API callers can report pending activity honestly.
func ReadPublished(ctx context.Context, pool *pgxpool.Pool, scope Scope) (Snapshot, int64, error) {
	if err := validateScope(scope); err != nil {
		return Snapshot{}, 0, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, 0, errors.New("could not start canonical memory read")
	}
	defer tx.Rollback(context.Background())
	var scopeID, version, accepted int64
	err = tx.QueryRow(ctx, `SELECT s.id,p.current_memory_version,p.accepted_revision FROM users u JOIN subject_scopes s ON s.user_id=u.id JOIN publication_state p ON p.scope_id=s.id
WHERE u.tenant_id=$1 AND u.namespace=$2 AND u.glid=$3 AND s.role=$4 AND p.current_memory_version IS NOT NULL`, scope.Tenant, scope.Namespace, scope.GLID, scope.Role).Scan(&scopeID, &version, &accepted)
	if err != nil {
		return Snapshot{}, 0, errors.New("no published memory for this subject role")
	}
	s, err := loadSnapshot(ctx, tx, scope, scopeID, version)
	if err != nil {
		return s, 0, err
	}
	return s, accepted, tx.Commit(ctx)
}
