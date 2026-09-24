-- Deregister semantics: deleting a machine removes its machine-scoped
-- records with it (layout snapshots and task rows), while job shells stay
-- for audit — the FK was a bare RESTRICT, so any machine with a single
-- auto-discovery task (every registration runs one) was undeletable with a
-- raw SQLSTATE 23503 surfacing as an unmapped 500.
-- +goose Up
ALTER TABLE layout_snapshots DROP CONSTRAINT layout_snapshots_machine_id_fkey;
ALTER TABLE layout_snapshots ADD CONSTRAINT layout_snapshots_machine_id_fkey
    FOREIGN KEY (machine_id) REFERENCES machines (id) ON DELETE CASCADE;

ALTER TABLE tasks DROP CONSTRAINT tasks_machine_id_fkey;
ALTER TABLE tasks ADD CONSTRAINT tasks_machine_id_fkey
    FOREIGN KEY (machine_id) REFERENCES machines (id) ON DELETE CASCADE;

ALTER TABLE task_stages DROP CONSTRAINT task_stages_task_id_fkey;
ALTER TABLE task_stages ADD CONSTRAINT task_stages_task_id_fkey
    FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE layout_snapshots DROP CONSTRAINT layout_snapshots_machine_id_fkey;
ALTER TABLE layout_snapshots ADD CONSTRAINT layout_snapshots_machine_id_fkey
    FOREIGN KEY (machine_id) REFERENCES machines (id);

ALTER TABLE tasks DROP CONSTRAINT tasks_machine_id_fkey;
ALTER TABLE tasks ADD CONSTRAINT tasks_machine_id_fkey
    FOREIGN KEY (machine_id) REFERENCES machines (id);

ALTER TABLE task_stages DROP CONSTRAINT task_stages_task_id_fkey;
ALTER TABLE task_stages ADD CONSTRAINT task_stages_task_id_fkey
    FOREIGN KEY (task_id) REFERENCES tasks (id);
