-- A scheduled task run records WHICH container it executed in.
--
-- A task runs in exactly one container: the lowest running idx of the routable type
-- (instances.ResolveOne). Which replica that is moves between runs after a restart, a
-- scale-down or a roll, and nothing recorded where a run landed -- with one replica that
-- never mattered; with several, "which container holds the file this task wrote" had no
-- answer.
--
-- ⛔ Plain columns, NOT a foreign key to application_instances. A run is history (up to
-- keep_runs = 1000 of them) while an instance row is deleted on scale-down and has its
-- container_name rewritten on every roll, so an FK with ON DELETE SET NULL would quietly
-- erase the very answer this column exists to give, and an id alone would dangle. The
-- name the container had WHEN the run executed, plus its stable {process_type}-{idx}
-- label, are facts about that moment and stay true after the row is gone.
--
-- ⛔ Nullable, and no backfill. Rows written before this migration genuinely do not know
-- where they ran, and inventing a value would be a lie about history. NULL also stays for
-- a run that failed before an instance was resolved. Purely additive: nothing is dropped
-- or rewritten, so this is safe for update.sh against a live install.
ALTER TABLE application_scheduled_task_runs
    ADD COLUMN container_name     TEXT,
    ADD COLUMN process_type_name  TEXT,
    ADD COLUMN instance_idx       INTEGER;
