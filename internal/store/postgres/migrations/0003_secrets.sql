CREATE TABLE secrets (
    owner      text        NOT NULL,
    name       text        NOT NULL,
    sealed     bytea       NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (owner, name)
);
