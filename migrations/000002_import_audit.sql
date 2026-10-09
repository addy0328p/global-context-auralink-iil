-- Import history and source references are separate from conversational memory.
CREATE TABLE import_files (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id text NOT NULL,
    namespace text NOT NULL,
    source_name text NOT NULL,
    file_sha256 text NOT NULL CHECK (file_sha256 ~ '^[0-9a-f]{64}$'),
    mapping_version text NOT NULL,
    timezone_assumption text NOT NULL DEFAULT '',
    report jsonb NOT NULL CHECK (jsonb_typeof(report) = 'object'),
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, namespace, source_name, file_sha256, mapping_version, timezone_assumption)
);

CREATE TABLE import_rows (
    import_file_id bigint NOT NULL REFERENCES import_files(id),
    record_number integer NOT NULL CHECK (record_number > 0),
    row_sha256 text NOT NULL CHECK (row_sha256 ~ '^[0-9a-f]{64}$'),
    safe_payload jsonb NOT NULL CHECK (jsonb_typeof(safe_payload) = 'object'),
    quarantine_reason text,
    PRIMARY KEY (import_file_id, record_number)
);

CREATE INDEX import_rows_quarantine_idx ON import_rows(import_file_id, quarantine_reason)
    WHERE quarantine_reason IS NOT NULL;
