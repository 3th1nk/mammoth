-- +goose Up
-- Machine firmware inventory (docs/07-bmc.md §6): the controller-reported
-- firmware components, collected with the spec view by the discover flow
-- (pure-read capability; empty until a protocol that reports it succeeds).
ALTER TABLE machines ADD COLUMN firmware JSONB;

-- +goose Down
ALTER TABLE machines DROP COLUMN firmware;
