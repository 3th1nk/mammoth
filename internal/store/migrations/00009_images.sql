-- Image artifact library (docs/09-roadmap.md 下一阶段 6): registered
-- distro ISOs — downloaded, checksum-gated, content-addressed on disk.
-- spec.image.id references a ready row; the resolved path + sha256 are
-- rewritten into the spec snapshot at submit, so running jobs outlive
-- deletes.

-- +goose Up
CREATE TABLE images (
    id          text PRIMARY KEY,
    name        text NOT NULL DEFAULT '',
    source_url  text NOT NULL,
    sha256      text NOT NULL,
    distro      text NOT NULL DEFAULT '',
    version     text NOT NULL DEFAULT '',
    size_bytes  bigint NOT NULL DEFAULT 0,
    state       text NOT NULL DEFAULT 'fetching',
    error       text NOT NULL DEFAULT '',
    file_path   text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE images;
