CREATE TABLE sessions (
    hash       bytea PRIMARY KEY,
    login      text        NOT NULL,
    kind       smallint    NOT NULL,
    name       text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    spent_at   timestamptz
);

CREATE INDEX sessions_expires ON sessions (expires_at);
