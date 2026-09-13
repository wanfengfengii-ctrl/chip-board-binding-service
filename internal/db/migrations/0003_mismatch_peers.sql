-- Mismatch-peer lookup: the supervisor endpoint aggregates the immutable
-- mismatch inspections that implicated a given binding on either side. These
-- partial indexes match only MISMATCH rows and lead with the stored binding
-- id of one side, so the aggregation scans just the inspections that touched
-- the target binding instead of the whole ledger.
CREATE INDEX IF NOT EXISTS inspections_mismatch_chip_binding_idx
    ON inspections (chip_binding_id, id)
    WHERE result = 'MISMATCH';

CREATE INDEX IF NOT EXISTS inspections_mismatch_board_binding_idx
    ON inspections (board_binding_id, id)
    WHERE result = 'MISMATCH';
