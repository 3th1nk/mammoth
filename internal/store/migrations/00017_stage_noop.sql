-- configure_raid no-op marker (docs/06-install-pipeline.md §2): the six-stage
-- pipeline is a fixed contract, but a spec without hardware RAID declarations
-- turns the stage into an instant pass-through. The persisted noop flag lets
-- the console render "skipped" instead of a misleading millisecond duration.

-- +goose Up
ALTER TABLE task_stages ADD COLUMN noop boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE task_stages DROP COLUMN noop;
