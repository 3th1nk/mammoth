-- BIOS baseline profiles (docs/07-bmc.md §6.1, docs/09-roadmap.md 下一阶段
-- 13): named declarative BIOS attribute sets — the fleet-level baseline a
-- machine is assigned, and the drift-inspection reference. Profiles are
-- pure data (names are identifiers, attributes keep the vendor's JSON
-- types); assignment rides machines.bios_profile_id (SET NULL on delete —
-- a dead profile leaves the machine unassigned, never dangling). Drift
-- itself is computed live at read time (BiosView vs profile attributes) and
-- recorded as events — no stale verdict column.

-- +goose Up
CREATE TABLE bios_profiles (
    id          text PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    attributes  jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE machines ADD COLUMN bios_profile_id text REFERENCES bios_profiles (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE machines DROP COLUMN bios_profile_id;
DROP TABLE bios_profiles;
