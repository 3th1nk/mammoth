package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ImageRepo stores the registered image artifacts (docs/09-roadmap.md
// 下一阶段 6): a distro ISO's source URL, its mandatory sha256 gate, and the
// local cache state. The download itself lives in internal/images (the
// fetch worker); this repo is the row truth the API and the fetcher share.
type ImageRepo struct{ db *sql.DB }

func NewImageRepo(db *sql.DB) *ImageRepo { return &ImageRepo{db: db} }

// Image is one registered artifact. State runs fetching → ready | failed;
// FilePath points at the content-addressed cache file once ready
// (MediaDir/images/<sha256>.iso — several rows may share one file when the
// same bytes were registered twice).
//
// Format/RawSHA256/RawSizeBytes describe the dd pathway's compression facts
// (docs/09「镜像直写通路」): SHA256/SizeBytes gate and span the REGISTERED
// bytes (the compressed file), while the runtime verifies the UNCOMPRESSED
// stream — RawSHA256/RawSizeBytes carry that face when the cache file is
// gzip/xz (Format raw.gz/raw.xz). Format ” means undetected (pre-detection
// rows); treat it as raw at resolution.
type Image struct {
	ID           string
	Name         string
	SourceURL    string
	SHA256       string
	Distro       string
	Version      string
	Format       string // '' | raw | raw.gz | raw.xz (magic-byte sniff of the cache file)
	RawSHA256    string // uncompressed-image digest; = SHA256 when Format is raw/''
	RawSizeBytes int64  // uncompressed-image byte span
	SizeBytes    int64
	State        string // fetching | ready | failed
	Error        string
	FilePath     string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (r *ImageRepo) Create(ctx context.Context, im *Image) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO images (id, name, source_url, sha256, distro, version, format, raw_sha256, raw_size_bytes, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		im.ID, im.Name, im.SourceURL, im.SHA256, im.Distro, im.Version, im.Format, im.RawSHA256, im.RawSizeBytes, im.State)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

const imageSelect = `
	SELECT id, name, source_url, sha256, distro, version, format, raw_sha256, raw_size_bytes, size_bytes, state, error, file_path, created_at, updated_at
	FROM images`

func scanImage(row interface{ Scan(...any) error }) (*Image, error) {
	var im Image
	err := row.Scan(&im.ID, &im.Name, &im.SourceURL, &im.SHA256, &im.Distro, &im.Version,
		&im.Format, &im.RawSHA256, &im.RawSizeBytes, &im.SizeBytes, &im.State, &im.Error, &im.FilePath, &im.CreatedAt, &im.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &im, nil
}

func (r *ImageRepo) Get(ctx context.Context, id string) (*Image, error) {
	return scanImage(r.db.QueryRowContext(ctx, imageSelect+` WHERE id = $1`, id))
}

// List returns every registration, newest first.
func (r *ImageRepo) List(ctx context.Context) ([]Image, error) {
	rows, err := r.db.QueryContext(ctx, imageSelect+` ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		im, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *im)
	}
	return out, rows.Err()
}

// ReadyBySHA returns a ready registration carrying exactly this digest, if
// any — a re-registration of bytes the library already holds skips the
// download and shares the cached file.
func (r *ImageRepo) ReadyBySHA(ctx context.Context, sha string) (*Image, error) {
	return scanImage(r.db.QueryRowContext(ctx, imageSelect+` WHERE sha256 = $1 AND state = 'ready' ORDER BY created_at LIMIT 1`, sha))
}

// SetReady marks a fetch complete: size and cache path land with the state.
func (r *ImageRepo) SetReady(ctx context.Context, id string, sizeBytes int64, filePath string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE images SET state = 'ready', size_bytes = $2, file_path = $3, error = '', updated_at = now()
		WHERE id = $1`, id, sizeBytes, filePath)
	return err
}

// SetDetected backfills the distro/version the fetch worker identified from
// the ISO (docs/04-install-spec.md §4.1 auto-detection). Only empty fields
// move — the registrant's own values always win.
func (r *ImageRepo) SetDetected(ctx context.Context, id, distro, version string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE images SET
		  distro = COALESCE(NULLIF(distro, ''), NULLIF($2, ''), distro),
		  version = COALESCE(NULLIF(version, ''), NULLIF($3, ''), version),
		  updated_at = now()
		WHERE id = $1`, id, distro, version)
	return err
}

// SetDetectedFormat lands the compression facts the fetch worker sniffed and
// hashed out of the cache file (docs/09「镜像直写通路」): format plus the
// uncompressed digest/byte span the dd runtime verifies against. Unlike
// SetDetected these have no registrant face — the bytes are the only truth —
// so they overwrite wholesale (a re-detect after a cache change re-lands).
func (r *ImageRepo) SetDetectedFormat(ctx context.Context, id, format, rawSHA256 string, rawSizeBytes int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE images SET format = $2, raw_sha256 = $3, raw_size_bytes = $4, updated_at = now()
		WHERE id = $1`, id, format, rawSHA256, rawSizeBytes)
	return err
}

// SetMissing marks a ready row whose cache file vanished out of band
// (operator moved or removed it in the media images directory). The
// directory scanner restores the row to ready when the file returns.
func (r *ImageRepo) SetMissing(ctx context.Context, id, cause string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE images SET state = 'missing', error = $2, updated_at = now() WHERE id = $1`, id, cause)
	return err
}

// SetFailed marks a fetch dead with its reason. The row stays: the
// registration (and its sha256 gate) is the operator's record; resubmitting
// re-downloads.
func (r *ImageRepo) SetFailed(ctx context.Context, id string, cause string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE images SET state = 'failed', error = $2, updated_at = now()
		WHERE id = $1`, id, cause)
	return err
}

// Delete removes the registration. Callers decide file reclamation (the
// fetcher drops the cache file when no other row references the digest).
func (r *ImageRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM images WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountBySHA counts registrations pointing at a digest — the reference
// count for cache-file reclamation.
func (r *ImageRepo) CountBySHA(ctx context.Context, sha string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM images WHERE sha256 = $1`, sha).Scan(&n)
	return n, err
}

// Fetching lists rows still claiming a download — the fetch worker's
// startup recovery set (rows orphaned by a process restart).
func (r *ImageRepo) Fetching(ctx context.Context) ([]Image, error) {
	rows, err := r.db.QueryContext(ctx, imageSelect+` WHERE state = 'fetching' ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		im, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *im)
	}
	return out, rows.Err()
}
