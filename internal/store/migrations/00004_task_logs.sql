-- Task execution logs (docs/08-data-model.md §2, M6): the persisted half of
-- the log dual-write (docs/02-architecture.md §5.2) — structured lines that
-- carry task_id are stored at write time so GET /jobs/{id}/tasks/{taskId}/logs
-- can replay what the runner did and observed. Single table + (task_id, id)
-- index; retention is the reaper TTL (MAMMOTH_TASK_LOGS_TTL, 90d default),
-- monthly partitioning is an evolution path if volume ever demands it.

-- +goose Up
CREATE TABLE task_logs (
    id      bigserial PRIMARY KEY,
    task_id text NOT NULL,
    ts      timestamptz NOT NULL DEFAULT now(),
    level   text NOT NULL,
    stage   text,
    message text NOT NULL,
    attrs   jsonb
);
CREATE INDEX idx_task_logs_task ON task_logs (task_id, id);

-- +goose Down
DROP TABLE task_logs;
