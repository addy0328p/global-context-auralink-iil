package ingestion

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Tenant = "local_demo"
const Namespace = "historical_dataset"

// Store commits one source file at a time. A failed file leaves no partial rows;
// a repeat run skips committed files with the same hash and mapping version.
func Store(ctx context.Context, pool *pgxpool.Pool, dataset *Dataset, zone string) (Report, error) {
	report := Report{MappingVersion: MappingVersion, SourceTimezone: zone}
	for _, file := range dataset.Files {
		result, err := storeFile(ctx, pool, file, zone)
		if err != nil {
			return report, fmt.Errorf("%s: %w", file.Report.Name, err)
		}
		report.Files = append(report.Files, result)
	}
	stats, err := Verify(ctx, pool)
	if err != nil {
		return report, err
	}
	report.Database = &stats
	if !stats.Consistent {
		return report, errors.New("dataset revision verification failed")
	}
	return report, nil
}

// Verify returns aggregate counts only; it never exposes customer identifiers.
func Verify(ctx context.Context, pool *pgxpool.Pool) (DatabaseStats, error) {
	var stats DatabaseStats
	err := pool.QueryRow(ctx, `WITH own_users AS (SELECT id FROM users WHERE tenant_id=$1 AND namespace=$2),
 own_scopes AS (SELECT s.id,s.role FROM subject_scopes s JOIN own_users u ON s.user_id=u.id)
SELECT (SELECT count(*) FROM own_users),
 (SELECT count(*) FROM own_scopes WHERE role='buyer'),
 (SELECT count(*) FROM own_scopes WHERE role='seller'),
 (SELECT count(*) FROM raw_events WHERE tenant_id=$1 AND namespace=$2),
 (SELECT count(*) FROM event_projections WHERE tenant_id=$1 AND namespace=$2),
 (SELECT COALESCE(sum(p.accepted_revision),0) FROM publication_state p JOIN own_scopes s ON s.id=p.scope_id),
 (SELECT count(*) FROM raw_events r WHERE tenant_id=$1 AND namespace=$2 AND NOT EXISTS (SELECT 1 FROM event_projections p WHERE p.event_id=r.id))`, Tenant, Namespace).Scan(&stats.Users, &stats.BuyerScopes, &stats.SellerScopes, &stats.Events, &stats.Projections, &stats.AcceptedRevisions, &stats.EventsWithoutProjection)
	if err != nil {
		return stats, errors.New("could not verify dataset counts")
	}
	stats.Consistent = stats.Projections == stats.AcceptedRevisions && stats.EventsWithoutProjection == 0
	return stats, nil
}

