-- Rescue mode (docs/07-bmc.md §6.5, docs/09-roadmap.md 下一阶段 11): the
-- boot_rescue action runs as its own job type / flow — it boots the machine
-- into the engine's in-memory rescue system and waits for the agent's
-- rescue-ready report, so it is a state-machine flow (like install), not a
-- single BMC call (like power). The jobs.type CHECK gains the new enum
-- value; install/rescue mutual exclusion is query-side (flow_name IN), no
-- schema change beyond this constraint.

-- +goose Up
ALTER TABLE jobs DROP CONSTRAINT jobs_type_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_type_check
    CHECK (type IN ('install', 'power', 'discover', 'rescue'));

-- +goose Down
ALTER TABLE jobs DROP CONSTRAINT jobs_type_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_type_check
    CHECK (type IN ('install', 'power', 'discover'));
