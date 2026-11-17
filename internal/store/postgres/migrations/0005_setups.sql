-- One row per setup revision of a repo. The build columns describe its latest build.
CREATE TABLE repo_setups (
    repo       text        NOT NULL,
    rev        integer     NOT NULL,
    files      jsonb       NOT NULL,
    status     text        NOT NULL,
    machine    text        NOT NULL DEFAULT '',
    image      text        NOT NULL DEFAULT '',
    digest     text        NOT NULL DEFAULT '',
    inner_key  text        NOT NULL DEFAULT '',
    commit_sha text        NOT NULL DEFAULT '',
    log        text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    created_by text        NOT NULL,
    built_at   timestamptz,
    PRIMARY KEY (repo, rev)
);

CREATE INDEX repo_setups_building ON repo_setups (status) WHERE status = 'building';

-- The repo's current ready revision, 0 for none.
ALTER TABLE repos ADD COLUMN setup_rev integer NOT NULL DEFAULT 0;