func storeFile(ctx context.Context, pool *pgxpool.Pool, file *File, zone string) (FileReport, error) {
	report := file.Report
	version := report.MappingVersion
	if version == "" {
		version = MappingVersion
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return report, errors.New("could not begin import transaction")
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(724611924)"); err != nil {
		return report, errors.New("could not lock import")
	}
	var incompatible bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM import_files WHERE tenant_id=$1 AND namespace=$2 AND timezone_assumption<>$3)`, Tenant, Namespace, zone).Scan(&incompatible)
	if err != nil {
		return report, errors.New("could not read import history; run migrations first")
	}
	if incompatible {
		return report, errors.New("timezone assumption differs from the existing historical import")
	}
	var fileID int64
	err = tx.QueryRow(ctx, `SELECT id FROM import_files WHERE tenant_id=$1 AND namespace=$2 AND source_name=$3 AND file_sha256=$4 AND mapping_version=$5 AND timezone_assumption=$6`, Tenant, Namespace, report.Name, report.SHA256, version, zone).Scan(&fileID)
	if err == nil {
		report.Skipped = true
		return report, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return report, errors.New("could not check source file history")
	}
	err = tx.QueryRow(ctx, `INSERT INTO import_files (tenant_id,namespace,source_name,file_sha256,mapping_version,timezone_assumption,report) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, Tenant, Namespace, report.Name, report.SHA256, version, zone, encode(report)).Scan(&fileID)
	if err != nil {
		return report, errors.New("could not register source file")
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"import_rows"}, []string{"import_file_id", "record_number", "row_sha256", "safe_payload", "quarantine_reason"}, copySource(len(file.Rows), func(i int) []any {
		r := file.Rows[i]
		return []any{fileID, r.Number, r.Hash, r.Payload, nullable(r.Reason)}
	}))
	if err != nil {
		return report, errors.New("could not stage audited source rows")
	}
	_, err = tx.Exec(ctx, `
CREATE TEMP TABLE ingest_profiles (seq integer, glid text, role text, payload jsonb, provenance jsonb) ON COMMIT DROP;
CREATE TEMP TABLE ingest_events (seq integer, glid text, role text, source text, entity text, revision text, kind text, channel text, thread text, payload jsonb, hash text, raw_time text, occurred_at timestamptz, quality text) ON COMMIT DROP;`)
	if err != nil {
		return report, errors.New("could not prepare import batches")
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"ingest_profiles"}, []string{"seq", "glid", "role", "payload", "provenance"}, copySource(len(file.Profiles), func(i int) []any {
		p := file.Profiles[i]
		return []any{i + 1, p.GLID, p.Role, p.Payload, p.Provenance}
	}))
	if err != nil {
		return report, errors.New("could not batch profiles")
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"ingest_events"}, []string{"seq", "glid", "role", "source", "entity", "revision", "kind", "channel", "thread", "payload", "hash", "raw_time", "occurred_at", "quality"}, copySource(len(file.Events), func(i int) []any {
		e := file.Events[i]
		return []any{i + 1, e.GLID, e.Role, e.Source, e.EntityID, e.Revision, e.Kind, e.Channel, nullable(e.Thread), e.Payload, e.Hash, nullable(e.RawTime), e.OccurredAt, e.TimeQuality}
	}))
	if err != nil {
		return report, errors.New("could not batch normalized events")
	}
	// Profile snapshots remain role-specific, including users who buy and sell.
	command, err := tx.Exec(ctx, `INSERT INTO users (tenant_id,namespace,glid)
SELECT $1,$2,glid FROM (SELECT glid FROM ingest_profiles UNION SELECT glid FROM ingest_events) p
ON CONFLICT (tenant_id,namespace,glid) DO NOTHING`, Tenant, Namespace)
	if err != nil {
		return report, errors.New("could not create dataset identities")
	}
	report.NewProfiles = int(command.RowsAffected())
	_, err = tx.Exec(ctx, `WITH latest AS (
 SELECT DISTINCT ON (glid,role) glid,role,payload,provenance FROM ingest_profiles
 ORDER BY glid,role,COALESCE(provenance->>'source_sort_key','') DESC,seq DESC
), combined AS (
 SELECT p.glid,jsonb_object_agg(p.role,p.payload) profile,jsonb_object_agg(p.role,p.provenance) provenance
 FROM latest p JOIN users u ON u.glid=p.glid AND u.tenant_id=$1 AND u.namespace=$2
 WHERE COALESCE(p.provenance->>'source_sort_key','') >= COALESCE(u.provenance->p.role->>'source_sort_key','')
 GROUP BY p.glid
)
UPDATE users u SET profile=u.profile||p.profile,provenance=u.provenance||p.provenance,updated_at=clock_timestamp()
FROM combined p WHERE u.tenant_id=$1 AND u.namespace=$2 AND u.glid=p.glid`, Tenant, Namespace)
	if err != nil {
		return report, errors.New("could not store source profiles")
	}
	_, err = tx.Exec(ctx, `INSERT INTO subject_scopes (user_id,role)
SELECT DISTINCT u.id,p.role FROM (SELECT glid,role FROM ingest_profiles UNION SELECT glid,role FROM ingest_events) p
JOIN users u ON u.glid=p.glid AND u.tenant_id=$1 AND u.namespace=$2 ON CONFLICT DO NOTHING`, Tenant, Namespace)
	if err != nil {
		return report, errors.New("could not separate buyer and seller scopes")
	}
	_, err = tx.Exec(ctx, `CREATE TEMP TABLE ingest_scoped ON COMMIT DROP AS
SELECT e.*,u.id user_id,s.id scope_id FROM ingest_events e JOIN users u ON u.glid=e.glid
JOIN subject_scopes s ON s.user_id=u.id AND s.role=e.role WHERE u.tenant_id=$1 AND u.namespace=$2`, Tenant, Namespace)
	if err != nil {
		return report, errors.New("could not resolve event owners")
	}
	_, err = tx.Exec(ctx, `INSERT INTO conversation_threads (scope_id,thread_ref)
SELECT DISTINCT scope_id,thread FROM ingest_scoped WHERE thread IS NOT NULL ON CONFLICT DO NOTHING;
INSERT INTO publication_state (scope_id) SELECT DISTINCT scope_id FROM ingest_scoped ON CONFLICT DO NOTHING;`)
	if err != nil {
		return report, errors.New("could not create scoped threads and revision tracking")
	}
	// Lock in the same order before allocating revisions, including against a live writer.
	locks, err := tx.Query(ctx, `SELECT p.scope_id FROM publication_state p JOIN (SELECT DISTINCT scope_id FROM ingest_scoped) s USING (scope_id) ORDER BY p.scope_id FOR UPDATE OF p`)
	if err != nil {
		return report, errors.New("could not lock memory revisions")
	}
	for locks.Next() {
	}
	err = locks.Err()
	locks.Close()
	if err != nil {
		return report, errors.New("could not acquire memory revision locks")
	}
	command, err = tx.Exec(ctx, `INSERT INTO raw_events (tenant_id,namespace,source,source_entity_id,source_revision,event_kind,channel,payload,content_sha256,source_time_raw,occurred_at,time_quality)
SELECT DISTINCT ON (source,entity,revision) $1,$2,source,entity,revision,kind,channel,payload,hash,raw_time,occurred_at,quality
FROM ingest_scoped ORDER BY source,entity,revision,seq ON CONFLICT (tenant_id,namespace,source,source_entity_id,source_revision) DO NOTHING`, Tenant, Namespace)
	if err != nil {
		return report, errors.New("could not store source events")
	}
	report.NewEvents = int(command.RowsAffected())
	_, err = tx.Exec(ctx, `CREATE TEMP TABLE ingest_new_projections ON COMMIT DROP AS
WITH resolved AS (
 SELECT DISTINCT ON (r.id,e.scope_id) r.id event_id,e.scope_id,e.user_id,t.id thread_id,e.seq
 FROM ingest_scoped e JOIN raw_events r ON r.tenant_id=$1 AND r.namespace=$2 AND r.source=e.source AND r.source_entity_id=e.entity AND r.source_revision=e.revision
 LEFT JOIN conversation_threads t ON t.scope_id=e.scope_id AND t.thread_ref=e.thread
 WHERE NOT EXISTS (SELECT 1 FROM event_projections p WHERE p.event_id=r.id AND p.scope_id=e.scope_id)
 ORDER BY r.id,e.scope_id,e.seq
)
SELECT r.*,p.accepted_revision+row_number() OVER (PARTITION BY r.scope_id ORDER BY r.seq,r.event_id) new_revision
FROM resolved r JOIN publication_state p ON p.scope_id=r.scope_id`, Tenant, Namespace)
	if err != nil {
		return report, errors.New("could not allocate new event revisions")
	}
	_, err = tx.Exec(ctx, `INSERT INTO event_projections (event_id,scope_id,user_id,tenant_id,namespace,scope_revision,thread_id)
SELECT event_id,scope_id,user_id,$1,$2,new_revision,thread_id FROM ingest_new_projections`, Tenant, Namespace)
	if err != nil {
		return report, errors.New("could not attach events to the correct memory scope")
	}
	_, err = tx.Exec(ctx, `UPDATE publication_state p SET accepted_revision=n.latest,updated_at=clock_timestamp()
FROM (SELECT scope_id,max(new_revision) latest FROM ingest_new_projections GROUP BY scope_id) n WHERE p.scope_id=n.scope_id`)
	if err != nil {
		return report, errors.New("could not advance accepted event revisions")
	}
	if _, err = tx.Exec(ctx, `UPDATE import_files SET report=$2 WHERE id=$1`, fileID, encode(report)); err != nil {
		return report, errors.New("could not save import audit")
	}
	if err = tx.Commit(ctx); err != nil {
		return report, errors.New("could not commit source file import")
	}
	return report, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func copySource(count int, values func(int) []any) pgx.CopyFromSource {
	i := 0
	return pgx.CopyFromFunc(func() ([]any, error) {
		if i == count {
			return nil, nil
		}
		row := values(i)
		i++
		return row, nil
	})
}
