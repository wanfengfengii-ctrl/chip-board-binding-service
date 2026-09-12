-- Request ledger: one row per successfully processed idempotency key.
-- Rows are inserted once and never updated or deleted.
CREATE TABLE IF NOT EXISTS requests (
    request_key  TEXT        NOT NULL,
    chip_uid     TEXT        NOT NULL,
    board_serial TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT requests_pkey PRIMARY KEY (request_key),
    CONSTRAINT requests_request_key_format  CHECK (request_key  ~ '^[A-Z0-9-]{1,64}$'),
    CONSTRAINT requests_chip_uid_format     CHECK (chip_uid     ~ '^[A-Z0-9-]{1,64}$'),
    CONSTRAINT requests_board_serial_format CHECK (board_serial ~ '^[A-Z0-9-]{1,64}$')
);

-- One-to-one binding between a chip UID and a board serial number.
-- The unique constraints are the concurrency mechanism: exactly one of any
-- competing transactions can commit a row for a given chip or board, no
-- matter how many service replicas or stations race.
CREATE TABLE IF NOT EXISTS bindings (
    id           BIGSERIAL   NOT NULL,
    request_key  TEXT        NOT NULL,
    chip_uid     TEXT        NOT NULL,
    board_serial TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT bindings_pkey PRIMARY KEY (id),
    CONSTRAINT bindings_request_key_unique UNIQUE (request_key),
    CONSTRAINT bindings_request_fkey FOREIGN KEY (request_key) REFERENCES requests (request_key),
    CONSTRAINT bindings_chip_uid_unique UNIQUE (chip_uid),
    CONSTRAINT bindings_board_serial_unique UNIQUE (board_serial),
    CONSTRAINT bindings_request_key_format  CHECK (request_key  ~ '^[A-Z0-9-]{1,64}$'),
    CONSTRAINT bindings_chip_uid_format     CHECK (chip_uid     ~ '^[A-Z0-9-]{1,64}$'),
    CONSTRAINT bindings_board_serial_format CHECK (board_serial ~ '^[A-Z0-9-]{1,64}$')
);
