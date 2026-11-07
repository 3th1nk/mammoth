-- Decommission (docs/07-bmc.md §6.6, docs/09-roadmap.md 下一阶段 18): the
-- machine lifecycle's terminal flow — NIST 800-88 erase (all drives), the
-- evidence capture (firmware inventory / SEL tail / BIOS snapshot / machine
-- identity), then the retired terminal state. The jobs.type CHECK gains the
-- new flow; machines.state gains 'retired' (reinstatement is a fresh
-- discover — the probe's own state machine walks it back to ready, no new
-- endpoint). The evidence reports persist per task with the machine's
-- rolling window (acceptance_reports' discipline).

-- +goose Up
ALTER TABLE jobs DROP CONSTRAINT jobs_type_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_type_check
    CHECK (type IN ('install', 'power', 'discover', 'rescue', 'decommission'));

ALTER TABLE machines DROP CONSTRAINT machines_state_check;
ALTER TABLE machines ADD CONSTRAINT machines_state_check
    CHECK (state IN ('registering', 'discovering', 'ready', 'error', 'retired'));

CREATE TABLE decommission_reports (
    id          text PRIMARY KEY,
    machine_id  text NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    task_id     text NOT NULL,
    content     jsonb NOT NULL,
    captured_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_decommission_machine ON decommission_reports (machine_id, captured_at);

-- +goose Down
DROP TABLE decommission_reports;
ALTER TABLE machines DROP CONSTRAINT machines_state_check;
ALTER TABLE machines ADD CONSTRAINT machines_state_check
    CHECK (state IN ('registering', 'discovering', 'ready', 'error'));
ALTER TABLE jobs DROP CONSTRAINT jobs_type_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_type_check
    CHECK (type IN ('install', 'power', 'discover', 'rescue'));
