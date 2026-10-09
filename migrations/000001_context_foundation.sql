-- Profiles and historical/synthetic/live data are separated at the identity level.
CREATE TABLE users (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) > 0),
    namespace text NOT NULL CHECK (length(btrim(namespace)) > 0),
    glid text NOT NULL CHECK (glid ~ '^[0-9]+$'),
    is_synthetic boolean NOT NULL DEFAULT false,
    profile jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(profile) = 'object'),
    provenance jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(provenance) = 'object'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, namespace, glid),
    UNIQUE (id, tenant_id, namespace),
    CHECK (namespace <> 'synthetic_demo' OR is_synthetic)
);

CREATE TABLE subject_scopes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users(id),
    role text NOT NULL CHECK (role IN ('buyer', 'seller')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (user_id, role)
);

CREATE TABLE conversation_threads (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope_id bigint NOT NULL REFERENCES subject_scopes(id),
    thread_ref text NOT NULL CHECK (length(btrim(thread_ref)) > 0),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'closed', 'cancelled')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (scope_id, thread_ref),
    UNIQUE (id, scope_id)
);

-- Keep source identity distinct from projections into buyer/seller memory.
CREATE TABLE raw_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id text NOT NULL CHECK (length(btrim(tenant_id)) > 0),
    namespace text NOT NULL CHECK (length(btrim(namespace)) > 0),
    source text NOT NULL CHECK (length(btrim(source)) > 0),
    source_entity_id text NOT NULL CHECK (length(btrim(source_entity_id)) > 0),
    source_revision text NOT NULL CHECK (length(btrim(source_revision)) > 0),
    event_kind text NOT NULL CHECK (length(btrim(event_kind)) > 0),
    channel text NOT NULL CHECK (channel IN ('voice', 'chat', 'whatsapp', 'app', 'import')),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    source_time_raw text,
    occurred_at timestamptz,
    time_quality text NOT NULL CHECK (time_quality IN ('verified', 'assumed', 'unresolved', 'synthetic')),
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    is_synthetic boolean NOT NULL DEFAULT false,
    UNIQUE (tenant_id, namespace, source, source_entity_id, source_revision),
    UNIQUE (id, tenant_id, namespace),
    CHECK (time_quality NOT IN ('verified', 'synthetic') OR occurred_at IS NOT NULL),
    CHECK (time_quality <> 'unresolved' OR occurred_at IS NULL),
    CHECK (namespace <> 'synthetic_demo' OR is_synthetic)
);

CREATE TABLE event_projections (
    event_id bigint NOT NULL,
    scope_id bigint NOT NULL REFERENCES subject_scopes(id),
    user_id bigint NOT NULL,
    tenant_id text NOT NULL,
    namespace text NOT NULL,
    scope_revision bigint NOT NULL CHECK (scope_revision > 0),
    thread_id bigint,
    PRIMARY KEY (event_id, scope_id),
    UNIQUE (scope_id, scope_revision),
    FOREIGN KEY (event_id, tenant_id, namespace) REFERENCES raw_events(id, tenant_id, namespace),
    FOREIGN KEY (user_id, tenant_id, namespace) REFERENCES users(id, tenant_id, namespace),
    FOREIGN KEY (thread_id, scope_id) REFERENCES conversation_threads(id, scope_id)
);

-- Ensure repeated scope/user keys cannot attribute an event to a different user.
ALTER TABLE subject_scopes ADD CONSTRAINT subject_scopes_id_user_unique UNIQUE (id, user_id);
ALTER TABLE event_projections ADD CONSTRAINT event_projection_scope_user
    FOREIGN KEY (scope_id, user_id) REFERENCES subject_scopes(id, user_id);

CREATE TABLE memory_snapshots (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope_id bigint NOT NULL REFERENCES subject_scopes(id),
    memory_version bigint NOT NULL CHECK (memory_version > 0),
    included_revision bigint NOT NULL CHECK (included_revision >= 0),
    memory_md text NOT NULL CHECK (length(btrim(memory_md)) > 0),
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    derived_context jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(derived_context) = 'object'),
    generated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    published_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (scope_id, memory_version)
);

CREATE TABLE publication_state (
    scope_id bigint PRIMARY KEY REFERENCES subject_scopes(id),
    accepted_revision bigint NOT NULL DEFAULT 0 CHECK (accepted_revision >= 0),
    published_revision bigint NOT NULL DEFAULT 0 CHECK (published_revision >= 0),
    current_memory_version bigint,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (published_revision <= accepted_revision),
    FOREIGN KEY (scope_id, current_memory_version) REFERENCES memory_snapshots(scope_id, memory_version)
);

CREATE INDEX event_projections_thread_idx ON event_projections(thread_id, scope_revision);
CREATE INDEX raw_events_time_idx ON raw_events(tenant_id, namespace, occurred_at);
CREATE INDEX memory_snapshots_latest_idx ON memory_snapshots(scope_id, memory_version DESC);
