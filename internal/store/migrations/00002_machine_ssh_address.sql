-- In-band (SSH) address for the inband_ssh probe (docs/05-inventory.md §3).
-- The BMC address reaches the management plane; the probe needs the machine's
-- own address. Nullable: machines without it are spec-level only.

-- +goose Up
ALTER TABLE machines ADD COLUMN ssh_address text;

-- +goose Down
ALTER TABLE machines DROP COLUMN ssh_address;
