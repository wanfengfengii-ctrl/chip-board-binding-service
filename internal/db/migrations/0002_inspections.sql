-- Physical verification records. A repair technician scans a chip UID and a
-- board serial together before teardown; each scan pair is resolved against
-- the existing bindings and stored once, as an immutable audit record.
-- Rows are inserted only and can never be updated or deleted (enforced by
-- the inspections_block_modification trigger below).
CREATE TABLE IF NOT EXISTS inspections (
    id               BIGSERIAL    NOT NULL,
    chip_uid         TEXT         NOT NULL,
    board_serial     TEXT         NOT NULL,
    result           TEXT         NOT NULL,
    chip_binding_id  BIGINT,
    board_binding_id BIGINT,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT inspections_pkey PRIMARY KEY (id),
    CONSTRAINT inspections_result_check CHECK (result IN ('CONSISTENT', 'MISMATCH', 'PARTIAL', 'UNREGISTERED')),
    CONSTRAINT inspections_chip_binding_fkey  FOREIGN KEY (chip_binding_id)  REFERENCES bindings (id),
    CONSTRAINT inspections_board_binding_fkey FOREIGN KEY (board_binding_id) REFERENCES bindings (id),
    CONSTRAINT inspections_chip_uid_format     CHECK (chip_uid     ~ '^[A-Z0-9-]{1,64}$'),
    CONSTRAINT inspections_board_serial_format CHECK (board_serial ~ '^[A-Z0-9-]{1,64}$')
);

-- Block any modification of a recorded inspection at the database level, so
-- the guarantee holds regardless of which client (or future service version)
-- connects.
CREATE OR REPLACE FUNCTION inspections_block_modification()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'inspections records are immutable';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS inspections_no_update ON inspections;
CREATE TRIGGER inspections_no_update
    BEFORE UPDATE ON inspections
    FOR EACH ROW EXECUTE FUNCTION inspections_block_modification();

DROP TRIGGER IF EXISTS inspections_no_delete ON inspections;
CREATE TRIGGER inspections_no_delete
    BEFORE DELETE ON inspections
    FOR EACH ROW EXECUTE FUNCTION inspections_block_modification();
