CREATE TABLE roles (
    id          SMALLINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE permissions (
    id            SMALLINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    description   TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (action, resource_type)
);

CREATE TABLE role_permissions (
    role_id       SMALLINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_id SMALLINT NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    granted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE user_roles (
    user_id       UUID     NOT NULL,
    role_id       SMALLINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    resource_type TEXT     NOT NULL,
    resource_id   TEXT     NOT NULL,
    scope         JSONB    NOT NULL DEFAULT '{}',
    granted_by    UUID     NOT NULL,
    granted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ,
    PRIMARY KEY (user_id, role_id, resource_type, resource_id)
);

-- GIN index for scope @> containment queries (GET /query/v1/resources with scope_filter).
-- jsonb_path_ops: compact, fast for @>, does not support ? operators (not needed).
CREATE INDEX idx_user_roles_scope
    ON user_roles USING GIN (scope jsonb_path_ops);

CREATE TABLE user_permission_overrides (
    user_id       UUID     NOT NULL,
    permission_id SMALLINT NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    resource_id   TEXT     NOT NULL,
    scope         JSONB    NOT NULL DEFAULT '{}',
    effect        TEXT     NOT NULL CHECK (effect IN ('ALLOW', 'DENY')),
    reason        TEXT,
    set_by        UUID     NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ,
    PRIMARY KEY (user_id, permission_id, resource_id)
);

CREATE INDEX idx_user_permission_overrides_scope
    ON user_permission_overrides USING GIN (scope jsonb_path_ops);

CREATE TABLE event_log (
    event_id     UUID PRIMARY KEY,
    event_type   TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
