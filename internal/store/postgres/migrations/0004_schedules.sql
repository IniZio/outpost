CREATE TABLE schedules (
    id      bigserial   PRIMARY KEY,
    owner   text        NOT NULL,
    name    text        NOT NULL,
    kind    smallint    NOT NULL,
    cron    text        NOT NULL,
    tz      text        NOT NULL,
    spec    jsonb       NOT NULL,
    enabled boolean     NOT NULL,
    since   timestamptz NOT NULL,
    UNIQUE (owner, name)
);

-- One row per slot that fired (task_id set) or was skipped (task_id 0). The key is what makes a
-- slot fire at most once.
CREATE TABLE schedule_fires (
    schedule_id bigint      NOT NULL REFERENCES schedules ON DELETE CASCADE,
    due         timestamptz NOT NULL,
    task_id     bigint      NOT NULL,
    at          timestamptz NOT NULL,
    PRIMARY KEY (schedule_id, due)
);
