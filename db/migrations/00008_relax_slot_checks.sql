-- +goose Up
-- 00007's CHECK constraints conflicted with the FKs' ON DELETE SET NULL:
-- cascading a tournament delete nulls source_match_id / source_group_id on
-- consumer slots BEFORE those slot rows are themselves cascaded away, which
-- trips the CHECKs mid-delete (caught by the Phase 3.3 integration suite's
-- fixture wipe). A nulled ref is legitimate — it means the slot's source was
-- deleted and the slot is permanently unresolved; every reader already treats
-- it that way (resolvers select BY ref, so nulled rows simply never match).
--
-- The invariants move to the writers (insertSlot / persist pass 3 /
-- AddManualMatch always populate refs alongside the type), in line with the
-- service-authoritative, no-trigger approach.
ALTER TABLE match_participants
    DROP CONSTRAINT IF EXISTS mp_group_placement_ref,
    DROP CONSTRAINT IF EXISTS mp_match_ref;

-- +goose Down
ALTER TABLE match_participants
    ADD CONSTRAINT mp_group_placement_ref CHECK
        (source_type <> 'group_placement'
         OR (source_group_id IS NOT NULL AND source_rank >= 1)),
    ADD CONSTRAINT mp_match_ref CHECK
        (source_type NOT IN ('match_winner', 'match_loser')
         OR source_match_id IS NOT NULL);
