CREATE SEQUENCE task_ids;

CREATE TABLE tasks (
    id         bigint PRIMARY KEY,
    version    bigint      NOT NULL,
    owner      text        NOT NULL,
    kind       smallint    NOT NULL,
    machine    text        NOT NULL,
    created_at timestamptz NOT NULL,
    next_ask   bigint      NOT NULL,
    agent_run  bigint      NOT NULL,
    box_gen    bigint      NOT NULL,
    spec       jsonb       NOT NULL,
    phase      text        NOT NULL,
    status     smallint    NOT NULL,
    wake       timestamptz,
    unfinished boolean     NOT NULL,
    entries    integer     NOT NULL DEFAULT 0,
    payload    jsonb       NOT NULL
);

CREATE INDEX tasks_wake ON tasks (wake) WHERE wake IS NOT NULL;
CREATE INDEX tasks_unfinished ON tasks (id) WHERE unfinished;
CREATE INDEX tasks_owner ON tasks (owner, id DESC);
CREATE INDEX tasks_status ON tasks (status, id DESC);

CREATE TABLE timeline (
    task_id bigint      NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    seq     integer     NOT NULL,
    at      timestamptz NOT NULL,
    kind    text        NOT NULL,
    text    text        NOT NULL,
    PRIMARY KEY (task_id, seq)
);

CREATE TABLE outbox (
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id bigint   NOT NULL,
    kind    smallint NOT NULL,
    machine text     NOT NULL,
    run     bigint   NOT NULL,
    gen     bigint   NOT NULL,
    text    text     NOT NULL,
    done    boolean  NOT NULL DEFAULT false
);

CREATE INDEX outbox_pending ON outbox (id) WHERE NOT done;

CREATE TABLE repos (
    name        text PRIMARY KEY,
    ready_image text        NOT NULL DEFAULT '',
    ready_on    text        NOT NULL DEFAULT '',
    ready_at    timestamptz,
    notes       text        NOT NULL DEFAULT '',
    setup_task  bigint      NOT NULL DEFAULT 0
);

CREATE TABLE machines (
    id        text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    name      text     NOT NULL,
    owner     text     NOT NULL,
    role      smallint NOT NULL,
    slots     integer  NOT NULL,
    exec      boolean  NOT NULL,
    key       bytea    NOT NULL,
    link_gen  bigint   NOT NULL DEFAULT 0,
    online    boolean  NOT NULL DEFAULT false,
    linked_at timestamptz,
    build     text     NOT NULL DEFAULT '',
    UNIQUE (owner, name)
);

CREATE TABLE enrollments (
    user_code   text PRIMARY KEY,
    device_hash bytea       NOT NULL UNIQUE,
    req         jsonb       NOT NULL,
    state       smallint    NOT NULL,
    owner       text        NOT NULL DEFAULT '',
    machine     text        NOT NULL DEFAULT '',
    expires     timestamptz NOT NULL
);
