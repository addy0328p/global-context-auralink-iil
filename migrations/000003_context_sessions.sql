-- Sessions bind channel access to a server-approved subject and requirement.
-- Only a SHA-256 digest of the per-session tool capability is stored.
CREATE TABLE context_sessions (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'),
    principal_id text NOT NULL CHECK (length(btrim(principal_id)) > 0),
    scope_id bigint NOT NULL REFERENCES subject_scopes(id),
    channel text NOT NULL CHECK (channel IN ('voice','chat','whatsapp','app')),
    thread_id bigint,
    initial_memory_version bigint NOT NULL,
    capability_sha256 text NOT NULL CHECK (capability_sha256 ~ '^[0-9a-f]{64}$'),
    resumed_from_id text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    UNIQUE (id, scope_id, principal_id),
    FOREIGN KEY (thread_id,scope_id) REFERENCES conversation_threads(id,scope_id),
    FOREIGN KEY (scope_id,initial_memory_version) REFERENCES memory_snapshots(scope_id,memory_version),
    FOREIGN KEY (resumed_from_id,scope_id,principal_id) REFERENCES context_sessions(id,scope_id,principal_id),
    CHECK (expires_at > created_at)
);
CREATE INDEX context_sessions_expiry_idx ON context_sessions(expires_at);
