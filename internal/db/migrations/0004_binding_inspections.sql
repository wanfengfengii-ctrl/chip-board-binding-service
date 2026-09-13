-- Per-binding inspection history: a repair supervisor reviews which physical
-- verifications ever hit a given binding on the chip side, the board side or
-- both. The history query filters on the stored binding id of either side and
-- pages backwards by inspection id, so each index leads with one side's
-- binding id and trails the inspection id the keyset walks. Every verdict is
-- relevant history, so unlike the mismatch-peers indexes these are not
-- partial on the result.
CREATE INDEX IF NOT EXISTS inspections_chip_binding_history_idx
    ON inspections (chip_binding_id, id);

CREATE INDEX IF NOT EXISTS inspections_board_binding_history_idx
    ON inspections (board_binding_id, id);
