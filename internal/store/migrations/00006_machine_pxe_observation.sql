-- PXE responder observations per machine (docs/08-data-model.md): the
-- firmware architecture the PXE client announced (DHCP option 93, RFC 4578)
-- and when it was last seen. Observation columns only — never identity keys:
-- the machine is still anchored by bmc_address, the NIC MAC lives in the
-- hardware view. Feeds the boot-strategy gate (docs/09-roadmap.md, next
-- phase); arm64 sightings accumulate ahead of the arm64 boot chain.

-- +goose Up
ALTER TABLE machines ADD COLUMN pxe_firmware text;
ALTER TABLE machines ADD COLUMN pxe_last_seen_at timestamptz;

-- +goose Down
ALTER TABLE machines DROP COLUMN pxe_last_seen_at;
ALTER TABLE machines DROP COLUMN pxe_firmware;
