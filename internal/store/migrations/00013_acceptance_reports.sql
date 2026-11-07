-- Acceptance reports (docs/06-install-pipeline.md §5, docs/09-roadmap.md
-- 下一阶段 12): the post-install validation record — the freshly installed
-- system's collected facts (in-band ===SYS===/layout/NICs) held against the
-- declared intent (hostname pattern, static addresses), persisted per
-- install task with the machine's latest kept for the read face. Evidence,
-- never a gate: a mismatched check degrades the verdict, the task outcome
-- stays verify_ready's business. History is pruned to the same rolling
-- window discipline as layout_snapshots.

-- +goose Up
CREATE TABLE acceptance_reports (
    id          text PRIMARY KEY,
    machine_id  text NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    task_id     text NOT NULL,
    verdict     text NOT NULL CHECK (verdict IN ('pass', 'degraded', 'fail')),
    content     jsonb NOT NULL,
    captured_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_acceptance_machine ON acceptance_reports (machine_id, captured_at);

-- +goose Down
DROP TABLE acceptance_reports;
