-- dd pathway compressed-image facts (docs/09「镜像直写通路」): the library
-- caches the REGISTERED bytes (the compressed file the digest gate saw),
-- while the dd runtime verifies the UNCOMPRESSED stream that hits the disk.
-- The fetch worker sniffs the compression format from magic bytes and
-- stream-decompresses once to record the uncompressed digest + byte span;
-- spec resolution carries these so image.id stays sound for raw.gz/raw.xz.

-- +goose Up
ALTER TABLE images ADD COLUMN format         text NOT NULL DEFAULT '';
ALTER TABLE images ADD COLUMN raw_sha256     text NOT NULL DEFAULT '';
ALTER TABLE images ADD COLUMN raw_size_bytes bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE images DROP COLUMN format;
ALTER TABLE images DROP COLUMN raw_sha256;
ALTER TABLE images DROP COLUMN raw_size_bytes;
