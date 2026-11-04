-- Headless machines (docs/13-headless-design.md): BMC-less x86_64 hosts on
-- the protocol=none path. Identity is the NIC MAC carried in a synthetic
-- bmc_address ("none://<mac>", same shape as fake://) so the existing
-- UNIQUE constraint keeps registrations one-per-machine with no schema
-- change to the address column. The credential reference becomes nullable —
-- a headless machine has no controller and therefore no BMC credential;
-- every other protocol still requires one (validated in the API layer).
-- The protocol CHECK gains the new enum value.

-- +goose Up
ALTER TABLE machines DROP CONSTRAINT machines_bmc_protocol_check;
ALTER TABLE machines ADD CONSTRAINT machines_bmc_protocol_check
    CHECK (bmc_protocol IN ('redfish', 'ipmi', 'auto', 'fake', 'none'));
ALTER TABLE machines ALTER COLUMN bmc_credential_id DROP NOT NULL;

-- +goose Down
UPDATE machines SET bmc_protocol = 'auto' WHERE bmc_protocol = 'none';
ALTER TABLE machines ALTER COLUMN bmc_credential_id SET NOT NULL;
ALTER TABLE machines DROP CONSTRAINT machines_bmc_protocol_check;
ALTER TABLE machines ADD CONSTRAINT machines_bmc_protocol_check
    CHECK (bmc_protocol IN ('redfish', 'ipmi', 'auto', 'fake'));
